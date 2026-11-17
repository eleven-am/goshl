package testmedia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

type LiveKind string

const (
	LiveH264AAC   LiveKind = "h264_aac"
	LiveH264MP2   LiveKind = "h264_mp2"
	LiveMPEG2MP2  LiveKind = "mpeg2i_mp2"
	LiveHEVCAC3   LiveKind = "hevc_ac3"
	LiveH264IAAC  LiveKind = "h264i_aac"
	liveSeconds            = "12"
	liveCacheName          = "goshl-testmedia-live-v1"
)

var liveMu sync.Mutex

func Live(t testing.TB, kind LiveKind) string {
	t.Helper()
	Require(t)
	args, ok := liveArgs(kind)
	if !ok {
		t.Fatalf("unknown live fixture %q", kind)
	}
	liveMu.Lock()
	defer liveMu.Unlock()
	dir := filepath.Join(os.TempDir(), liveCacheName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create live fixture cache: %v", err)
	}
	output := filepath.Join(dir, string(kind)+".ts")
	if info, err := os.Stat(output); err == nil && info.Size() > 0 {
		return output
	}
	partial := output + fmt.Sprintf(".%d.tmp", os.Getpid())
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	full = append(full, "-f", "mpegts", partial)
	if data, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		os.Remove(partial)
		t.Fatalf("create live fixture %s: %v: %s", kind, err, data)
	}
	if err := os.Rename(partial, output); err != nil {
		t.Fatalf("store live fixture %s: %v", kind, err)
	}
	return output
}

func liveArgs(kind LiveKind) ([]string, bool) {
	switch kind {
	case LiveH264AAC:
		return []string{
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-t", liveSeconds,
			"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "50", "-bf", "2",
			"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		}, true
	case LiveH264MP2:
		return []string{
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25",
			"-f", "lavfi", "-i", "sine=frequency=550:sample_rate=48000",
			"-t", liveSeconds,
			"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "50", "-bf", "2",
			"-c:a", "mp2", "-b:a", "192k", "-ac", "2",
		}, true
	case LiveMPEG2MP2:
		return []string{
			"-f", "lavfi", "-i", "testsrc2=size=720x576:rate=50",
			"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000",
			"-t", liveSeconds,
			"-vf", "interlace=scan=tff,setfield=tff,setsar=64/45",
			"-c:v", "mpeg2video", "-flags", "+ilme+ildct", "-b:v", "3000k", "-g", "12", "-bf", "2", "-pix_fmt", "yuv420p",
			"-c:a", "mp2", "-b:a", "192k", "-ac", "2", "-metadata:s:a:0", "language=fra",
		}, true
	case LiveH264IAAC:
		return []string{
			"-f", "lavfi", "-i", "testsrc2=size=720x576:rate=50",
			"-f", "lavfi", "-i", "sine=frequency=330:sample_rate=48000",
			"-t", liveSeconds,
			"-vf", "interlace=scan=tff,setfield=tff,setsar=64/45",
			"-c:v", "libx264", "-preset", "veryfast", "-flags", "+ilme+ildct", "-pix_fmt", "yuv420p", "-g", "25",
			"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		}, true
	case LiveHEVCAC3:
		return []string{
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=50",
			"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
			"-t", liveSeconds,
			"-c:v", "libx265", "-preset", "ultrafast", "-b:v", "2000k", "-g", "100", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p",
			"-c:a", "ac3", "-b:a", "384k", "-ac", "6",
		}, true
	}
	return nil, false
}
