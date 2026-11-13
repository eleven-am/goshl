package probe

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/testmedia"
)

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

func shortAudioSource(t *testing.T, audio ...testmedia.AudioSpec) string {
	t.Helper()
	return longSource(t, 10, audio...)
}

func longSource(t *testing.T, seconds int, audio ...testmedia.AudioSpec) string {
	t.Helper()
	keyframes := make([]int, 0, seconds)
	for frame := 0; frame < seconds*24; frame += 24 {
		keyframes = append(keyframes, frame)
	}
	return testmedia.H264(t, testmedia.Spec{Container: "mkv", Width: 160, Height: 90, FPS: "24", KeyframeFrames: keyframes, Duration: float64(seconds), Audio: audio})
}

func untag(t *testing.T, source string) string {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Contains(data, []byte("DURATION")) {
		t.Fatalf("fixture must carry DURATION tags to remove")
	}
	path := filepath.Join(t.TempDir(), "untagged.mkv")
	if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte("DURATION"), []byte("DURATIOX")), 0o644); err != nil {
		t.Fatalf("write untagged fixture: %v", err)
	}
	requireNoDeclaredStreamEnd(t, path)
	return path
}

func pipedFixture(t *testing.T, audio ...testmedia.AudioSpec) string {
	t.Helper()
	piped := filepath.Join(t.TempDir(), "piped.mkv")
	file, err := os.Create(piped)
	if err != nil {
		t.Fatalf("create piped fixture: %v", err)
	}
	defer file.Close()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", shortAudioSource(t, audio...), "-map", "0", "-c", "copy", "-f", "matroska", "pipe:1")
	cmd.Stdout = file
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("write piped fixture: %v: %s", err, stderr.String())
	}
	requireNoDeclaredStreamEnd(t, piped)
	return piped
}

func untaggedFixture(t *testing.T, audio ...testmedia.AudioSpec) string {
	t.Helper()
	return untag(t, shortAudioSource(t, audio...))
}

func measuredStreamEnd(t *testing.T, path string, stream int) float64 {
	t.Helper()
	end := 0.0
	for _, packet := range testmedia.Packets(t, path, strconv.Itoa(stream)) {
		end = math.Max(end, packet.PTS+packet.Duration)
	}
	return end - testmedia.FormatStart(t, path)
}

func TestProbeMeasuresAudioEndWhenNothingDeclaresIt(t *testing.T) {
	audio := []testmedia.AudioSpec{{Codec: "aac", Duration: 7.5}, {Codec: "ac3", Duration: 9.7}}
	fixtures := map[string]string{"written to a pipe": pipedFixture(t, audio...), "tags removed": untaggedFixture(t, audio...)}
	for name, path := range fixtures {
		meta, err := NewProber().ProbeUncached(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: probe: %v", name, err)
		}
		if len(meta.Audios) != 2 {
			t.Fatalf("%s: fixture must carry two audio streams, got %d", name, len(meta.Audios))
		}
		for _, stream := range meta.Audios {
			want := measuredStreamEnd(t, path, stream.Index)
			if math.Abs(stream.End-want) > 0.001 {
				t.Errorf("%s: stream %d (%s): End = %.6f, its last packet ends at %.6f", name, stream.Index, stream.Codec, stream.End, want)
			}
		}
		if meta.Duration < 9.9 || meta.Duration > 10.1 {
			t.Errorf("%s: Duration = %.3f, the video lasts 10 s", name, meta.Duration)
		}
	}
}

func TestProbeFindsStreamEndsFarBeforeTheFileEnd(t *testing.T) {
	path := untag(t, longSource(t, 960,
		testmedia.AudioSpec{Codec: "aac"},
		testmedia.AudioSpec{Codec: "ac3", Duration: 20},
		testmedia.AudioSpec{Codec: "ac3", Duration: 501.3},
		testmedia.AudioSpec{Codec: "aac", Duration: 951},
	))
	meta, err := NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(meta.Audios) != 4 {
		t.Fatalf("fixture must carry four audio streams, got %d", len(meta.Audios))
	}
	for _, stream := range meta.Audios {
		if want := measuredStreamEnd(t, path, stream.Index); math.Abs(stream.End-want) > 0.001 {
			t.Errorf("stream %d (%s): End = %.6f, its last packet ends at %.6f", stream.Index, stream.Codec, stream.End, want)
		}
	}
}

func gappedTrackFixture(t *testing.T, seconds int, resumeAt, resumeFor float64) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if data, err := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("build gapped fixture: %v: %s", err, data)
		}
	}
	first, second := filepath.Join(dir, "first.mka"), filepath.Join(dir, "second.mka")
	run("-f", "lavfi", "-t", "20", "-i", "sine=frequency=550:sample_rate=48000", "-c:a", "ac3", first)
	run("-f", "lavfi", "-t", strconv.FormatFloat(resumeFor, 'f', 3, 64), "-i", "sine=frequency=660:sample_rate=48000", "-c:a", "ac3", second)
	list := filepath.Join(dir, "track.ffconcat")
	script := "ffconcat version 1.0\nfile first.mka\nduration " + strconv.FormatFloat(resumeAt, 'f', 3, 64) + "\nfile second.mka\n"
	if err := os.WriteFile(list, []byte(script), 0o644); err != nil {
		t.Fatalf("write concat list: %v", err)
	}
	joined := filepath.Join(dir, "gapped.mkv")
	run("-i", longSource(t, seconds, testmedia.AudioSpec{Codec: "aac"}), "-f", "concat", "-safe", "0", "-i", list, "-map", "0:v", "-map", "0:a", "-map", "1:a", "-c", "copy", joined)
	return untag(t, joined)
}

func TestProbeNeverReportsAnEndBeforeAGapInTheStream(t *testing.T) {
	path := gappedTrackFixture(t, 960, 700, 30)
	meta, err := NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(meta.Audios) != 2 {
		t.Fatalf("fixture must carry two audio streams, got %d", len(meta.Audios))
	}
	gapped := meta.Audios[1]
	packets := testmedia.Packets(t, path, strconv.Itoa(gapped.Index))
	hole := 0.0
	for index := 1; index < len(packets); index++ {
		hole = math.Max(hole, packets[index].PTS-packets[index-1].PTS)
	}
	if hole < 600 {
		t.Fatalf("fixture track must have a hole of more than 600 s, longest is %.3f", hole)
	}
	want := measuredStreamEnd(t, path, gapped.Index)
	if want < 729 || math.Abs(gapped.End-want) > 0.001 {
		t.Fatalf("End = %.6f, the track resumes after its hole and its last packet ends at %.6f", gapped.End, want)
	}
}

func TestProbeKeepsDeclaredAudioEnds(t *testing.T) {
	path := testmedia.H264(t, testmedia.Spec{Container: "mp4", Duration: 6, Audio: []testmedia.AudioSpec{{Codec: "aac", Duration: 4}}})
	original := streamEndBudget
	defer func() { streamEndBudget = original }()
	streamEndBudget = time.Nanosecond
	meta, err := NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.Audios[0].End < 3.9 || meta.Audios[0].End > 4.1 {
		t.Fatalf("End = %.3f, the declared stream duration is 4 s and needs no measurement", meta.Audios[0].End)
	}
}

func TestStreamEndReadNeverSelectsAStream(t *testing.T) {
	for until, want := range map[float64]string{math.Inf(1): "2083.912000%", 2088.912: "2083.912000%2088.912000"} {
		args := streamEndArgs(2083.912, until)
		if slices.Contains(args, "-select_streams") {
			t.Fatalf("selecting a stream makes a Matroska interval read scan the whole file: %v", args)
		}
		if position := slices.Index(args, "-read_intervals"); position < 0 || args[position+1] != want {
			t.Fatalf("until %.3f: the read interval must be %s: %v", until, want, args)
		}
		if !slices.Contains(args, "packet=stream_index,pts_time,duration_time") {
			t.Fatalf("the read must report stream, time and duration per packet: %v", args)
		}
	}
}

func TestProbeMeasuresVideoEndWhenNothingDeclaresIt(t *testing.T) {
	path := untaggedFixture(t, testmedia.AudioSpec{Codec: "aac", Duration: 7.5})
	meta, err := NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if want := measuredStreamEnd(t, path, meta.Video.Index); math.Abs(meta.Video.End-want) > 0.001 {
		t.Fatalf("Video.End = %.6f, the last video packet ends at %.6f", meta.Video.End, want)
	}
}

func TestProbeLeavesEndsUnknownWhenTheMeasurementBudgetRunsOut(t *testing.T) {
	path := untaggedFixture(t, testmedia.AudioSpec{Codec: "aac", Duration: 7.5})
	original := streamEndBudget
	defer func() { streamEndBudget = original }()
	streamEndBudget = time.Nanosecond
	meta, err := NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("an exhausted measurement budget must not fail the probe: %v", err)
	}
	if meta.Audios[0].End != 0 {
		t.Fatalf("End = %.3f, nothing was measured so the end must stay unknown", meta.Audios[0].End)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	streamEndBudget = original
	if _, err := NewProber().ProbeUncached(cancelled, path); err == nil {
		t.Fatalf("a cancelled probe must fail")
	}
}

func TestProbeRecordsAudioSampleRate(t *testing.T) {
	for _, container := range []string{"mp4", "mkv"} {
		path := testmedia.H264(t, testmedia.Spec{Container: container, Duration: 2, Audio: []testmedia.AudioSpec{{Codec: "ac3", SampleRate: 44100}, {Codec: "eac3", SampleRate: 32000}, {Codec: "aac"}}})
		meta, err := NewProber().ProbeUncached(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: probe: %v", container, err)
		}
		var rates []int
		for _, audio := range meta.Audios {
			rates = append(rates, audio.SampleRate)
		}
		if !slices.Equal(rates, []int{44100, 32000, 48000}) {
			t.Errorf("%s: sample rates = %v, want [44100 32000 48000]", container, rates)
		}
	}
}
