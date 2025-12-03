package hwaccel

import (
	"context"
	"os"
	"path/filepath"
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
