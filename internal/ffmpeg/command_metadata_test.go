package ffmpeg

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

const sentinelTitle = "SENTINEL-TITLE"

func TestVODOutputsCarryNoSourceMetadata(t *testing.T) {
	fixture := newIntegrationFixture(t, "mkv")
	tagged := filepath.Join(t.TempDir(), "tagged.mkv")
	remux := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", fixture.path,
		"-map", "0", "-c", "copy",
		"-metadata", "title="+sentinelTitle,
		"-metadata:s:v:0", "title="+sentinelTitle,
		"-metadata:s:a:0", "title="+sentinelTitle,
		tagged)
	if out, err := remux.CombinedOutput(); err != nil {
		t.Fatalf("tag source: %v: %s", err, out)
	}
	source := exec.Command("ffprobe", "-v", "error", "-show_format", "-show_streams", tagged)
	if out, err := source.Output(); err != nil || !strings.Contains(string(out), sentinelTitle) {
		t.Fatalf("source must carry the sentinel: %v: %s", err, out)
	}

	end := fixture.segments[0].End
	segments := []domain.Segment{{Index: 0, Start: 0, End: end, Duration: end}}
	builder := integrationBuilder("-g", "60")
	cases := map[string]func(dir string) ([]string, error){
		"transcode": func(dir string) ([]string, error) {
			return builder.Video(VideoParams{InputURL: tagged, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 400000, PeakBitrate: 600000, BufferSize: 1200000, Level: 30}, Segments: segments, OutputDir: dir})
		},
		"copy": func(dir string) ([]string, error) {
			return builder.Video(VideoParams{InputURL: tagged, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: measureLanding(t, builder, tagged, 0, 0), OutputDir: dir})
		},
		"audio": func(dir string) ([]string, error) {
			return builder.Audio(AudioParams{InputURL: tagged, StreamIndex: 1, Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, Segments: segments, OutputDir: dir})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			args, err := build(dir)
			runJob(t, args, err)
			segment := filepath.Join(dir, "segment-00000.ts")
			out, err := exec.Command("ffprobe", "-v", "error", "-show_programs", "-show_format", "-show_streams", segment).Output()
			if err != nil {
				t.Fatalf("probe %s: %v", segment, err)
			}
			if strings.Contains(string(out), sentinelTitle) {
				t.Fatalf("segment leaks the source title:\n%s", out)
			}
			raw, err := exec.Command("ffmpeg", "-v", "error", "-i", segment, "-c", "copy", "-f", "null", "-").CombinedOutput()
			if err != nil {
				t.Fatalf("decode %s: %v: %s", segment, err, raw)
			}
		})
	}
}
