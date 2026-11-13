package ffmpeg

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func rawSource(t *testing.T, size, rate, pixelFormat string, extra ...string) string {
	t.Helper()
	testmedia.Require(t)
	path := filepath.Join(t.TempDir(), "source.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-t", "0.5", "-i", fmt.Sprintf("testsrc2=size=%s:rate=%s", size, rate), "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", pixelFormat, "-g", "12"}
	args = append(args, extra...)
	if data, err := exec.Command("ffmpeg", append(args, path)...).CombinedOutput(); err != nil {
		t.Fatalf("create %s source: %v: %s", size, err, data)
	}
	return path
}

func encodedStream(t *testing.T, path string) (int, int, int) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,level", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var width, height, level int
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if _, err := fmt.Sscanf(strings.ReplaceAll(line, ",", " "), "%d %d %d", &width, &height, &level); err != nil {
		t.Fatalf("parse ffprobe output %q: %v", line, err)
	}
	return width, height, level
}

func probedLadder(t *testing.T, path string) (*domain.Metadata, []domain.VideoRendition) {
	t.Helper()
	meta, err := probe.NewProber().ProbeUncached(context.Background(), path)
	if err != nil {
		t.Fatalf("probe source: %v", err)
	}
	meta.Keyframes = nil
	ladder, _, err := rendition.GenerateVideo(meta, 2)
	if err != nil {
		t.Fatalf("generate ladder: %v", err)
	}
	return meta, ladder
}

func TestTranscodeOddHeightSource(t *testing.T) {
	path := rawSource(t, "320x182", "24", "yuv444p", "-vf", "format=yuv444p,crop=320:181:0:0:exact=1")
	meta, ladder := probedLadder(t, path)
	if meta.Video.Height != 181 || len(ladder) != 1 || ladder[0].Method != domain.Transcode {
		t.Fatalf("fixture must be an odd-height source with one transcoded rung, got %dx%d and %#v", meta.Video.Width, meta.Video.Height, ladder)
	}
	dir := t.TempDir()
	args, err := integrationBuilder("-preset", "veryfast").Video(VideoParams{InputURL: path, StreamIndex: meta.Video.Index, Rendition: ladder[0], Segments: []domain.Segment{{Index: 0, Start: 0, End: 0.5, Duration: 0.5}}, OutputDir: dir})
	if files := runJob(t, args, err); len(files) != 1 {
		t.Fatalf("job listed %v, want one segment", files)
	}
	width, height, level := encodedStream(t, filepath.Join(dir, "segment-00000.ts"))
	if width != 320 || height != 180 || level != ladder[0].Level {
		t.Fatalf("encoded %dx%d at level %d, want 320x180 at the rendition level %d", width, height, level, ladder[0].Level)
	}
}

func TestVideoToolboxEncodesEveryLadderRungAtItsLevel(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("h264_videotoolbox is only available on darwin")
	}
	testmedia.Require(t)
	encoders, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(encoders), "h264_videotoolbox") {
		t.Skip("ffmpeg lacks h264_videotoolbox")
	}
	builder := NewCommandBuilder(hwaccel.NewConfig(domain.AccelVideoToolbox))
	sources := []struct{ size, rate string }{
		{"1920x1080", "24000/1001"},
		{"1024x576", "25"},
		{"4096x2160", "24"},
	}
	for _, source := range sources {
		t.Run(source.size+"@"+strings.ReplaceAll(source.rate, "/", "_"), func(t *testing.T) {
			path := rawSource(t, source.size, source.rate, "yuv420p")
			meta, ladder := probedLadder(t, path)
			if len(ladder) < 2 {
				t.Fatalf("expected a ladder, got %#v", ladder)
			}
			for _, rung := range ladder {
				if rung.Method != domain.Transcode {
					t.Fatalf("fixture rung %s must transcode", rung.Name)
				}
				dir := t.TempDir()
				args, err := builder.Video(VideoParams{InputURL: path, StreamIndex: meta.Video.Index, Rendition: rung, Segments: []domain.Segment{{Index: 0, Start: 0, End: 0.5, Duration: 0.5}}, OutputDir: dir})
				if err != nil {
					t.Fatalf("build %s: %v", rung.Name, err)
				}
				if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
					t.Errorf("%s %dx%d at level %d: %v: %s", rung.Name, rung.Width, rung.Height, rung.Level, err, data)
					continue
				}
				width, height, level := encodedStream(t, filepath.Join(dir, "segment-00000.ts"))
				if width != rung.Width || height != rung.Height || level != rung.Level {
					t.Errorf("%s encoded %dx%d at level %d, the rendition advertises %dx%d at level %d", rung.Name, width, height, level, rung.Width, rung.Height, rung.Level)
				}
			}
		})
	}
}
