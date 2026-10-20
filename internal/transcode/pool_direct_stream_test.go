package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
)

type capturingStorage struct {
	memoryStorage
	metadata map[string][]byte
	segments map[int][]byte
	writes   []int
}

func newCapturingStorage() *capturingStorage {
	return &capturingStorage{metadata: map[string][]byte{}, segments: map[int][]byte{}}
}

func (c *capturingStorage) MetadataExists(ctx context.Context, mediaKey string) (bool, error) {
	_, ok := c.metadata[mediaKey]
	return ok, nil
}

func (c *capturingStorage) GetMetadata(ctx context.Context, mediaKey string) ([]byte, error) {
	return c.metadata[mediaKey], nil
}

func (c *capturingStorage) SetMetadata(ctx context.Context, mediaKey string, data []byte) error {
	c.metadata[mediaKey] = data
	return nil
}

func (c *capturingStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	c.segments[info.Index] = data
	c.writes = append(c.writes, info.Index)
	return nil
}

func videoPacketTimes(t *testing.T, ffprobePath, dir string, index int, data []byte) []float64 {
	t.Helper()
	path := filepath.Join(dir, "segment-"+strconv.Itoa(index)+".ts")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write segment %d: %v", index, err)
	}
	out, err := exec.Command(ffprobePath, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("probe segment %d: %v", index, err)
	}
	var times []float64
	for _, line := range strings.Fields(string(out)) {
		value, err := strconv.ParseFloat(strings.TrimSuffix(line, ","), 64)
		if err != nil {
			t.Fatalf("parse pts %q: %v", line, err)
		}
		times = append(times, value)
	}
	if len(times) == 0 {
		t.Fatalf("segment %d has no video packets", index)
	}
	sort.Float64s(times)
	return times
}

type directStreamFixture struct {
	name   string
	file   string
	bframe string
	extra  []string
}

var directStreamFixtures = []directStreamFixture{
	{name: "mp4 with B-frames", file: "b.mp4", bframe: "3", extra: []string{"-movflags", "+faststart"}},
	{name: "mkv with B-frames", file: "b.mkv", bframe: "3"},
	{name: "mkv without B-frames", file: "nob.mkv", bframe: "0"},
}

func newDirectStreamFixture(t *testing.T, ffmpegPath string, fixture directStreamFixture) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), fixture.file)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-t", "10", "-c:v", "libx264", "-bf", fixture.bframe, "-g", "25", "-keyint_min", "25", "-sc_threshold", "0",
	}
	args = append(args, fixture.extra...)
	args = append(args, source)
	if data, err := exec.Command(ffmpegPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, data)
	}
	return source
}

func newDirectStreamPool(storage *capturingStorage, coordinator *stubCoordinator) *Pool {
	builder := ffmpeg.NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelNone,
		EncodeFlags:  []string{"-c:v", "libx264"},
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
	})
	return NewPool(coordinator, 1, domain.StreamVideo, 2, storage, builder, storage)
}

func directStreamJob(id, source string, segments []domain.Segment, start, end int, overlap *domain.Segment, rendition *domain.VideoRendition) domain.Job {
	expected := make([]int, 0, end-start+1)
	for index := start; index <= end; index++ {
		expected = append(expected, index)
	}
	return domain.Job{
		ID:              id,
		RunID:           id,
		MediaKey:        "media",
		Profile:         "profile",
		SourceURL:       source,
		Rendition:       rendition.Name,
		StreamType:      domain.StreamVideo,
		StartIndex:      start,
		EndIndex:        end,
		RequestedIndex:  start,
		EnqueuedAt:      time.Now(),
		Deadline:        time.Now().Add(time.Minute),
		Segments:        segments[start : end+1],
		Overlap:         overlap,
		ExpectedIndices: expected,
		VideoRendition:  rendition,
	}
}

func TestPoolDirectStreamJobsAlignSegmentsToPlaylist(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable")
	}

	for _, fixture := range directStreamFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			source := newDirectStreamFixture(t, ffmpegPath, fixture)
			storage := newCapturingStorage()
			ctx := context.Background()
			meta, err := probe.NewProber(storage).ProbeWithKey(ctx, source, "media")
			if err != nil {
				t.Fatalf("probe fixture: %v", err)
			}
			segments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, 2)
			if len(segments) < 4 {
				t.Fatalf("expected at least 4 segments, got %d", len(segments))
			}

			pool := newDirectStreamPool(storage, &stubCoordinator{})
			rendition := &domain.VideoRendition{Name: "1080p", Method: domain.DirectStream}

			pool.processJob(ctx, domain.JobDelivery{Job: directStreamJob("job-a", source, segments, 0, 1, nil, rendition), Receipt: "job-a"})
			storage.writes = nil
			overlap := segments[1]
			pool.processJob(ctx, domain.JobDelivery{Job: directStreamJob("job-b", source, segments, 2, 3, &overlap, rendition), Receipt: "job-b"})

			if len(storage.writes) != 2 || storage.writes[0] != 2 || storage.writes[1] != 3 {
				t.Fatalf("job B must store exactly segments 2 and 3, stored %v", storage.writes)
			}

			const frame = 0.04
			const slack = 0.001
			dir := t.TempDir()
			times := make(map[int][]float64, 4)
			for index := 0; index <= 3; index++ {
				data, ok := storage.segments[index]
				if !ok {
					t.Fatalf("segment %d was not stored", index)
				}
				times[index] = videoPacketTimes(t, ffprobePath, dir, index, data)
				head := times[index][0]
				tolerance := frame + slack
				if index < 2 {
					tolerance = 2*frame + slack
				}
				if diff := head - segments[index].Start; diff > tolerance || diff < -tolerance {
					t.Errorf("segment %d starts at %.3f, playlist start is %.3f", index, head, segments[index].Start)
				}
			}

			lastOfPrevious := times[1][len(times[1])-1]
			if gap := times[2][0] - lastOfPrevious; gap > frame+slack {
				t.Errorf("gap between segment 1 and 2 is %.3f, want at most one frame", gap)
			}
		})
	}
}

func TestPoolDirectStreamJobFailsWhenSeekLandingUnavailable(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}

	source := newDirectStreamFixture(t, ffmpegPath, directStreamFixtures[0])
	storage := newCapturingStorage()
	ctx := context.Background()
	meta, err := probe.NewProber(storage).ProbeWithKey(ctx, source, "media")
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	segments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, 2)

	coordinator := &stubCoordinator{}
	pool := newDirectStreamPool(storage, coordinator)
	rendition := &domain.VideoRendition{Name: "1080p", Method: domain.DirectStream}
	missing := filepath.Join(t.TempDir(), "missing.mp4")

	pool.processJob(ctx, domain.JobDelivery{Job: directStreamJob("job-a", missing, segments, 2, 3, nil, rendition), Receipt: "job-a"})

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
