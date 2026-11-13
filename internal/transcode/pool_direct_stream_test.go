package transcode

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/testmedia"
)

const (
	poolTick       = 1.0 / 90000
	poolVideoFrame = 1001.0 / 24000
	poolAACFrame   = 1024.0 / 48000
	poolAC3Frame   = 1536.0 / 48000
)

type capturingStorage struct {
	memoryStorage
	segments map[int][]byte
	writes   []int
}

func newCapturingStorage() *capturingStorage {
	return &capturingStorage{segments: map[int][]byte{}}
}

func (c *capturingStorage) GetMetadata(ctx context.Context, mediaKey string) ([]byte, error) {
	panic("transcode workers must not read metadata")
}

func (c *capturingStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	c.segments[info.Index] = data
	c.writes = append(c.writes, info.Index)
	return nil
}

type poolFixture struct {
	path     string
	start    float64
	video    []testmedia.Packet
	meta     *domain.Metadata
	segments []domain.Segment
}

func newPoolFixture(t *testing.T, container string) poolFixture {
	t.Helper()
	path := testmedia.H264(t, testmedia.Spec{
		Container:      container,
		BFrames:        3,
		Pyramid:        true,
		KeyframeFrames: []int{0, 50, 101, 149, 200, 251},
		Duration:       12,
		Audio:          []testmedia.AudioSpec{{Codec: "aac"}, {Codec: "ac3"}},
	})
	meta, err := probe.NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	fixture := poolFixture{path: path, start: testmedia.FormatStart(t, path), video: testmedia.Packets(t, path, "v:0"), meta: meta}
	fixture.segments = playlist.CalculateSegments(meta.Keyframes, meta.Duration, 2)
	if len(fixture.segments) < 5 {
		t.Fatalf("fixture must plan at least 5 segments, planned %d", len(fixture.segments))
	}
	return fixture
}

func (f poolFixture) sourceVideoStart(index int) float64 {
	if index > 0 {
		return f.segments[index].Start
	}
	first := math.Inf(1)
	for _, packet := range f.video {
		first = math.Min(first, packet.PTS-f.start)
	}
	return first
}

func (f poolFixture) sourceAudioStart(t *testing.T, stream int) float64 {
	t.Helper()
	packets := testmedia.Packets(t, f.path, fmt.Sprintf("%d", stream))
	if len(packets) == 0 {
		t.Fatalf("stream %d has no packets", stream)
	}
	return packets[0].PTS - f.start
}

func newTimelinePool(storage *capturingStorage, coordinator *stubCoordinator, stream domain.StreamType) *Pool {
	builder := ffmpeg.NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelNone,
		EncodeFlags:  []string{"-c:v", "libx264", "-preset", "veryfast"},
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
	})
	return NewPool(coordinator, 1, stream, 2, storage, builder, storage)
}

type poolRendition struct {
	video *domain.VideoRendition
	audio *domain.AudioRendition
}

func (r poolRendition) name() string {
	if r.video != nil {
		return r.video.Name
	}
	return r.audio.Name
}

func (r poolRendition) stream() domain.StreamType {
	if r.video != nil {
		return domain.StreamVideo
	}
	return domain.StreamAudio
}

func (r poolRendition) copiedVideo() bool {
	return r.video != nil && r.video.Method == domain.DirectStream
}

func timelineJob(id string, fixture poolFixture, rendition poolRendition, start, end int) domain.Job {
	expected := make([]int, 0, end-start+1)
	for index := start; index <= end; index++ {
		expected = append(expected, index)
	}
	job := domain.Job{
		ID:               id,
		RunID:            id,
		MediaKey:         "media",
		Profile:          "profile",
		SourceURL:        fixture.path,
		Rendition:        rendition.name(),
		StreamType:       rendition.stream(),
		StartIndex:       start,
		EndIndex:         end,
		RequestedIndex:   start,
		EnqueuedAt:       time.Now(),
		StartBy:          time.Now().Add(time.Minute),
		Segments:         fixture.segments[start : end+1],
		ExpectedIndices:  expected,
		VideoRendition:   rendition.video,
		AudioRendition:   rendition.audio,
		VideoStreamIndex: fixture.meta.Video.Index,
	}
	if start > 0 && (rendition.copiedVideo() || rendition.audio != nil) {
		job.Overlap = &fixture.segments[start-1]
	}
	if rendition.copiedVideo() && end+1 < len(fixture.segments) {
		job.Trailing = &fixture.segments[end+1]
	}
	return job
}

func runTimelineJobs(t *testing.T, fixture poolFixture, rendition poolRendition, selector string) map[int][]float64 {
	t.Helper()
	storage := newCapturingStorage()
	coordinator := &stubCoordinator{}
	pool := newTimelinePool(storage, coordinator, rendition.stream())
	for _, bounds := range [][2]int{{2, 3}, {0, 1}} {
		id := fmt.Sprintf("job-%d", bounds[0])
		storage.writes = nil
		pool.processJob(context.Background(), domain.JobDelivery{Job: timelineJob(id, fixture, rendition, bounds[0], bounds[1]), Receipt: id})
		if len(storage.writes) != 2 || storage.writes[0] != bounds[0] || storage.writes[1] != bounds[1] {
			t.Fatalf("%s job %d-%d stored %v, statuses %+v", rendition.name(), bounds[0], bounds[1], storage.writes, coordinator.jobPublishes)
		}
	}
	dir := t.TempDir()
	times := make(map[int][]float64, 4)
	for index := 0; index <= 3; index++ {
		path := filepath.Join(dir, fmt.Sprintf("segment-%d.ts", index))
		if err := os.WriteFile(path, storage.segments[index], 0o644); err != nil {
			t.Fatalf("write segment %d: %v", index, err)
		}
		for _, packet := range testmedia.Packets(t, path, selector) {
			times[index] = append(times[index], packet.PTS)
		}
		if len(times[index]) == 0 {
			t.Fatalf("%s segment %d has no packets", rendition.name(), index)
		}
		sort.Float64s(times[index])
	}
	return times
}

func assertJoins(t *testing.T, name string, times map[int][]float64, frame, tolerance float64) {
	t.Helper()
	for index := 1; index <= 3; index++ {
		previous, current := times[index-1], times[index]
		last := previous[len(previous)-1]
		seen := make(map[int64]bool, len(previous))
		for _, pts := range previous {
			seen[int64(math.Round(pts*90000))] = true
		}
		for _, pts := range current {
			if seen[int64(math.Round(pts*90000))] {
				t.Errorf("%s: segments %d and %d both contain PTS %.6f", name, index-1, index, pts)
			}
		}
		if gap := current[0] - last; math.Abs(gap-frame) > tolerance {
			t.Errorf("%s: join %d/%d advances by %.6f, want one frame %.6f", name, index-1, index, gap, frame)
		}
	}
}

func copyRendition() poolRendition {
	return poolRendition{video: &domain.VideoRendition{Name: "180p", Method: domain.DirectStream}}
}

func transcodeRendition() poolRendition {
	return poolRendition{video: &domain.VideoRendition{Name: "180p", Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 400000}}
}

func aacRendition() poolRendition {
	return poolRendition{audio: &domain.AudioRendition{Name: "aac_stereo", Method: domain.Transcode, Channels: 2, Bitrate: 128000, StreamIndex: 1}}
}

func ac3Rendition() poolRendition {
	return poolRendition{audio: &domain.AudioRendition{Name: "ac3_passthrough", Method: domain.DirectStream, Channels: 2, StreamIndex: 2}}
}

func TestPoolCopyJobsShareNoPTSAcrossJoins(t *testing.T) {
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			fixture := newPoolFixture(t, container)
			times := runTimelineJobs(t, fixture, copyRendition(), "v:0")
			tolerance := 2 * poolTick
			if container == "mkv" {
				tolerance = 0.001
			}
			assertJoins(t, "copied video", times, poolVideoFrame, tolerance)
		})
	}
}

func TestPoolJobsHaveConstantTimelineOffset(t *testing.T) {
	offset := domain.OutputTimelineOffset.Seconds()
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			fixture := newPoolFixture(t, container)
			transcodeTolerance := poolVideoFrame / 2
			if container == "mkv" {
				transcodeTolerance = poolVideoFrame
			}
			videos := []struct {
				rendition poolRendition
				tolerance float64
			}{{copyRendition(), 2 * poolTick}, {transcodeRendition(), transcodeTolerance}}
			for _, video := range videos {
				times := runTimelineJobs(t, fixture, video.rendition, "v:0")
				for index := 0; index <= 3; index++ {
					want := fixture.sourceVideoStart(index) + offset
					if diff := times[index][0] - want; math.Abs(diff) > video.tolerance {
						t.Errorf("%s video segment %d starts at %.6f, source packet plus offset is %.6f (off by %+.6f)", video.rendition.video.Method, index, times[index][0], want, diff)
					}
				}
			}
			audios := []struct {
				rendition poolRendition
				frame     float64
			}{{aacRendition(), poolAACFrame}, {ac3Rendition(), poolAC3Frame}}
			for _, audio := range audios {
				times := runTimelineJobs(t, fixture, audio.rendition, "a:0")
				for index := 0; index <= 3; index++ {
					want := fixture.segments[index].Start + offset
					if index == 0 {
						want = fixture.sourceAudioStart(t, audio.rendition.audio.StreamIndex) + offset
					}
					if diff := times[index][0] - want; math.Abs(diff) > audio.frame+2*poolTick {
						t.Errorf("%s segment %d starts at %.6f, source position plus offset is %.6f (off by %+.6f)", audio.rendition.audio.Name, index, times[index][0], want, diff)
					}
				}
			}
		})
	}
}

func TestPoolCopyAudioJobsDiscardSeekPreRoll(t *testing.T) {
	offset := domain.OutputTimelineOffset.Seconds()
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			fixture := newPoolFixture(t, container)
			times := runTimelineJobs(t, fixture, ac3Rendition(), "a:0")
			assertJoins(t, "copied audio", times, poolAC3Frame, 2*poolTick)
			boundary := fixture.segments[2].Start + offset
			if first := times[2][0]; first < boundary-2*poolTick || first > boundary+poolAC3Frame {
				t.Errorf("segment 2 starts at %.6f, want the first packet at or after %.6f", first, boundary)
			}
		})
	}
}

func TestPoolAACJobsJoinExactly(t *testing.T) {
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			fixture := newPoolFixture(t, container)
			assertJoins(t, "aac", runTimelineJobs(t, fixture, aacRendition(), "a:0"), poolAACFrame, 2*poolTick)
		})
	}
}

func TestPoolDirectStreamJobFailsWhenSeekLandingUnavailable(t *testing.T) {
	fixture := newPoolFixture(t, "mp4")
	storage := newCapturingStorage()
	coordinator := &stubCoordinator{}
	pool := newTimelinePool(storage, coordinator, domain.StreamVideo)
	fixture.path = filepath.Join(t.TempDir(), "missing.mp4")

	pool.processJob(context.Background(), domain.JobDelivery{Job: timelineJob("job-a", fixture, copyRendition(), 2, 3), Receipt: "job-a"})

	if len(storage.writes) != 0 {
		t.Fatalf("failed job must store no segments, stored %v", storage.writes)
	}
	if len(coordinator.jobPublishes) != 1 || coordinator.jobPublishes[0].State != domain.JobStateFailed {
		t.Fatalf("expected one failed settlement, got %v", coordinator.jobPublishes)
	}
	if !strings.Contains(coordinator.jobPublishes[0].Error, "seek landing") {
		t.Fatalf("failure must come from the seek landing measurement, got %q", coordinator.jobPublishes[0].Error)
	}
}

func newAudioFirstFixture(t *testing.T) poolFixture {
	t.Helper()
	base := newPoolFixture(t, "mkv")
	path := filepath.Join(t.TempDir(), "audio-first.mkv")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", base.path, "-map", "0:a", "-map", "0:v", "-c", "copy", path}
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("remux audio-first fixture: %v: %s", err, data)
	}
	meta, err := probe.NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe audio-first fixture: %v", err)
	}
	if meta.Video.Index == 0 {
		t.Fatalf("audio-first fixture must place video after the audio streams")
	}
	fixture := poolFixture{path: path, start: testmedia.FormatStart(t, path), video: testmedia.Packets(t, path, "v:0"), meta: meta}
	fixture.segments = playlist.CalculateSegments(meta.Keyframes, meta.Duration, 2)
	if len(fixture.segments) < 5 {
		t.Fatalf("fixture must plan at least 5 segments, planned %d", len(fixture.segments))
	}
	return fixture
}

func TestPoolVideoJobsUseProbedVideoStreamIndex(t *testing.T) {
	fixture := newAudioFirstFixture(t)
	for _, rendition := range []poolRendition{copyRendition(), transcodeRendition()} {
		t.Run(string(rendition.video.Method), func(t *testing.T) {
			times := runTimelineJobs(t, fixture, rendition, "v:0")
			for index := 0; index <= 3; index++ {
				if len(times[index]) == 0 {
					t.Fatalf("segment %d carries no video packets", index)
				}
			}
		})
	}
}
