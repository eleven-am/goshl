package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func scanFixture(t *testing.T, name string, extra ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	path := filepath.Join(t.TempDir(), name)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=64x36:r=10", "-f", "lavfi", "-i", "sine=r=8000", "-t", "4", "-c:v", "libx264", "-g", "5", "-bf", "2", "-c:a", "aac"}
	args = append(append(args, extra...), path)
	if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func probeBytes(t *testing.T, name string, data []byte) (float64, float64, []float64, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := NewProber().Probe(context.Background(), path)
	if err != nil {
		return 0, 0, nil, err
	}
	audioEnd := 0.0
	if len(meta.Audios) > 0 {
		audioEnd = meta.Audios[0].End
	}
	return meta.Duration, audioEnd, meta.Keyframes, nil
}

func TestTruncatedSourceDurationStopsAtTheLastReadablePacket(t *testing.T) {
	cases := map[string][]string{
		"cues_last.mkv":  nil,
		"cues_first.mkv": {"-reserve_index_space", "4096"},
		"faststart.mp4":  {"-movflags", "+faststart"},
	}
	for name, extra := range cases {
		data := scanFixture(t, name, extra...)
		full, _, fullKeyframes, err := probeBytes(t, name, data)
		if err != nil {
			t.Fatalf("%s intact: %v", name, err)
		}
		if full < 3.9 || len(fullKeyframes) != 8 {
			t.Fatalf("%s intact: duration %v keyframes %v", name, full, fullKeyframes)
		}
		duration, audioEnd, keyframes, err := probeBytes(t, name, data[:len(data)*6/10])
		if err != nil {
			t.Fatalf("%s cut to 60%%: %v", name, err)
		}
		if duration > 3.2 || audioEnd > duration {
			t.Errorf("%s cut to 60%%: duration %.3f (intact %.3f), audio end %.3f", name, duration, full, audioEnd)
		}
		if len(keyframes) == 0 || len(keyframes) >= len(fullKeyframes) || keyframes[len(keyframes)-1] > duration {
			t.Errorf("%s cut to 60%%: keyframes %v for duration %.3f", name, keyframes, duration)
		}
	}
}

func TestSourceWithNoReadableVideoPacketIsRejected(t *testing.T) {
	data := scanFixture(t, "cues_last.mkv")
	cluster := strings.Index(string(data), "\x1f\x43\xb6\x75")
	if cluster < 0 {
		t.Fatal("fixture has no cluster")
	}
	duration, _, keyframes, err := probeBytes(t, "headers_only.mkv", data[:cluster+8])
	if err == nil {
		t.Fatalf("headers without a single packet were accepted: duration %.3f keyframes %v", duration, keyframes)
	}
}
