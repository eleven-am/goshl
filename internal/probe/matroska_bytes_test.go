package probe

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func videoGroupBytes(t *testing.T, path string) []int64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=size,flags", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var groups []int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		size, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(fields[1], "K") || len(groups) == 0 {
			groups = append(groups, 0)
		}
		groups[len(groups)-1] += size
	}
	return groups
}

func TestProbeMatroskaCueIndexFillsKeyframeBytes(t *testing.T) {
	path := makeMedia(t, "bytes.mkv", "-bf", "2")
	meta, err := NewProber().Probe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	truth := videoGroupBytes(t, path)
	if len(meta.Keyframes) == 0 || len(meta.KeyframeBytes) != len(meta.Keyframes) || len(truth) != len(meta.Keyframes) {
		t.Fatalf("keyframes %d, bytes %d, truth %d", len(meta.Keyframes), len(meta.KeyframeBytes), len(truth))
	}
	for i, size := range meta.KeyframeBytes {
		if size < truth[i] {
			t.Fatalf("group %d: %d bytes, video holds %d", i, size, truth[i])
		}
	}
}

func TestProbeMatroskaWithoutRelativePositionsKeepsCueKeyframesAndNoBytes(t *testing.T) {
	source := makeMedia(t, "relative.mkv", "-bf", "2")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte{0xF7, 0x81, 0x01, 0xF1}
	stripped := 0
	for from := 0; ; {
		at := bytes.Index(data[from:], marker)
		if at < 0 {
			break
		}
		at += from + len(marker)
		at += 1 + int(data[at]&0x7F)
		if data[at] == 0xF0 {
			data[at] = 0xB2
			stripped++
		}
		from = at
	}
	if stripped == 0 {
		t.Fatal("fixture has no relative positions to strip")
	}
	path := filepath.Join(t.TempDir(), "plain.mkv")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := ffprobeCallLog(t)
	meta, err := NewProber().Probe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got := packetPassCount(t, logPath); got != 0 {
		t.Fatalf("packet pass ran %d times for a cued matroska file", got)
	}
	if len(meta.Keyframes) != 6 {
		t.Fatalf("keyframes %v, want the 6 cued ones", meta.Keyframes)
	}
	if len(meta.KeyframeBytes) != 0 {
		t.Fatalf("keyframe bytes %v, want none", meta.KeyframeBytes)
	}
}
