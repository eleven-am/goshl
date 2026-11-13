package mkvindex

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestKeyframesDeclineSegmentLongerThanSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	path := filepath.Join(t.TempDir(), "cues_first.mkv")
	build := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=64x36:r=10", "-t", "4", "-c:v", "libx264", "-g", "5", "-reserve_index_space", "4096", path)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := KeyframesFrom(bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("intact source: %v", err)
	}
	padded := append(append([]byte{}, data...), make([]byte, 4096)...)
	if _, err := KeyframesFrom(bytes.NewReader(padded), int64(len(padded)), 0); err != nil {
		t.Fatalf("source with trailing bytes: %v", err)
	}
	tail := data[:len(data)-16]
	if keyframes, err := KeyframesFrom(bytes.NewReader(tail), int64(len(tail)), 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("source missing its last 16 bytes: keyframes %v err %v, want ErrUnsupported", keyframes, err)
	}
	cut := data[:len(data)*6/10]
	keyframes, err := KeyframesFrom(bytes.NewReader(cut), int64(len(cut)), 0)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("source cut to %d of %d bytes: keyframes %v err %v, want ErrUnsupported", len(cut), len(data), keyframes, err)
	}
}
