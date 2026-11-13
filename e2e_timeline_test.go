package goshl

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

const (
	timelineTick       = 1.0 / 90000
	timelineVideoFrame = 1001.0 / 24000
)

type timelineStorage struct {
	*stubStorage
	lock      sync.Mutex
	metadata  map[string][]byte
	segments  map[domain.SegmentData][]byte
	subtitles map[string][]byte
}

func newTimelineStorage() *timelineStorage {
	return &timelineStorage{
		stubStorage: &stubStorage{},
		metadata:    make(map[string][]byte),
		segments:    make(map[domain.SegmentData][]byte),
		subtitles:   make(map[string][]byte),
	}
}

func (s *timelineStorage) MetadataExists(ctx context.Context, mediaKey string) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	_, ok := s.metadata[mediaKey]
	return ok, nil
}

func (s *timelineStorage) GetMetadata(ctx context.Context, mediaKey string) ([]byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.metadata[mediaKey], nil
}

func (s *timelineStorage) SetMetadata(ctx context.Context, mediaKey string, data []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.metadata[mediaKey] = data
	return nil
}

func (s *timelineStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.segments[info] = append([]byte(nil), data...)
	return nil
}

func (s *timelineStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	data, ok := s.segments[info]
	if !ok {
		return nil, fmt.Errorf("segment %d of %s is not stored", info.Index, info.Rendition)
	}
	return data, nil
}

func (s *timelineStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	_, ok := s.segments[info]
	return ok, nil
}

func (s *timelineStorage) WriteSubtitleVTT(ctx context.Context, assetKey string, trackID string, data []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.subtitles[assetKey+"|"+trackID] = append([]byte(nil), data...)
	return nil
}

func (s *timelineStorage) ReadSubtitleVTT(ctx context.Context, assetKey string, trackID string) ([]byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.subtitles[assetKey+"|"+trackID], nil
}

func (s *timelineStorage) SubtitleVTTExists(ctx context.Context, assetKey string, trackID string) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	_, ok := s.subtitles[assetKey+"|"+trackID]
	return ok, nil
}

type timelineObserver struct {
	mu       sync.Mutex
	failures []string
}

func (o *timelineObserver) Observe(ctx context.Context, event domain.Event) {
	if event.Type != domain.EventJobFailed {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, fmt.Sprintf("%s %s %d-%d: %s", event.StreamType, event.Rendition, event.StartIndex, event.EndIndex, event.Error))
}

func (o *timelineObserver) failed() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.failures...)
}

type timelineSegment struct {
	pts  []float64
	keys []float64
}

type timelineSource struct {
	path      string
	start     float64
	video     []testmedia.Packet
	segments  []domain.Segment
	keyframes []float64
}

func timelineFixture(t *testing.T, container string) timelineSource {
	t.Helper()
	path := testmedia.H264(t, testmedia.Spec{
		Container:      container,
		Width:          480,
		Height:         400,
		FPS:            "24000/1001",
		BFrames:        3,
		Pyramid:        true,
		KeyframeFrames: []int{0, 25, 50, 101, 125, 149, 200, 251, 299, 350, 401, 449, 500, 551},
		Duration:       24,
		Audio:          []testmedia.AudioSpec{{Codec: "aac"}, {Codec: "ac3"}},
		Subtitles:      true,
	})
	source := timelineSource{path: path, start: testmedia.FormatStart(t, path), video: testmedia.Packets(t, path, "v:0")}
	for _, packet := range source.video {
		if packet.Key {
			source.keyframes = append(source.keyframes, packet.PTS-source.start)
		}
	}
	sort.Float64s(source.keyframes)
	return source
}

func (s timelineSource) firstVideoPTS() float64 {
	first := math.Inf(1)
	for _, packet := range s.video {
		first = math.Min(first, packet.PTS-s.start)
	}
	return first
}

func (s timelineSource) expectedStart(index int) float64 {
	if index == 0 {
		return s.firstVideoPTS()
	}
	return s.segments[index].Start
}

func readTimelineSegment(t *testing.T, dir, name string, index int, selector string, data []byte) timelineSegment {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%s-%05d.ts", name, index))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s segment %d: %v", name, index, err)
	}
	var segment timelineSegment
	for _, packet := range testmedia.Packets(t, path, selector) {
		segment.pts = append(segment.pts, packet.PTS)
		if packet.Key {
			segment.keys = append(segment.keys, packet.PTS)
		}
	}
	sort.Float64s(segment.pts)
	return segment
}

func requestOrder(count int) []int {
	order := []int{3, 0, 1, 2}
	for index := 4; index < count; index++ {
		order = append(order, index)
	}
	return order
}

func fetchTimelineRendition(t *testing.T, controller *Controller, source timelineSource, stream StreamType, name, selector string) []timelineSegment {
	t.Helper()
	dir := t.TempDir()
	fetched := make([]timelineSegment, len(source.segments))
	for _, index := range requestOrder(len(source.segments)) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		data, err := controller.Segment(ctx, source.path, stream, name, index)
		cancel()
		if err != nil {
			t.Errorf("%s segment %d: %v", name, index, err)
			continue
		}
		fetched[index] = readTimelineSegment(t, dir, name, index, selector, data)
		if len(fetched[index].pts) == 0 {
			t.Errorf("%s segment %d has no packets", name, index)
		}
	}
	return fetched
}

func assertContinuousJoins(t *testing.T, name string, segments []timelineSegment, frame, tolerance float64) {
	t.Helper()
	for index := 1; index < len(segments); index++ {
		previous, current := segments[index-1], segments[index]
		if len(previous.pts) == 0 || len(current.pts) == 0 {
			continue
		}
		last := previous.pts[len(previous.pts)-1]
		first := current.pts[0]
		if first <= last {
			t.Errorf("%s: segment %d starts at %.6f, not after segment %d which ends at %.6f", name, index, first, index-1, last)
		}
		seen := make(map[int64]bool, len(previous.pts))
		for _, pts := range previous.pts {
			seen[int64(math.Round(pts*90000))] = true
		}
		for _, pts := range current.pts {
			if seen[int64(math.Round(pts*90000))] {
				t.Errorf("%s: segments %d and %d both contain PTS %.6f", name, index-1, index, pts)
			}
		}
		if gap := first - last; math.Abs(gap-frame) > tolerance {
			t.Errorf("%s: join %d/%d advances by %.6f, want one frame %.6f", name, index-1, index, gap, frame)
		}
	}
}

func assertVideoTimeline(t *testing.T, name string, source timelineSource, segments []timelineSegment, transcoded bool, tolerance, joinTolerance float64) {
	t.Helper()
	offset := domain.OutputTimelineOffset.Seconds()
	assertContinuousJoins(t, name, segments, timelineVideoFrame, joinTolerance)
	for index, segment := range segments {
		if len(segment.pts) == 0 {
			continue
		}
		want := source.expectedStart(index) + offset
		if diff := segment.pts[0] - want; math.Abs(diff) > tolerance {
			t.Errorf("%s: segment %d starts at %.6f, source packet is at %.6f (off by %+.6f)", name, index, segment.pts[0], want, diff)
		}
		if !transcoded {
			continue
		}
		if len(segment.keys) != 1 {
			t.Errorf("%s: segment %d has %d keyframes %v, want exactly one", name, index, len(segment.keys), segment.keys)
			continue
		}
		if diff := segment.keys[0] - want; math.Abs(diff) > tolerance {
			t.Errorf("%s: segment %d keyframe at %.6f, source keyframe is at %.6f (off by %+.6f)", name, index, segment.keys[0], want, diff)
		}
	}
}

func assertAudioTimeline(t *testing.T, name string, source timelineSource, stream int, segments []timelineSegment, frame float64) {
	t.Helper()
	offset := domain.OutputTimelineOffset.Seconds()
	assertContinuousJoins(t, name, segments, frame, 2*timelineTick)
	packets := testmedia.Packets(t, source.path, strconv.Itoa(stream))
	if len(packets) == 0 {
		t.Fatalf("%s: source stream %d has no packets", name, stream)
	}
	for index, segment := range segments {
		if len(segment.pts) == 0 {
			continue
		}
		want := source.segments[index].Start + offset
		if index == 0 {
			want = packets[0].PTS - source.start + offset
		}
		if diff := segment.pts[0] - want; math.Abs(diff) > frame+2*timelineTick {
			t.Errorf("%s: segment %d starts at %.6f, source position is at %.6f (off by %+.6f)", name, index, segment.pts[0], want, diff)
		}
	}
}

func firstCueStart(vtt string) (float64, bool) {
	for _, line := range strings.Split(vtt, "\n") {
		start, _, found := strings.Cut(line, " --> ")
		if !found {
			continue
		}
		var seconds float64
		for _, part := range strings.Split(strings.TrimSpace(start), ":") {
			value, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return 0, false
			}
			seconds = seconds*60 + value
		}
		return seconds, true
	}
	return 0, false
}

func TestControllerSegmentsFormOneContinuousTimeline(t *testing.T) {
	testmedia.Require(t)
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			source := timelineFixture(t, container)
			storage := newTimelineStorage()
			observer := &timelineObserver{}
			controller := NewController(Options{
				Storage:        storage,
				Coordinator:    newClusterCoordinator(),
				PathGen:        stubPathGen{},
				TargetDuration: 2,
				SegmentsPerJob: 2,
				SegmentTimeout: 2 * time.Minute,
				JobTimeout:     2 * time.Minute,
				Observer:       observer,
			})
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if err := controller.Start(ctx); err != nil {
				t.Fatalf("start controller: %v", err)
			}
			defer controller.Stop()

			meta, err := controller.Metadata(context.Background(), source.path)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			source.segments = playlist.CalculateSegments(source.keyframes, meta.Duration, 2)
			if len(source.segments) < 8 {
				t.Fatalf("fixture must plan at least 8 segments, planned %d", len(source.segments))
			}

			joinTolerance := 2 * timelineTick
			if container == "mkv" {
				joinTolerance = 0.001
			}
			var direct, transcoded int
			videos, _, err := rendition.GenerateVideo(meta, 2)
			if err != nil {
				t.Fatalf("generate video renditions: %v", err)
			}
			for _, video := range videos {
				segments := fetchTimelineRendition(t, controller, source, StreamVideo, video.Name, "v:0")
				if video.Method == domain.DirectStream {
					direct++
					assertVideoTimeline(t, video.Name+" (copy)", source, segments, false, 2*timelineTick, joinTolerance)
					continue
				}
				transcoded++
				tolerance := timelineVideoFrame / 2
				if container == "mkv" {
					tolerance = timelineVideoFrame
				}
				assertVideoTimeline(t, video.Name+" (transcode)", source, segments, true, tolerance, joinTolerance)
			}
			if transcoded == 0 {
				t.Errorf("fixture produced no transcoded rendition")
			}
			if container == "mp4" && direct == 0 {
				t.Errorf("mp4 fixture produced no direct-stream rendition")
			}

			var stereo, passthrough int
			for _, audio := range rendition.GenerateAudioTracks(meta.Audios) {
				switch {
				case audio.Method == domain.DirectStream:
					passthrough++
					segments := fetchTimelineRendition(t, controller, source, StreamAudio, audio.Name, "a:0")
					assertAudioTimeline(t, audio.Name, source, audio.StreamIndex, segments, 1536.0/48000)
				case strings.HasSuffix(audio.Name, "aac_stereo") && stereo == 0:
					stereo++
					segments := fetchTimelineRendition(t, controller, source, StreamAudio, audio.Name, "a:0")
					assertAudioTimeline(t, audio.Name, source, audio.StreamIndex, segments, 1024.0/48000)
				}
			}
			if stereo == 0 || passthrough == 0 {
				t.Errorf("fixture must offer aac_stereo and an ac3 passthrough rendition, got %d and %d", stereo, passthrough)
			}

			if len(meta.Subtitles) == 0 {
				t.Fatalf("fixture has no subtitle track")
			}
			vtt, err := controller.SubtitleVTT(context.Background(), source.path, meta.Subtitles[0].ID)
			if err != nil {
				t.Fatalf("subtitle vtt: %v", err)
			}
			wantMap := fmt.Sprintf("X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000", int64(domain.OutputTimelineOffset.Seconds()*90000))
			if !strings.Contains(string(vtt), wantMap) {
				t.Errorf("subtitle VTT lacks %q:\n%s", wantMap, vtt)
			}
			wantCue := 1 + source.firstVideoPTS()
			if cue, ok := firstCueStart(string(vtt)); !ok || math.Abs(cue-wantCue) > 0.0015 {
				t.Errorf("first subtitle cue starts at %.3f (found %v), the video timeline puts it at %.3f:\n%s", cue, ok, wantCue, vtt)
			}

			if failures := observer.failed(); len(failures) > 0 {
				t.Errorf("jobs failed: %s", strings.Join(failures, "; "))
			}
		})
	}
}
