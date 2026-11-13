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
	"time"

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

	if !strings.Contains(strings.Join(none.EncodeFlags, " "), "-preset superfast") {
		t.Fatalf("software flags must use superfast: %v", none.EncodeFlags)
	}
	if !strings.Contains(strings.Join(NewConfig(domain.AccelVideoToolbox).EncodeFlags, " "), "-g 250") {
		t.Fatalf("videotoolbox flags must cap the keyframe interval: %v", NewConfig(domain.AccelVideoToolbox).EncodeFlags)
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

const fakeFFmpegTryNextScript = `#!/bin/sh
if [ "$1" = "-hwaccels" ]; then
printf 'Hardware acceleration methods:\ncuda\nvideotoolbox\nvaapi\n'
exit 0
fi
if [ "$1" = "-encoders" ]; then
printf ' V..... h264_nvenc\n V..... h264_videotoolbox\n V..... h264_vaapi\n'
exit 0
fi
echo "$*" >> "$GOSHL_FAKE_LOG"
case "$*" in
*h264_nvenc*) %s;;
*h264_videotoolbox*) %s;;
*h264_vaapi*) %s;;
esac
exit 0
`

func installTryNextFFmpeg(t *testing.T, cuda, videotoolbox, vaapi string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := strings.Replace(fakeFFmpegTryNextScript, "%s", cuda, 1)
	script = strings.Replace(script, "%s", videotoolbox, 1)
	script = strings.Replace(script, "%s", vaapi, 1)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("GOSHL_FAKE_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestDetectBestFallsThroughToNextValidAccelerator(t *testing.T) {
	log := installTryNextFFmpeg(t, "echo 'nvenc: no capable devices found' >&2; exit 1", "exit 0", "exit 0")

	cfg := DetectBestContext(context.Background())
	if cfg.Accelerator != domain.AccelVideoToolbox || cfg.Encoder != "h264_videotoolbox" {
		t.Fatalf("expected videotoolbox after cuda failed validation, got %s (%s)", cfg.Accelerator, cfg.Error)
	}
	if cfg.Requested != domain.AccelCUDA {
		t.Fatalf("requested must stay the preferred accelerator, got %s", cfg.Requested)
	}
	if !strings.Contains(cfg.Error, "cuda") || !strings.Contains(cfg.Error, "no capable devices") {
		t.Fatalf("skipped accelerator failure must be reported, got %q", cfg.Error)
	}
	if len(cfg.Available) != 4 {
		t.Fatalf("available must list every detected accelerator, got %v", cfg.Available)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if strings.Contains(string(calls), "h264_vaapi") {
		t.Fatalf("validation must stop at the first accelerator that passes, calls:\n%s", calls)
	}
}

func TestDetectBestJoinsEveryValidationFailure(t *testing.T) {
	installTryNextFFmpeg(t, "echo 'nvenc broken' >&2; exit 1", "echo 'vt broken' >&2; exit 1", "echo 'vaapi broken' >&2; exit 1")

	cfg := DetectBestContext(context.Background())
	if cfg.Accelerator != domain.AccelNone || cfg.Encoder != "libx264" {
		t.Fatalf("expected software fallback, got %s", cfg.Accelerator)
	}
	if cfg.Requested != domain.AccelCUDA {
		t.Fatalf("requested must stay the preferred accelerator, got %s", cfg.Requested)
	}
	for _, want := range []string{"cuda", "nvenc broken", "videotoolbox", "vt broken", "vaapi", "vaapi broken"} {
		if !strings.Contains(cfg.Error, want) {
			t.Fatalf("error must report %q, got %q", want, cfg.Error)
		}
	}
}

func TestDetectBestBoundsEachCandidate(t *testing.T) {
	installTryNextFFmpeg(t, "exec sleep 30", "exit 0", "exit 0")
	previous := validationTimeout
	validationTimeout = 300 * time.Millisecond
	t.Cleanup(func() { validationTimeout = previous })

	started := time.Now()
	cfg := DetectBestContext(context.Background())
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("a hung candidate must be bounded by its own timeout, took %s", elapsed)
	}
	if cfg.Accelerator != domain.AccelVideoToolbox {
		t.Fatalf("expected videotoolbox after cuda timed out, got %s (%s)", cfg.Accelerator, cfg.Error)
	}
	if !strings.Contains(cfg.Error, "cuda") {
		t.Fatalf("timed out accelerator must be reported, got %q", cfg.Error)
	}
}

func TestDetectBestStopsWhenContextIsCancelled(t *testing.T) {
	log := installTryNextFFmpeg(t, "exit 0", "exit 0", "exit 0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := DetectBestContext(ctx)
	if cfg.Accelerator != domain.AccelNone || cfg.Error == "" {
		t.Fatalf("cancelled detection must fall back to software with a reason, got %s %q", cfg.Accelerator, cfg.Error)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "h264_") {
		t.Fatalf("no candidate may be validated after cancellation, calls:\n%s", calls)
	}
}

func TestCandidatesFollowPriorityOrder(t *testing.T) {
	got := Candidates([]domain.Accelerator{domain.AccelVAAPI, domain.AccelNone, domain.AccelVideoToolbox, domain.AccelQSV, domain.AccelCUDA})
	want := []domain.Accelerator{domain.AccelCUDA, domain.AccelQSV, domain.AccelVideoToolbox, domain.AccelVAAPI}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}
