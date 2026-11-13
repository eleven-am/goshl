package testmedia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type AudioSpec struct {
	Codec    string
	Delay    float64
	Duration float64
}

type Spec struct {
	Container      string
	Width          int
	Height         int
	FPS            string
	BFrames        int
	Pyramid        bool
	OpenGOP        bool
	KeyframeFrames []int
	Duration       float64
	Audio          []AudioSpec
	Subtitles      bool
	StartOffset    float64
}

type Packet struct {
	PTS      float64
	DTS      float64
	Duration float64
	Key      bool
}

func Require(t testing.TB) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is unavailable: %v", tool, err)
		}
	}
}

func H264(t testing.TB, spec Spec) string {
	t.Helper()
	Require(t)
	spec = withDefaults(spec)
	dir := t.TempDir()
	output := filepath.Join(dir, "source."+spec.Container)

	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", formatSeconds(spec.Duration),
		"-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%s", spec.Width, spec.Height, spec.FPS),
	}
	for index, audio := range spec.Audio {
		duration := audio.Duration
		if duration <= 0 {
			duration = spec.Duration
		}
		if audio.Delay != 0 {
			args = append(args, "-itsoffset", formatSeconds(audio.Delay))
		}
		args = append(args,
			"-f", "lavfi", "-t", formatSeconds(duration),
			"-i", fmt.Sprintf("sine=frequency=%d:sample_rate=48000", 440+110*index),
		)
	}
	if spec.Subtitles {
		subtitles := filepath.Join(dir, "source.srt")
		if err := os.WriteFile(subtitles, []byte(subtitleScript(spec.Duration)), 0o644); err != nil {
			t.Fatalf("write subtitle fixture: %v", err)
		}
		args = append(args, "-i", subtitles)
	}

	args = append(args, "-map", "0:v")
	for index := range spec.Audio {
		args = append(args, "-map", fmt.Sprintf("%d:a", index+1))
	}
	if spec.Subtitles {
		args = append(args, "-map", fmt.Sprintf("%d:s", len(spec.Audio)+1))
	}

	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-bf", strconv.Itoa(spec.BFrames),
		"-x264-params", x264Params(spec),
		"-force_key_frames", keyframeExpression(spec.KeyframeFrames),
	)
	for index, audio := range spec.Audio {
		args = append(args, fmt.Sprintf("-c:a:%d", index), audio.Codec, fmt.Sprintf("-b:a:%d", index), "128k", fmt.Sprintf("-ac:a:%d", index), "2")
	}
	if spec.Subtitles {
		codec := "srt"
		if spec.Container == "mp4" || spec.Container == "mov" {
			codec = "mov_text"
		}
		args = append(args, "-c:s", codec)
	}
	if spec.StartOffset != 0 {
		args = append(args, "-output_ts_offset", formatSeconds(spec.StartOffset))
	}
	if spec.Container == "mp4" || spec.Container == "mov" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, output)

	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create %s fixture: %v: %s", spec.Container, err, data)
	}
	return output
}

func Packets(t testing.TB, path, selector string) []Packet {
	t.Helper()
	Require(t)
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", selector,
		"-show_entries", "packet=pts_time,dts_time,duration_time,flags",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		t.Fatalf("read packets of %s (%s): %v", path, selector, err)
	}
	var packets []Packet
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 4 {
			continue
		}
		pts, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		dts, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			dts = pts
		}
		duration, _ := strconv.ParseFloat(fields[2], 64)
		packets = append(packets, Packet{PTS: pts, DTS: dts, Duration: duration, Key: strings.Contains(fields[3], "K")})
	}
	return packets
}

func FormatStart(t testing.TB, path string) float64 {
	t.Helper()
	Require(t)
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=start_time", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("read start time of %s: %v", path, err)
	}
	start, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parse start time of %s: %v", path, err)
	}
	return start
}

func withDefaults(spec Spec) Spec {
	if spec.Container == "" {
		spec.Container = "mp4"
	}
	if spec.Width == 0 {
		spec.Width = 320
	}
	if spec.Height == 0 {
		spec.Height = 180
	}
	if spec.FPS == "" {
		spec.FPS = "24000/1001"
	}
	if spec.Duration == 0 {
		spec.Duration = 10
	}
	if len(spec.KeyframeFrames) == 0 {
		spec.KeyframeFrames = []int{0}
	}
	return spec
}

func x264Params(spec Spec) string {
	pyramid := "none"
	if spec.Pyramid {
		pyramid = "normal"
	}
	openGOP := 0
	if spec.OpenGOP {
		openGOP = 1
	}
	return fmt.Sprintf("keyint=100000:min-keyint=1:scenecut=0:open-gop=%d:b-pyramid=%s", openGOP, pyramid)
}

func keyframeExpression(frames []int) string {
	terms := make([]string, 0, len(frames))
	for _, frame := range frames {
		terms = append(terms, fmt.Sprintf("eq(n,%d)", frame))
	}
	return "expr:" + strings.Join(terms, "+")
}

func subtitleScript(duration float64) string {
	var script strings.Builder
	cue := 1
	for start := 1.0; start+1 < duration; start += 4 {
		fmt.Fprintf(&script, "%d\n%s --> %s\ncue %d\n\n", cue, srtTime(start), srtTime(start+1), cue)
		cue++
	}
	return script.String()
}

func srtTime(seconds float64) string {
	millis := int64(seconds*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", millis/3600000, millis/60000%60, millis/1000%60, millis%1000)
}

func formatSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 6, 64)
}
