package transcode

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func newShortVideoFixture(t *testing.T, container string) poolFixture {
	t.Helper()
	keyframes := make([]int, 0, 6)
	for frame := 0; frame < 144; frame += 24 {
		keyframes = append(keyframes, frame)
	}
	path := testmedia.H264(t, testmedia.Spec{
		Container:      container,
		FPS:            "24",
		KeyframeFrames: keyframes,
		Duration:       5.75,
		Audio:          []testmedia.AudioSpec{{Codec: "aac", Duration: 20}},
	})
	meta, err := probe.NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	if meta.Video.End < 5.7 || meta.Video.End > 5.85 || meta.Duration < 19.9 {
		t.Fatalf("fixture must hold 5.75 s of video in a 20 s file, probe says video end %.3f, duration %.3f", meta.Video.End, meta.Duration)
	}
	fixture := poolFixture{path: path, start: testmedia.FormatStart(t, path), video: testmedia.Packets(t, path, "v:0"), meta: meta}
	fixture.segments = playlist.CalculateSegments(meta.Keyframes, meta.Duration, 0.99)
	if len(fixture.segments) < 15 || fixture.segments[5].Start > 5.1 || fixture.segments[6].Start < 5.9 {
		t.Fatalf("fixture must plan segments of about 1 s with the video ending inside segment 5, planned %+v", fixture.segments)
	}
	return fixture
}

func runVideoPartition(t *testing.T, fixture poolFixture, rendition poolRendition, partition [][2]int) map[int]timedSegment {
	t.Helper()
	storage := newCapturingStorage()
	coordinator := &stubCoordinator{}
	pool := newTimelinePool(storage, coordinator, domain.StreamVideo)
	for _, bounds := range partition {
		id := fmt.Sprintf("job-%d-%d", bounds[0], bounds[1])
		storage.writes = nil
		pool.processJob(context.Background(), domain.JobDelivery{Job: timelineJob(id, fixture, rendition, bounds[0], bounds[1]), Receipt: id})
		if want := bounds[1] - bounds[0] + 1; len(storage.writes) != want {
			t.Fatalf("%s job %d-%d stored %v, want %d segments, statuses %+v", rendition.name(), bounds[0], bounds[1], storage.writes, want, coordinator.jobPublishes)
		}
	}
	dir := t.TempDir()
	segments := make(map[int]timedSegment, len(fixture.segments))
	for index := range fixture.segments {
		path := filepath.Join(dir, fmt.Sprintf("segment-%d.ts", index))
		if err := os.WriteFile(path, storage.segments[index], 0o644); err != nil {
			t.Fatalf("write segment %d: %v", index, err)
		}
		var segment timedSegment
		for _, packet := range testmedia.Packets(t, path, "v:0") {
			segment.pts = append(segment.pts, packet.PTS)
			if packet.Key {
				segment.keys = append(segment.keys, packet.PTS)
			}
		}
		if len(segment.pts) == 0 {
			t.Fatalf("%s segment %d has no packets", rendition.name(), index)
		}
		sort.Float64s(segment.pts)
		segments[index] = segment
	}
	return segments
}

type timedSegment struct {
	pts  []float64
	keys []float64
}

func TestVideoShorterThanPlanStillYieldsEverySegment(t *testing.T) {
	offset := domain.OutputTimelineOffset.Seconds()
	frame := 1.0 / 24
	for _, container := range []string{"mp4", "mkv"} {
		fixture := newShortVideoFixture(t, container)
		last := len(fixture.segments) - 1
		ladder, _, err := rendition.GenerateVideo(fixture.meta, 0.99)
		if err != nil || len(ladder) == 0 {
			t.Fatalf("%s: generate ladder: %v", container, err)
		}
		source := ladder[0]
		if source.Method != domain.Transcode || source.SourceEnd != fixture.meta.Video.End || source.SourceEnd == 0 {
			t.Fatalf("%s: a source whose video ends before the plan must transcode its source rung and carry the video end, got %+v", container, source)
		}
		partitions := map[string][][2]int{
			"padded tail":     {{0, 3}, {4, last}},
			"frozen tail":     {{0, 5}, {6, 9}, {10, last}},
			"single segments": {{0, 4}, {5, 5}, {6, 6}, {7, last}},
			"late jobs first": {{last, last}, {last - 1, last - 1}, {0, last - 2}},
		}
		tolerance := frame
		for name, partition := range partitions {
			t.Run(container+"/"+name, func(t *testing.T) {
				segments := runVideoPartition(t, fixture, poolRendition{video: &source}, partition)
				for index := range fixture.segments {
					segment := segments[index]
					if len(segment.keys) != 1 || segment.keys[0] != segment.pts[0] {
						t.Errorf("segment %d must open with its only keyframe, keyframes %v, first PTS %.6f", index, segment.keys, segment.pts[0])
					}
					if index == 0 {
						continue
					}
					if want := fixture.segments[index].Start + offset; math.Abs(segment.pts[0]-want) > tolerance {
						t.Errorf("segment %d starts at %.6f, planned %.6f", index, segment.pts[0], want)
					}
					previous := segments[index-1]
					if gap := segment.pts[0] - previous.pts[len(previous.pts)-1]; math.Abs(gap-frame) > 0.0015 {
						t.Errorf("join %d/%d advances by %.6f, want one frame %.6f", index-1, index, gap, frame)
					}
				}
				final := segments[last]
				if end := final.pts[len(final.pts)-1] + frame; math.Abs(end-(fixture.meta.Duration+offset)) > 2*frame {
					t.Errorf("video ends at %.6f, want the plan end %.6f", end, fixture.meta.Duration+offset)
				}
			})
		}
	}
}
