package hwaccel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func fakeFilterList(t *testing.T, lines ...string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'LIST'\nFilters:\n  T.. = Timeline support\n" + strings.Join(lines, "\n") + "\nLIST\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDetectTonemapNeedsZscaleAndTonemap(t *testing.T) {
	scale := " .. scale             V->V       Scale the input video size and/or convert the image format."
	tonemap := " .S tonemap           V->V       Conversion to/from different dynamic ranges."
	zscale := " .S zscale            V->V       Apply resizing, colorspace and bit depth conversion."
	decoy := " .. tonemap_vaapi     V->V       VAAPI VPP for tone-mapping"

	fakeFilterList(t, scale, tonemap, zscale)
	filter, err := DetectTonemap(context.Background())
	if err != nil || !strings.HasPrefix(filter, "zscale=w=%d:h=%d:t=linear") || !strings.HasSuffix(filter, sdrTags) {
		t.Fatalf("complete build: filter %q, err %v", filter, err)
	}
	for name, lines := range map[string][]string{"zscale": {scale, tonemap, decoy}, "tonemap": {scale, zscale, decoy}} {
		fakeFilterList(t, lines...)
		_, err := DetectTonemap(context.Background())
		if !errors.Is(err, ErrTonemapUnavailable) || !strings.Contains(err.Error(), name) {
			t.Fatalf("build without %s: err %v", name, err)
		}
	}
}

func TestSystemMemoryConfigsNormaliseToTaggedSDR(t *testing.T) {
	for _, config := range []*domain.HWAccelConfig{NewConfig(domain.AccelNone), NewConfig(domain.AccelVideoToolbox), NewConfig(domain.AccelCUDA)} {
		for _, part := range []string{"out_color_matrix=bt709", "out_range=tv", "format=yuv420p", sdrTags} {
			if !strings.Contains(config.ScaleFilter, part) {
				t.Errorf("%s scale filter %q lacks %s", config.Accelerator, config.ScaleFilter, part)
			}
		}
	}
}
