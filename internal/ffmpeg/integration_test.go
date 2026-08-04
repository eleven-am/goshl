package ffmpeg

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestSoftwareTranscodeCoversNonZeroTimestampSource(t *testing.T) {
	testNonZeroTimestampCoverage(t, domain.Transcode)
}

func TestDirectStreamCoversNonZeroTimestampSource(t *testing.T) {
	testNonZeroTimestampCoverage(t, domain.DirectStream)
}

func testNonZeroTimestampCoverage(t *testing.T, method domain.PlaybackMethod) {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable")
	}

	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "nonzero.ts")
	create := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-t", "4", "-c:v", "libx264", "-g", "25", "-keyint_min", "25", "-sc_threshold", "0",
		"-f", "mpegts", input,
	)
	if data, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, data)
	}

	outputDir := filepath.Join(tmpDir, "segments")
	if err := os.Mkdir(outputDir, 0755); err != nil {
		t.Fatalf("create output directory: %v", err)
	}
	builder := NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelNone,
		EncodeFlags:  []string{"-c:v", "libx264"},
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
	})
	args := builder.Video(VideoParams{
		InputURL:    input,
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: method, Width: 320, Height: 180, Bitrate: 500_000},
		Segments: []domain.Segment{
			{Index: 0, Start: 0, End: 2, Duration: 2},
			{Index: 1, Start: 2, End: 4, Duration: 2},
		},
		OutputDir: outputDir,
	})
	command := exec.Command(ffmpegPath, args...)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("process fixture with %s: %v: %s", method, err, data)
	}

	var totalDuration float64
	for index := 0; index < 2; index++ {
		segmentPath := filepath.Join(outputDir, fmt.Sprintf("segment-%05d.ts", index))
		data, err := exec.Command(ffprobePath, "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", segmentPath).Output()
		if err != nil {
			t.Fatalf("probe segment %d: %v", index, err)
		}
		duration, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			t.Fatalf("parse segment %d duration: %v", index, err)
		}
		totalDuration += duration
	}
	if totalDuration < 3.8 || totalDuration > 4.3 {
		t.Fatalf("%s segments cover %.3fs instead of the 4s source", method, totalDuration)
	}
	if method == domain.Transcode {
		segmentPath := filepath.Join(outputDir, "segment-00000.ts")
		data, err := exec.Command(ffprobePath, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=profile,pix_fmt", "-of", "csv=p=0", segmentPath).Output()
		if err != nil {
			t.Fatalf("probe transcoded video compatibility: %v", err)
		}
		compatibility := strings.ToLower(strings.TrimSpace(string(data)))
		if !strings.Contains(compatibility, "high") || !strings.Contains(compatibility, "yuv420p") {
			t.Fatalf("unexpected browser compatibility profile: %s", compatibility)
		}
	}
}
