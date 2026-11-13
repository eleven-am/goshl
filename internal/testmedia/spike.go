package testmedia

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

type SpikeSpec struct {
	Width      int
	Height     int
	Duration   float64
	SpikeStart float64
	SpikeEnd   float64
	GOPFrames  int
}

func Spike(t testing.TB, spec SpikeSpec) string {
	t.Helper()
	Require(t)
	if spec.GOPFrames == 0 {
		spec.GOPFrames = 50
	}
	output := filepath.Join(t.TempDir(), "spike.mp4")
	video := fmt.Sprintf("testsrc2=size=%dx%d:rate=25,noise=alls=40:allf=t:enable='between(t,%g,%g)'", spec.Width, spec.Height, spec.SpikeStart, spec.SpikeEnd)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", formatSeconds(spec.Duration), "-i", video,
		"-f", "lavfi", "-t", formatSeconds(spec.Duration), "-i", "sine=frequency=440:sample_rate=48000",
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-crf", "30",
		"-g", fmt.Sprint(spec.GOPFrames), "-keyint_min", fmt.Sprint(spec.GOPFrames), "-sc_threshold", "0",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-movflags", "+faststart", output,
	}
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create spike fixture: %v: %s", err, data)
	}
	return output
}
