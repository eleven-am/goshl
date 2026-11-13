package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func probeFormat(t *testing.T, path string) (string, error) {
	t.Helper()
	input, err := InputArgs(path)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-v", "error", "-show_entries", "format=format_name", "-of", "csv=p=0"}, input...)
	output, err := exec.Command("ffprobe", args...).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func TestFormatWhitelistExcludesPlaylistDemuxers(t *testing.T) {
	for _, name := range strings.Split(FormatWhitelist, ",") {
		switch name {
		case "hls", "concat", "dash", "webm_dash_manifest", "sdp", "rtsp", "rtp", "sap", "image2", "ffmetadata", "lavfi", "tedcaptions":
			t.Errorf("format whitelist contains %q", name)
		}
	}
}

func TestScriptDisguisedAsMediaIsRejected(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "neighbour.mkv")
	build := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=64x36:r=10", "-t", "1", "-c:v", "libx264", real)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, output)
	}
	if format, err := probeFormat(t, real); err != nil || !strings.Contains(format, "matroska") {
		t.Fatalf("real media no longer probes: %q %v", format, err)
	}
	segment := filepath.Join(dir, "segment.ts")
	if output, err := exec.Command("ffmpeg", "-v", "error", "-y", "-i", real, "-c", "copy", "-f", "mpegts", segment).CombinedOutput(); err != nil {
		t.Fatalf("build segment: %v: %s", err, output)
	}
	scripts := map[string]string{
		"concat.mp4":    "ffconcat version 1.0\nfile 'neighbour.mkv'\n",
		"playlist.m3u8": "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\n" + segment + "\n#EXT-X-ENDLIST\n",
	}
	for name, body := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if format, err := probeFormat(t, path); err == nil {
			t.Errorf("%s (a %d byte text file) was opened as %q", name, len(body), format)
		}
	}
}

func TestRemoteSchemeIsLowerCasedForFFmpeg(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTP://Example.com/A.mp4?Q=1": "http://Example.com/A.mp4?Q=1",
		"Https://example.com/a.mp4":    "https://example.com/a.mp4",
	} {
		input, err := Resolve(raw)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", raw, err)
		}
		if input.Arg != want || !input.Remote {
			t.Errorf("Resolve(%q).Arg = %q, want %q", raw, input.Arg, want)
		}
	}
}
