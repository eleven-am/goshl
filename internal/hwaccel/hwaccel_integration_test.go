package hwaccel

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s not found on PATH", binary)
		}
	}
}

func requireVideoToolbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("h264_videotoolbox is only available on darwin")
	}
	requireFFmpeg(t)
	encoders, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(encoders), "h264_videotoolbox") {
		t.Skip("ffmpeg lacks h264_videotoolbox")
	}
}

func encodeWithConfig(t *testing.T, cfg *domain.HWAccelConfig, source string, extra ...string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "out.ts")
	args := []string{"-v", "error", "-f", "lavfi", "-i", source}
	args = append(args, cfg.EncodeFlags...)
	args = append(args, "-vf", fmt.Sprintf(cfg.ScaleFilter, 854, 480), "-profile:v", "high", "-level:v", "3.1", "-b:v", "1400000", "-maxrate", "2100000", "-bufsize", "7000000")
	args = append(args, extra...)
	args = append(args, "-y", output)
	if combined, err := exec.CommandContext(context.Background(), "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("encode failed: %v\n%s", err, combined)
	}
	return output
}

func probeField(t *testing.T, path, entries string) string {
	t.Helper()
	output, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", entries, "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v", err)
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)[0])
}

func TestSoftwareConfigEncodesHighProfile(t *testing.T) {
	requireFFmpeg(t)
	cfg := NewConfig(domain.AccelNone)
	output := encodeWithConfig(t, cfg, "testsrc2=size=1280x720:rate=24:duration=1")
	if profile := probeField(t, output, "stream=profile"); profile != "High" {
		t.Fatalf("software pipeline encodes %q while the playlist advertises High (%v)", profile, cfg.EncodeFlags)
	}
}

func TestVideoToolboxEmitsOnlyForcedKeyframes(t *testing.T) {
	requireVideoToolbox(t)
	cfg := NewConfig(domain.AccelVideoToolbox)
	output := encodeWithConfig(t, cfg, "testsrc2=size=1280x720:rate=24:duration=6", cfg.KeyframeFlag, "0,3")
	flags := packetFlags(t, output)
	keyframes := 0
	for _, flag := range flags {
		if strings.HasPrefix(flag, "K") {
			keyframes++
		}
	}
	if len(flags) != 144 || keyframes != 2 {
		t.Fatalf("expected 144 frames with keyframes only at the 2 forced boundaries, got %d frames and %d keyframes (%v)", len(flags), keyframes, cfg.EncodeFlags)
	}
}

func packetFlags(t *testing.T, path string) []string {
	t.Helper()
	output, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=flags", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v", err)
	}
	return strings.Fields(string(output))
}
