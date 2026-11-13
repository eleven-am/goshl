package transcode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func TestPoolTranscodeJobFollowsPlanWhenVideoStartsLate(t *testing.T) {
	path := testmedia.H264(t, testmedia.Spec{
		FPS:            "24",
		KeyframeFrames: []int{0, 48, 96, 144, 192, 240},
		Duration:       10,
		VideoDelay:     0.0834,
		Audio:          []testmedia.AudioSpec{{Codec: "aac"}},
	})
	meta, err := probe.NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	fixture := poolFixture{path: path, start: testmedia.FormatStart(t, path), meta: meta}
	fixture.segments = playlist.CalculateSegments(meta.Keyframes, meta.Duration, 2)
	if len(fixture.segments) < 4 {
		t.Fatalf("fixture must plan at least 4 segments, planned %d", len(fixture.segments))
	}

	storage := newCapturingStorage()
	coordinator := &stubCoordinator{}
	pool := newTimelinePool(storage, coordinator, domain.StreamVideo)
	pool.processJob(context.Background(), domain.JobDelivery{Job: timelineJob("late-start", fixture, transcodeRendition(), 0, 3), Receipt: "late-start"})
	if len(storage.writes) != 4 {
		t.Fatalf("job 0-3 stored %v, statuses %+v", storage.writes, coordinator.jobPublishes)
	}

	dir := t.TempDir()
	offset := domain.OutputTimelineOffset.Seconds()
	for index := 1; index <= 3; index++ {
		file := filepath.Join(dir, fmt.Sprintf("segment-%d.ts", index))
		if err := os.WriteFile(file, storage.segments[index], 0o644); err != nil {
			t.Fatalf("write segment %d: %v", index, err)
		}
		var keys []float64
		for _, packet := range testmedia.Packets(t, file, "v:0") {
			if packet.Key {
				keys = append(keys, packet.PTS)
			}
		}
		sort.Float64s(keys)
		if len(keys) == 0 {
			t.Fatalf("segment %d has no keyframe", index)
		}
		if want := fixture.segments[index].Start + offset; math.Abs(keys[0]-want) > poolVideoFrame {
			t.Errorf("segment %d starts at %.6f, planned %.6f", index, keys[0], want)
		}
	}
}

const csvFFmpegScript = `#!/bin/sh
if [ "$1" = "--emit" ]; then
  shift
  for f in "$@"; do
    echo "$f"
  done
  exit 0
fi
echo "unexpected args: $@" >&2
exit 1
`

func runWorkerWithLines(t *testing.T, lines []string, ends map[int]SegmentEnd, expected []int) (*Worker, *memoryStorage) {
	t.Helper()
	tmp := installFakeFFmpeg(t, csvFFmpegScript)
	args := []string{"--emit"}
	for _, line := range lines {
		name := line
		for i := range line {
			if line[i] == ',' {
				name = line[:i]
				break
			}
		}
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("data"), 0o644); err != nil {
			t.Fatalf("prime file: %v", err)
		}
		args = append(args, line)
	}
	storage := &memoryStorage{}
	w := NewWorker(WorkerSpec{Args: args, Storage: storage, MediaKey: "media", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: expected, Ends: ends})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitWorker(t, w, 10*time.Second)
	return w, storage
}

func TestWorkerRejectsSegmentEndingOffThePlan(t *testing.T) {
	ends := map[int]SegmentEnd{1: {Time: 15, Early: 0.1, Late: 0.1}, 2: {Time: 20, Early: 0.1, Late: 0.1}}
	w, storage := runWorkerWithLines(t, []string{"segment-00001.ts,0.000000,15.020000", "segment-00002.ts,15.020000,25.000000"}, ends, []int{1, 2})
	if !errors.Is(w.Err(), ErrSegmentTimeline) {
		t.Fatalf("worker error = %v, want ErrSegmentTimeline", w.Err())
	}
	for _, write := range storage.writes {
		if write.Index == 2 {
			t.Fatalf("segment 2 must not be stored: %v", storage.writes)
		}
	}
}

func TestWorkerRejectsMissingEndColumnWhenEndsAreSet(t *testing.T) {
	ends := map[int]SegmentEnd{1: {Time: 15, Early: 0.1, Late: 0.1}}
	w, storage := runWorkerWithLines(t, []string{"segment-00001.ts"}, ends, []int{1})
	if !errors.Is(w.Err(), ErrSegmentTimeline) {
		t.Fatalf("worker error = %v, want ErrSegmentTimeline", w.Err())
	}
	if len(storage.writes) != 0 {
		t.Fatalf("nothing may be stored: %v", storage.writes)
	}
}

func TestWorkerAcceptsSegmentEndingOnThePlan(t *testing.T) {
	ends := map[int]SegmentEnd{1: {Time: 15, Early: 0.1, Late: 0.1}}
	w, storage := runWorkerWithLines(t, []string{"segment-00001.ts,0.000000,15.040000"}, ends, []int{1})
	if w.Err() != nil || len(storage.writes) != 1 {
		t.Fatalf("worker error %v, writes %v", w.Err(), storage.writes)
	}
}
