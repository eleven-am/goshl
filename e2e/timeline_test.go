package e2e

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

const lateVideoStart = 0.0834

func lateStartFixture(t *testing.T) timelineSource {
	t.Helper()
	path := testmedia.H264(t, testmedia.Spec{
		Container:      "mp4",
		Width:          480,
		Height:         400,
		FPS:            "24000/1001",
		BFrames:        3,
		Pyramid:        true,
		VideoDelay:     lateVideoStart,
		KeyframeFrames: []int{0, 25, 50, 101, 125, 149, 200, 251, 299, 350, 401, 449, 500, 551},
		Duration:       24,
		Audio:          []testmedia.AudioSpec{{Codec: "aac"}},
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

func TestControllerLateVideoStartDirectStreamSharesOneTimeline(t *testing.T) {
	testmedia.Require(t)
	source := lateStartFixture(t)
	if first := source.firstVideoPTS(); math.Abs(first-lateVideoStart) > 0.002 {
		t.Fatalf("fixture video starts at %.4f, want %.4f", first, lateVideoStart)
	}
	controller := goshl.NewController(goshl.Options{
		Storage:        newTimelineStorage(),
		Coordinator:    newClusterCoordinator(),
		PathGen:        stubPathGen{},
		TargetDuration: 2,
		SegmentsPerJob: 2,
		SegmentTimeout: 2 * time.Minute,
		JobTimeout:     2 * time.Minute,
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
	if !meta.Video.StartsWithKeyframe {
		t.Fatalf("late-start source must report StartsWithKeyframe, start %.4f keyframes %v", meta.Video.Start, meta.Keyframes)
	}
	source.segments = playlist.CalculateSegments(source.keyframes, meta.Duration, 2)
	if len(source.segments) < 8 {
		t.Fatalf("fixture must plan at least 8 segments, planned %d", len(source.segments))
	}
	videos, _, err := rendition.GenerateVideo(meta, 2)
	if err != nil {
		t.Fatalf("generate video renditions: %v", err)
	}
	if videos[0].Method != goshl.DirectStream {
		t.Fatalf("source rung method = %s, want direct_stream", videos[0].Method)
	}

	video := fetchTimelineRendition(t, controller, source, goshl.StreamVideo, videos[0].Name, "v:0")
	assertVideoTimeline(t, videos[0].Name+" (copy)", source, video, false, 2*timelineTick, 2*timelineTick)

	var audio []timelineSegment
	var audioName string
	for _, track := range rendition.GenerateAudioTracks(meta.Audios) {
		if strings.HasSuffix(track.Name, "aac_stereo") {
			audioName = track.Name
			audio = fetchTimelineRendition(t, controller, source, goshl.StreamAudio, track.Name, "a:0")
			assertAudioTimeline(t, track.Name, source, track.StreamIndex, audio, 1024.0/48000)
			break
		}
	}
	if len(audio) == 0 {
		t.Fatalf("no aac_stereo rendition")
	}

	offset := outputTimelineOffset.Seconds()
	if got := video[0].pts[0] - offset; math.Abs(got-source.firstVideoPTS()) > 2*timelineTick {
		t.Errorf("job 0 video starts %.4f s into the timeline, want %.4f", got, source.firstVideoPTS())
	}
	for index := range video {
		want := source.expectedStart(index) + offset
		if diff := video[index].pts[0] - want; math.Abs(diff) > 2*timelineTick {
			t.Errorf("segment %d (job %d) carries offset %+.4f, want %+.4f", index, index/2, video[index].pts[0]-source.expectedStart(index), offset)
		}
	}
	packets := testmedia.Packets(t, source.path, strconv.Itoa(1))
	gap := video[0].pts[0] - audio[0].pts[0]
	wantGap := source.firstVideoPTS() - (packets[0].PTS - source.start)
	if math.Abs(gap-wantGap) > 1024.0/48000+2*timelineTick {
		t.Errorf("%s: video leads audio by %.4f at the start, the source has %.4f", audioName, gap, wantGap)
	}
}

const (
	timelineTick       = 1.0 / 90000
	timelineVideoFrame = 1001.0 / 24000
)

type timelineSegment struct {
	pts  []float64
	keys []float64
}

type timelineSource struct {
	path      string
	start     float64
	video     []testmedia.Packet
	segments  []goshl.Segment
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

func fetchTimelineRendition(t *testing.T, controller *goshl.Controller, source timelineSource, stream goshl.StreamType, name, selector string) []timelineSegment {
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
	offset := outputTimelineOffset.Seconds()
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
	offset := outputTimelineOffset.Seconds()
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

func TestControllerSegmentsFormOneContinuousTimeline(t *testing.T) {
	testmedia.Require(t)
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			source := timelineFixture(t, container)
			storage := newTimelineStorage()
			observer := &timelineObserver{}
			controller := goshl.NewController(goshl.Options{
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
				segments := fetchTimelineRendition(t, controller, source, goshl.StreamVideo, video.Name, "v:0")
				if video.Method == goshl.DirectStream {
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
				case audio.Method == goshl.DirectStream:
					passthrough++
					segments := fetchTimelineRendition(t, controller, source, goshl.StreamAudio, audio.Name, "a:0")
					assertAudioTimeline(t, audio.Name, source, audio.StreamIndex, segments, 1536.0/48000)
				case strings.HasSuffix(audio.Name, "aac_stereo") && stereo == 0:
					stereo++
					segments := fetchTimelineRendition(t, controller, source, goshl.StreamAudio, audio.Name, "a:0")
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
			wantMap := fmt.Sprintf("X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000", int64(outputTimelineOffset.Seconds()*90000))
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
