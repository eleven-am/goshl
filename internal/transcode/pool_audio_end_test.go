package transcode

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
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
	return shortAudioFixture(t, container, nil, testmedia.AudioSpec{Codec: "aac", Duration: 7.5}, testmedia.AudioSpec{Codec: "ac3", Duration: 7.5})
}

func shortAudioFixture(t *testing.T, container string, undeclared func(*testing.T, string) string, audio ...testmedia.AudioSpec) poolFixture {
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
		Audio:          audio,
	})
	if undeclared != nil {
		path = undeclared(t, path)
		requireNoDeclaredStreamEnd(t, path)
	}
	meta, err := probe.NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	fixture := poolFixture{path: path, start: testmedia.FormatStart(t, path), meta: meta}
	fixture.segments = playlist.CalculateSegments(meta.Keyframes, meta.Duration, 0.99)
	if len(fixture.segments) != 10 {
		t.Fatalf("fixture must plan 10 segments, planned %d", len(fixture.segments))
	}
	for _, stream := range meta.Audios {
		if stream.End < 7.3 || stream.End > 7.6 {
			t.Fatalf("fixture audio stream %d must end near 7.5 s, probe says %.3f", stream.Index, stream.End)
		}
	}
	return fixture
}

func pipedCopy(t *testing.T, path string) string {
	t.Helper()
	piped := filepath.Join(t.TempDir(), "piped.mkv")
	file, err := os.Create(piped)
	if err != nil {
		t.Fatalf("create piped fixture: %v", err)
	}
	defer file.Close()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", path, "-map", "0", "-c", "copy", "-f", "matroska", "pipe:1")
	cmd.Stdout = file
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("write piped fixture: %v: %s", err, stderr.String())
	}
	return piped
}

func untaggedCopy(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	untagged := filepath.Join(t.TempDir(), "untagged.mkv")
	if err := os.WriteFile(untagged, bytes.ReplaceAll(data, []byte("DURATION"), []byte("DURATIOX")), 0o644); err != nil {
		t.Fatalf("write untagged fixture: %v", err)
	}
	return untagged
}

func requireNoDeclaredStreamEnd(t *testing.T, path string) {
	t.Helper()
	declared, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=duration:stream_tags=DURATION", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("inspect fixture: %v", err)
	}
	if left := strings.Trim(strings.NewReplacer("N/A", "", "\n", "").Replace(string(declared)), ", "); left != "" {
		t.Fatalf("fixture must declare no stream duration, ffprobe reports %q", left)
	}
}

func segmentSampleRate(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "segment.ts")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=sample_rate", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe segment: %v", err)
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
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
	times, _ := runAudioPartitionStored(t, fixture, rendition, partition)
	return times
}

func runAudioPartitionStored(t *testing.T, fixture poolFixture, rendition poolRendition, partition [][2]int) (map[int][]float64, *capturingStorage) {
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
	return times, storage
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

func tailPartitions(fixture poolFixture) map[string][][2]int {
	last := len(fixture.segments) - 1
	return map[string][][2]int{
		"silent tail":     {{0, last - 3}, {last - 2, last - 1}, {last, last}},
		"late jobs first": {{last, last}, {last - 1, last - 1}, {0, last - 2}},
	}
}

func assertAudioTail(t *testing.T, fixture poolFixture, times map[int][]float64, frame, tolerance float64) {
	t.Helper()
	offset := domain.OutputTimelineOffset.Seconds()
	for index := 1; index < len(fixture.segments); index++ {
		previous, current := times[index-1], times[index]
		if gap := current[0] - previous[len(previous)-1]; math.Abs(gap-frame) > tolerance {
			t.Errorf("join %d/%d advances by %.6f, want one frame %.6f", index-1, index, gap, frame)
		}
		boundary := fixture.segments[index].Start + offset
		if first := current[0]; first < boundary-frame-tolerance || first > boundary+frame+tolerance {
			t.Errorf("segment %d starts at %.6f, want within one frame of %.6f", index, first, boundary)
		}
	}
	last := times[len(times)-1]
	if end := last[len(last)-1] + frame; math.Abs(end-(fixture.meta.Duration+offset)) > 2*frame {
		t.Errorf("audio ends at %.6f, want the plan end %.6f", end, fixture.meta.Duration+offset)
	}
}

func TestPassthroughSilenceFollowsSourceSampleRate(t *testing.T) {
	for _, rate := range []int{44100, 32000} {
		for _, container := range []string{"mp4", "mkv"} {
			fixture := shortAudioFixture(t, container, nil,
				testmedia.AudioSpec{Codec: "ac3", Duration: 7.5, SampleRate: rate},
				testmedia.AudioSpec{Codec: "eac3", Duration: 7.5, SampleRate: rate},
			)
			frame := 1536.0 / float64(rate)
			tolerance := 2 * poolTick
			if container == "mkv" {
				tolerance = 0.0015
			}
			for _, suffix := range []string{"ac3_passthrough", "eac3_passthrough"} {
				rendition := shortAudioRendition(t, fixture, suffix)
				if rendition.audio.SampleRate != rate {
					t.Fatalf("%s must carry the source sample rate %d, got %d", rendition.name(), rate, rendition.audio.SampleRate)
				}
				for name, partition := range tailPartitions(fixture) {
					t.Run(fmt.Sprintf("%d/%s/%s/%s", rate, container, suffix, name), func(t *testing.T) {
						times, storage := runAudioPartitionStored(t, fixture, rendition, partition)
						assertAudioTail(t, fixture, times, frame, tolerance)
						for _, index := range []int{0, len(fixture.segments) - 3, len(fixture.segments) - 1} {
							if got := segmentSampleRate(t, storage.segments[index]); got != fmt.Sprint(rate) {
								t.Errorf("segment %d is sampled at %s Hz, the source is %d Hz", index, got, rate)
							}
						}
					})
				}
			}
		}
	}
}

func TestAudioPastUndeclaredEndStillYieldsEverySegment(t *testing.T) {
	shapes := map[string]func(*testing.T, string) string{"written to a pipe": pipedCopy, "tags removed": untaggedCopy}
	renditions := []struct {
		suffix string
		frame  float64
	}{{"aac_stereo", poolAACFrame}, {"ac3_passthrough", poolAC3Frame}}
	for shape, undeclared := range shapes {
		fixture := shortAudioFixture(t, "mkv", undeclared, testmedia.AudioSpec{Codec: "aac", Duration: 7.5}, testmedia.AudioSpec{Codec: "ac3", Duration: 7.5})
		for _, candidate := range renditions {
			rendition := shortAudioRendition(t, fixture, candidate.suffix)
			for name, partition := range tailPartitions(fixture) {
				t.Run(shape+"/"+candidate.suffix+"/"+name, func(t *testing.T) {
					assertAudioTail(t, fixture, runAudioPartition(t, fixture, rendition, partition), candidate.frame, 0.0015)
				})
			}
		}
	}
}
