package testmedia

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type ColourSpec struct {
	Container      string
	Width          int
	Height         int
	PixelFormat    string
	Primaries      string
	Transfer       string
	Matrix         string
	Range          string
	KeyframeFrames []int
	Duration       float64
	FlatLuma10     int
	FlatLuma8      int
}

type ColourTags struct {
	PixelFormat string
	Range       string
	Matrix      string
	Transfer    string
	Primaries   string
}

func Colour(t testing.TB, spec ColourSpec) string {
	t.Helper()
	Require(t)
	base := withDefaults(Spec{Container: spec.Container, Width: spec.Width, Height: spec.Height, KeyframeFrames: spec.KeyframeFrames, Duration: spec.Duration})
	if spec.PixelFormat == "" {
		spec.PixelFormat = "yuv420p"
	}
	source := fmt.Sprintf("testsrc2=size=%dx%d:rate=%s", base.Width, base.Height, base.FPS)
	filters := []string{"format=" + spec.PixelFormat}
	if spec.FlatLuma10 > 0 {
		source = fmt.Sprintf("color=c=black:size=%dx%d:rate=%s", base.Width, base.Height, base.FPS)
		filters = []string{"format=yuv420p10le", fmt.Sprintf("geq=lum=%d:cb=512:cr=512", spec.FlatLuma10)}
	}
	if spec.FlatLuma8 > 0 {
		source = fmt.Sprintf("color=c=black:size=%dx%d:rate=%s", base.Width, base.Height, base.FPS)
		filters = []string{"format=yuv420p", fmt.Sprintf("geq=lum=%d:cb=128:cr=128", spec.FlatLuma8)}
	}
	var tags []string
	for name, value := range map[string]string{"color_primaries": spec.Primaries, "color_trc": spec.Transfer, "colorspace": spec.Matrix, "range": spec.Range} {
		if value != "" {
			tags = append(tags, name+"="+value)
		}
	}
	if len(tags) > 0 {
		filters = append(filters, "setparams="+strings.Join(tags, ":"))
	}
	output := filepath.Join(t.TempDir(), "source."+base.Container)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", formatSeconds(base.Duration),
		"-i", source,
		"-vf", strings.Join(filters, ","),
		"-c:v", "libx264", "-preset", "veryfast",
		"-x264-params", x264Params(base),
		"-force_key_frames", keyframeExpression(base.KeyframeFrames),
		output,
	}
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create colour fixture: %v: %s", err, data)
	}
	return output
}

func Tags(t testing.TB, path string) ColourTags {
	t.Helper()
	Require(t)
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt,color_range,color_space,color_transfer,color_primaries",
		"-of", "default=nw=1", path).Output()
	if err != nil {
		t.Fatalf("read colour tags of %s: %v", path, err)
	}
	var tags ColourTags
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "pix_fmt":
			tags.PixelFormat = value
		case "color_range":
			tags.Range = value
		case "color_space":
			tags.Matrix = value
		case "color_transfer":
			tags.Transfer = value
		case "color_primaries":
			tags.Primaries = value
		}
	}
	return tags
}
