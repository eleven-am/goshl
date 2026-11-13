package transcode

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func newShortAudioFixture(t *testing.T, container string) poolFixture {
	t.Helper()
	keyframes := make([]int, 0, 10)
	for frame := 0; frame < 240; frame += 24 {
		keyframes = append(keyframes, frame)
	}
	path := testmedia.H264(t, testmedia.Spec{
		Container:      container,
		FPS:            "24",
		KeyframeFrames: keyframes,
		Duration:       10,
		Audio:          []testmedia.AudioSpec{{Codec: "aac", Duration: 7.5}, {Codec: "ac3", Duration: 7.5}},
	})
	meta, err := probe.NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	fixture := poolFixture{path: path, start: testmedia.FormatStart(t, path), meta: meta}
	fixture.segments = playlist.CalculateSegments(meta.Keyframes, meta.Duration, 1)
	if len(fixture.segments) != 10 {
		t.Fatalf("fixture must plan 10 segments, planned %d", len(fixture.segments))
	}
	for _, audio := range meta.Audios {
		if audio.End < 7.3 || audio.End > 7.6 {
			t.Fatalf("fixture audio stream %d must end near 7.5 s, probe says %.3f", audio.Index, audio.End)
		}
	}
	return fixture
}

func shortAudioRendition(t *testing.T, fixture poolFixture, suffix string) poolRendition {
	t.Helper()
	for _, candidate := range rendition.GenerateAudioTracks(fixture.meta.Audios) {
		if strings.HasSuffix(candidate.Name, suffix) {
			value := candidate
			return poolRendition{audio: &value}
		}
	}
	t.Fatalf("fixture offers no %s rendition", suffix)
	return poolRendition{}
}

func runAudioPartition(t *testing.T, fixture poolFixture, rendition poolRendition, partition [][2]int) map[int][]float64 {
	t.Helper()
	storage := newCapturingStorage()
	coordinator := &stubCoordinator{}
	pool := newTimelinePool(storage, coordinator, domain.StreamAudio)
	for _, bounds := range partition {
		id := fmt.Sprintf("job-%d-%d", bounds[0], bounds[1])
		storage.writes = nil
		pool.processJob(context.Background(), domain.JobDelivery{Job: timelineJob(id, fixture, rendition, bounds[0], bounds[1]), Receipt: id})
		want := bounds[1] - bounds[0] + 1
		if len(storage.writes) != want {
			t.Fatalf("%s job %d-%d stored %v, want %d segments, statuses %+v", rendition.name(), bounds[0], bounds[1], storage.writes, want, coordinator.jobPublishes)
		}
	}
	dir := t.TempDir()
	times := make(map[int][]float64, len(fixture.segments))
	for index := range fixture.segments {
		path := filepath.Join(dir, fmt.Sprintf("segment-%d.ts", index))
		if err := os.WriteFile(path, storage.segments[index], 0o644); err != nil {
			t.Fatalf("write segment %d: %v", index, err)
		}
		for _, packet := range testmedia.Packets(t, path, "a:0") {
			times[index] = append(times[index], packet.PTS)
		}
		if len(times[index]) == 0 {
			t.Fatalf("%s segment %d has no packets", rendition.name(), index)
		}
		sort.Float64s(times[index])
	}
	return times
}

func TestAudioShorterThanPlanStillYieldsEverySegment(t *testing.T) {
	offset := domain.OutputTimelineOffset.Seconds()
	partitions := map[string][][2]int{
		"padded tail":      {{0, 5}, {6, 9}},
		"silent tail":      {{0, 6}, {7, 8}, {9, 9}},
		"single segments":  {{0, 5}, {6, 6}, {7, 7}, {8, 8}, {9, 9}},
		"late jobs first":  {{9, 9}, {8, 8}, {0, 7}},
		"silent pair":      {{0, 7}, {8, 9}},
		"silent long tail": {{0, 4}, {5, 6}, {7, 7}, {9, 9}, {8, 8}},
	}
	renditions := []struct {
		suffix string
		frame  float64
	}{{"aac_stereo", poolAACFrame}, {"ac3_passthrough", poolAC3Frame}}
	for _, container := range []string{"mp4", "mkv"} {
		fixture := newShortAudioFixture(t, container)
		for _, candidate := range renditions {
			rendition := shortAudioRendition(t, fixture, candidate.suffix)
			for name, partition := range partitions {
				t.Run(container+"/"+candidate.suffix+"/"+name, func(t *testing.T) {
					times := runAudioPartition(t, fixture, rendition, partition)
					for index := 1; index < len(fixture.segments); index++ {
						previous, current := times[index-1], times[index]
						if gap := current[0] - previous[len(previous)-1]; math.Abs(gap-candidate.frame) > 2*poolTick {
							t.Errorf("join %d/%d advances by %.6f, want one frame %.6f", index-1, index, gap, candidate.frame)
						}
						boundary := fixture.segments[index].Start + offset
						if first := current[0]; first < boundary-candidate.frame-2*poolTick || first > boundary+candidate.frame+2*poolTick {
							t.Errorf("segment %d starts at %.6f, want within one frame of %.6f", index, first, boundary)
						}
					}
					last := times[len(times)-1]
					if end := last[len(last)-1] + candidate.frame; math.Abs(end-(fixture.meta.Duration+offset)) > 2*candidate.frame {
						t.Errorf("audio ends at %.6f, want the plan end %.6f", end, fixture.meta.Duration+offset)
					}
				})
			}
		}
	}
}
