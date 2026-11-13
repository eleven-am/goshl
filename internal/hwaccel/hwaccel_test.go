package hwaccel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestDetectParsesFakeFFmpegOutput(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegDetectScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	accels, err := Detect(context.Background())
	if err != nil {
		t.Fatalf("detect failed: %v", err)
	}

	found := func(target string) bool {
		for _, a := range accels {
			if string(a) == target {
				return true
			}
		}
		return false
	}

	if !found("cuda") || !found("videotoolbox") || !found("none") {
		t.Fatalf("expected accelerators in list, got %v", accels)
	}
}

func TestSelectPrefersPriorityOrder(t *testing.T) {
	accels := []domain.Accelerator{domain.AccelVideoToolbox, domain.AccelCUDA}
	if sel := Select(accels); sel != domain.AccelCUDA {
		t.Fatalf("expected cuda selected, got %s", sel)
	}

	if sel := Select([]domain.Accelerator{domain.AccelVAAPI}); sel != domain.AccelVAAPI {
		t.Fatalf("expected vaapi when only option, got %s", sel)
	}
}

func TestNewConfigReturnsExpectedFlags(t *testing.T) {
	cfg := NewConfig(domain.AccelQSV)
	joined := strings.Join(cfg.EncodeFlags, " ")
	if !strings.Contains(joined, "h264_qsv") {
		t.Fatalf("expected qsv encoder in flags: %v", cfg.EncodeFlags)
	}

	none := NewConfig(domain.Accelerator("unknown"))
	if none.Accelerator != "none" || none.Encoder != "libx264" {
		t.Fatalf("fallback config unexpected: %#v", none)
	}
	if !strings.Contains(none.ScaleFilter, "format=yuv420p") {
		t.Fatalf("software output must use browser-compatible 8-bit 4:2:0 video: %s", none.ScaleFilter)
	}

	cuda := NewConfig(domain.AccelCUDA)
	if cuda.KeyframeFlag != "-force_key_frames" {
		t.Fatalf("unexpected CUDA keyframe flag %q", cuda.KeyframeFlag)
	}
	if strings.Contains(strings.Join(cuda.DecodeFlags, " "), "hwaccel_output_format") || cuda.DeviceFrames {
		t.Fatalf("safe CUDA mode must transfer decoded frames through host memory: %#v", cuda)
	}
	device := NewConfigWithMode(domain.AccelCUDA, domain.DecodeDevice)
	if !strings.Contains(strings.Join(device.DecodeFlags, " "), "hwaccel_output_format cuda") || !device.DeviceFrames {
		t.Fatalf("device CUDA mode must retain frames on the GPU: %#v", device)
	}
}

func TestRequireConfigRejectsUnknownAccelerator(t *testing.T) {
	if _, err := RequireConfig(domain.Accelerator("bad")); err == nil {
		t.Fatalf("expected unknown accelerator error")
	}
}

func TestValidationDimensionsAreNVENCSafe(t *testing.T) {
	if validationWidth < 320 || validationHeight < 180 {
		t.Fatalf("hardware validation dimensions are too small: %dx%d", validationWidth, validationHeight)
	}
	filter := fmt.Sprintf(NewConfig(domain.AccelCUDA).ScaleFilter, validationWidth, validationHeight)
	if !strings.Contains(filter, "scale=320:180") {
		t.Fatalf("unexpected validation filter: %s", filter)
	}
}

func TestDetectBestExposesDetectionError(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	cfg := DetectBest()
	if cfg.Accelerator != domain.AccelNone {
		t.Fatalf("expected software fallback, got %s", cfg.Accelerator)
	}
	if cfg.Error == "" {
		t.Fatalf("expected detection error to be exposed")
	}
}

const fakeFFmpegDetectScript = `#!/bin/sh
if [ "$1" = "-hwaccels" ]; then
cat <<'EOF'
Hardware acceleration methods:
cuda
videotoolbox
EOF
exit 0
fi

if [ "$1" = "-encoders" ]; then
cat <<'EOF'
------ encoders -----
V..... h264_nvenc NVENC H.264 encoder
V..... h264_videotoolbox VideoToolbox H.264 encoder
EOF
exit 0
fi

exit 1
`

func TestValidateExercisesLevelPolicyBounds(t *testing.T) {
	tmp := t.TempDir()
	log := filepath.Join(tmp, "calls.log")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"*\"-level:v 5.2\"*) echo 'Invalid Profile/Level' >&2; exit 1;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(tmp, "ffmpeg"), []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := Validate(context.Background(), NewConfig(domain.AccelVideoToolbox))
	if err == nil {
		t.Fatalf("expected validation to fail when the ceiling level is rejected")
	}
	if !strings.Contains(err.Error(), "5.2") {
		t.Fatalf("expected error to name the rejected level, got %v", err)
	}
	recorded, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatalf("read call log: %v", readErr)
	}
	calls := string(recorded)
	for _, want := range []string{"-profile:v high -level:v 3.0", "-profile:v high -level:v 5.2"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("expected an encode with %q, calls:\n%s", want, calls)
		}
	}
}

func TestVideoToolboxEncodesPolicyLevels(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("h264_videotoolbox is only available on darwin")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH")
	}
	encoders, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(encoders), "h264_videotoolbox") {
		t.Skip("ffmpeg lacks h264_videotoolbox")
	}

	if strings.Join(validationLevels, ",") != "3.0,5.2" {
		t.Fatalf("validation levels = %v, want the policy levels 3.0 and 5.2", validationLevels)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	wrapper := "#!/bin/sh\necho \"$*\" >> \"" + logPath + "\"\nexec \"" + ffmpeg + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(wrapper), 0o755); err != nil {
		t.Fatalf("write ffmpeg wrapper: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := NewConfig(domain.AccelVideoToolbox)
	if err := Validate(context.Background(), cfg); err != nil {
		t.Fatalf("videotoolbox rejected the policy levels: %v", err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	invocations := strings.Split(strings.TrimSpace(string(logged)), "\n")
	for _, level := range validationLevels {
		found := false
		for _, invocation := range invocations {
			if strings.Contains(invocation, "-profile:v high -level:v "+level+" ") {
				found = true
			}
		}
		if !found {
			t.Fatalf("Validate never encoded at -level:v %s; invocations:\n%s", level, logged)
		}
	}

	broken := NewConfig(domain.AccelVideoToolbox)
	broken.EncodeFlags = []string{"-c:v", "goshl_missing_encoder"}
	if err := Validate(context.Background(), broken); err == nil {
		t.Fatal("Validate must fail for a pipeline whose encoder cannot run")
	}
}
