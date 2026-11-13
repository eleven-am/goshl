package probe

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/testmedia"
)

func remuxed(t *testing.T, source, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	command := append(append([]string{"-hide_banner", "-loglevel", "error", "-y", "-i", source, "-map", "0", "-c", "copy"}, args...), path)
	if data, err := exec.Command("ffmpeg", command...).CombinedOutput(); err != nil {
		t.Fatalf("remux fixture: %v: %s", err, data)
	}
	return path
}

func cutTo(t *testing.T, source string, percent int) string {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "cut"+filepath.Ext(source))
	if err := os.WriteFile(path, data[:len(data)*percent/100], 0o644); err != nil {
		t.Fatalf("write cut fixture: %v", err)
	}
	return path
}

func lastReadableEnd(t *testing.T, path string) float64 {
	t.Helper()
	end := 0.0
	for _, packet := range testmedia.Packets(t, path, "a:0") {
		end = math.Max(end, packet.PTS+packet.Duration)
	}
	return end - testmedia.FormatStart(t, path)
}

func TestProbeFindsAStreamEndBehindALongKeyframeInterval(t *testing.T) {
	testmedia.Require(t)
	source := filepath.Join(t.TempDir(), "long_gop.mkv")
	build := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-t", "120", "-i", "testsrc2=size=160x90:rate=25", "-f", "lavfi", "-t", "120", "-i", "sine=frequency=440",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "2000", "-keyint_min", "2000", "-sc_threshold", "0", "-c:a", "aac", source}
	if data, err := exec.Command("ffmpeg", build...).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, data)
	}
	path := untag(t, remuxed(t, source, "short_audio.mkv", "-bsf:a", "noise=drop=gt(pts*tb\\,40)"))
	keyframes := 0
	for _, packet := range testmedia.Packets(t, path, "v:0") {
		if packet.Key {
			keyframes++
		}
	}
	if keyframes > 2 {
		t.Fatalf("fixture must keep its keyframes 80 s apart, it has %d", keyframes)
	}
	meta, err := NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if want := measuredStreamEnd(t, path, meta.Audios[0].Index); want < 39 || math.Abs(meta.Audios[0].End-want) > 0.001 {
		t.Fatalf("End = %.6f, the last audio packet ends at %.6f", meta.Audios[0].End, want)
	}
}

func TestProbeDoesNotTrustFilledInStreamTimings(t *testing.T) {
	testmedia.Require(t)
	source := longSource(t, 60, testmedia.AudioSpec{Codec: "aac"})
	late := []string{"-bsf:a", "noise=drop=lt(pts*tb\\,30)+gt(pts*tb\\,45)"}
	matroska := remuxed(t, source, "late.mkv", late...)
	data, err := os.ReadFile(matroska)
	if err != nil {
		t.Fatal(err)
	}
	ends := strings.TrimSpace(ffprobeField(t, matroska, "a:0", "stream_tags=DURATION"))
	if len(ends) != len("00:00:15.000000000") || bytes.Count(data, []byte(ends)) != 1 {
		t.Fatalf("fixture must carry one audio end tag, ffprobe reports %q", ends)
	}
	data = bytes.Replace(data, []byte(ends), []byte("00:00:15.000000000"), 1)
	foreign := filepath.Join(t.TempDir(), "foreign.mkv")
	if err := os.WriteFile(foreign, bytes.ReplaceAll(data, []byte("Lavf"), []byte("Xavf")), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]string{
		"matroska with end tags":   matroska,
		"matroska with a span tag": foreign,
		"mpegts":                   remuxed(t, source, "late.ts", late...),
	}
	for name, path := range fixtures {
		meta, err := NewProber().ProbeUncached(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: probe: %v", name, err)
		}
		want := measuredStreamEnd(t, path, meta.Audios[0].Index)
		if want > 46 || math.Abs(meta.Audios[0].End-want) > 0.1 {
			t.Errorf("%s: End = %.3f, the audio starts at 30 s and its last packet ends at %.3f", name, meta.Audios[0].End, want)
		}
	}
}

func TestProbeMeasuresTransportStreamAudioEnds(t *testing.T) {
	source := longSource(t, 30, testmedia.AudioSpec{Codec: "aac"})
	full := remuxed(t, source, "full.ts")
	videoShort := remuxed(t, source, "short.ts", "-bsf:v", "noise=drop=gt(pts*tb\\,10)")
	for name, path := range map[string]string{"audio as long as the video": full, "audio outlasts the video": videoShort} {
		meta, err := NewProber().ProbeUncached(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: probe: %v", name, err)
		}
		want := measuredStreamEnd(t, path, meta.Audios[0].Index)
		if math.Abs(meta.Audios[0].End-want) > 0.001 {
			t.Errorf("%s: End = %.6f, the last audio packet ends at %.6f", name, meta.Audios[0].End, want)
		}
		if meta.Duration < want-0.001 {
			t.Errorf("%s: Duration = %.6f ends before the audio at %.6f", name, meta.Duration, want)
		}
	}
}

func TestProbeStopsAtTheLastReadableAudioPacket(t *testing.T) {
	testmedia.Require(t)
	dir := t.TempDir()
	tone := func(name string, args ...string) string {
		path := filepath.Join(dir, name)
		command := append(append([]string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-t", "120", "-i", "sine=frequency=440"}, args...), path)
		if data, err := exec.Command("ffmpeg", command...).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", name, err, data)
		}
		return path
	}
	source := longSource(t, 60, testmedia.AudioSpec{Codec: "aac"})
	videoShort := []string{"-bsf:v", "noise=drop=gt(pts*tb\\,20)"}
	cases := map[string]struct {
		path    string
		percent int
	}{
		"audio-only mp4":                 {tone("a.m4a", "-c:a", "aac", "-movflags", "+faststart"), 60},
		"audio-only matroska":            {tone("a.mka", "-c:a", "aac"), 60},
		"audio-only mp3":                 {tone("a.mp3", "-c:a", "libmp3lame"), 60},
		"matroska cut after the video":   {remuxed(t, source, "v.mkv", videoShort...), 95},
		"fast-start mp4 after the video": {remuxed(t, source, "v.mp4", append(videoShort, "-movflags", "+faststart")...), 95},
	}
	for name, c := range cases {
		whole, err := NewProber().ProbeUncached(context.Background(), c.path)
		if err != nil {
			t.Fatalf("%s: probe intact source: %v", name, err)
		}
		if want := lastReadableEnd(t, c.path); math.Abs(whole.Duration-want) > 0.1 {
			t.Errorf("%s: intact Duration = %.3f, the audio ends at %.3f", name, whole.Duration, want)
		}
		cut := cutTo(t, c.path, c.percent)
		meta, err := NewProber().ProbeUncached(context.Background(), cut)
		if err != nil {
			t.Fatalf("%s: probe cut source: %v", name, err)
		}
		want := lastReadableEnd(t, cut)
		if want > whole.Duration-3 {
			t.Fatalf("%s: fixture lost only %.3f s", name, whole.Duration-want)
		}
		if math.Abs(meta.Duration-want) > 0.1 || math.Abs(meta.Audios[0].End-want) > 0.1 {
			t.Errorf("%s: Duration = %.3f and audio End = %.3f, the last readable packet ends at %s", name, meta.Duration, meta.Audios[0].End, strconv.FormatFloat(want, 'f', 3, 64))
		}
	}
}
