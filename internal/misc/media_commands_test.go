package misc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/coordination/coordtest"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/source"
	"github.com/eleven-am/goshl/internal/testmedia"
)

type recordedCommand struct {
	tool string
	args []string
}

type mediaCommandRun struct {
	name   string
	err    error
	output string
}

func installRecordingTools(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh is unavailable: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "commands.log")
	binDir := t.TempDir()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is unavailable: %v", tool, err)
		}
		body := fmt.Sprintf("#!/bin/sh\n{ printf 'CMD %%s\\n' %q; printf 'ARG %%s\\n' \"$@\"; } >> %q\nexec %q \"$@\"\n", tool, logPath, real)
		if err := os.WriteFile(filepath.Join(binDir, tool), []byte(body), 0o755); err != nil {
			t.Fatalf("write recording %s: %v", tool, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readRecordedCommands(t *testing.T, logPath string) []recordedCommand {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read command log: %v", err)
	}
	var commands []recordedCommand
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "CMD "):
			commands = append(commands, recordedCommand{tool: strings.TrimPrefix(line, "CMD ")})
		case strings.HasPrefix(line, "ARG "):
			last := &commands[len(commands)-1]
			last.args = append(last.args, strings.TrimPrefix(line, "ARG "))
		}
	}
	return commands
}

func awkwardMediaFixture(t *testing.T) string {
	t.Helper()
	original := testmedia.H264(t, testmedia.Spec{
		Container: "mkv",
		Duration:  6,
		Audio:     []testmedia.AudioSpec{{Codec: "aac"}},
		Subtitles: true,
	})
	cueless := filepath.Join(t.TempDir(), "cueless.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-i", original, "-map", "0", "-c", "copy", "-live", "1", cueless).CombinedOutput(); err != nil {
		t.Fatalf("remux fixture without cues: %v: %s", err, out)
	}
	data, err := os.ReadFile(cueless)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "My Show: Part 1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create fixture dir: %v", err)
	}
	path := filepath.Join(dir, "episode 1: pilot.mkv")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func runEveryMediaCommand(t *testing.T) (string, []mediaCommandRun, []recordedCommand) {
	t.Helper()
	testmedia.Require(t)
	path := awkwardMediaFixture(t)
	logPath := installRecordingTools(t)
	sourceURL := "file:" + path
	ctx := context.Background()
	hw := &domain.HWAccelConfig{
		Accelerator:  domain.AccelNone,
		DecodeFlags:  []string{"-hwaccel", "none"},
		EncodeFlags:  []string{"-c:v", "libx264"},
		Encoder:      "libx264",
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d",
	}
	builder := ffmpeg.NewCommandBuilder(hw)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 2, Duration: 2}, {Index: 1, Start: 2, End: 4, Duration: 2}}
	var runs []mediaCommandRun
	execute := func(name string, args []string, err error) {
		run := mediaCommandRun{name: name, err: err}
		if err == nil {
			out, runErr := exec.Command("ffmpeg", args...).Output()
			run.err = runErr
			run.output = string(out)
		}
		runs = append(runs, run)
	}

	prober := probe.NewProber()
	if _, err := prober.ProbeUncached(ctx, sourceURL); err != nil {
		runs = append(runs, mediaCommandRun{name: "probe", err: err})
	} else {
		runs = append(runs, mediaCommandRun{name: "probe"})
	}

	landing, err := builder.SeekLanding(ctx, sourceURL, 0, 0)
	runs = append(runs, mediaCommandRun{name: "seek landing", err: err})

	copyArgs, err := builder.Video(ffmpeg.VideoParams{InputURL: sourceURL, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: &landing, OutputDir: t.TempDir()})
	execute("video copy", copyArgs, err)

	transcodeArgs, err := builder.Video(ffmpeg.VideoParams{InputURL: sourceURL, StreamIndex: 0, Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 160, Height: 90, Bitrate: 200000, PeakBitrate: 300000, BufferSize: 600000, Level: 30}, Segments: segments, OutputDir: t.TempDir()})
	execute("video transcode", transcodeArgs, err)

	audioArgs, err := builder.Audio(ffmpeg.AudioParams{InputURL: sourceURL, StreamIndex: 1, Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, Segments: segments, OutputDir: t.TempDir()})
	execute("audio", audioArgs, err)

	generator := NewGeneratorWithCoordinator(newCapturingVTTStore(), coordtest.NewMemCoordinator(), nil)
	spriteArgs, err := generator.spriteArgs(sourceURL, filepath.Join(t.TempDir(), "sprite-%d.jpg"), 6)
	execute("sprite", spriteArgs, err)

	thumbnailArgs, err := generator.thumbnailArgs(sourceURL, 6, domain.ThumbnailConfig{Position: 0.5, Width: 160})
	execute("thumbnail", thumbnailArgs, err)

	failures := map[string]error{}
	subtitleErr := generator.extractSubtitleBatchFFmpeg(ctx, sourceURL, []SubtitleJob{{Key: "m", TrackID: "en", StreamIndex: 2, Codec: "subrip"}}, failures)
	if subtitleErr == nil {
		subtitleErr = failures["en"]
	}
	runs = append(runs, mediaCommandRun{name: "subtitle", err: subtitleErr})

	return path, runs, readRecordedCommands(t, logPath)
}

func TestEveryMediaCommandCarriesProtocolWhitelist(t *testing.T) {
	path, _, commands := runEveryMediaCommand(t)
	want := []string{"-protocol_whitelist", source.ProtocolWhitelist, "-i", "file:" + path}
	seen := map[string]int{}
	for _, command := range commands {
		seen[command.tool]++
		found := false
		for index := 0; index+len(want) <= len(command.args); index++ {
			if strings.Join(command.args[index:index+len(want)], "\x00") == strings.Join(want, "\x00") {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s %q lacks %q", command.tool, command.args, want)
		}
	}
	if seen["ffprobe"] != 3 || seen["ffmpeg"] < 7 {
		t.Fatalf("recorded invocations = %v, want 3 ffprobe (streams, keyframes, stream ends) and at least 7 ffmpeg", seen)
	}
}

func TestEveryMediaCommandRunsWithWhitelistedFileInput(t *testing.T) {
	_, runs, _ := runEveryMediaCommand(t)
	if len(runs) != 8 {
		t.Fatalf("ran %d commands, want 8", len(runs))
	}
	for _, run := range runs {
		if run.err != nil {
			t.Errorf("%s: %v", run.name, run.err)
		}
		if run.name == "video copy" || run.name == "video transcode" || run.name == "audio" {
			if !strings.Contains(run.output, "segment-") {
				t.Errorf("%s: segment_list pipe:1 output = %q", run.name, run.output)
			}
		}
	}
}

func TestUnsupportedSourceFailsBeforeAnyProcessStarts(t *testing.T) {
	logPath := installRecordingTools(t)
	ctx := context.Background()
	generator := NewGenerator(&stubStorage{})
	builder := ffmpeg.NewCommandBuilder(&domain.HWAccelConfig{Accelerator: domain.AccelNone})
	for _, raw := range []string{"concat:a|b", "pipe:0", "rtmp://host/live", "s3://bucket/a.mp4"} {
		if _, err := probe.NewProber().ProbeUncached(ctx, raw); !isUnsupported(err) {
			t.Fatalf("probe %q: %v", raw, err)
		}
		if _, err := builder.SeekLanding(ctx, raw, 0, 0); !isUnsupported(err) {
			t.Fatalf("seek landing %q: %v", raw, err)
		}
		if _, err := generator.spriteArgs(raw, "out-%d.jpg", 10); !isUnsupported(err) {
			t.Fatalf("sprite %q: %v", raw, err)
		}
		if _, err := generator.thumbnailArgs(raw, 10, domain.ThumbnailConfig{Width: 100}); !isUnsupported(err) {
			t.Fatalf("thumbnail %q: %v", raw, err)
		}
		failures := map[string]error{}
		if err := generator.extractSubtitleBatchFFmpeg(ctx, raw, []SubtitleJob{{TrackID: "en", StreamIndex: 1}}, failures); !isUnsupported(err) {
			t.Fatalf("subtitle %q: %v", raw, err)
		}
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("a process was started for an unsupported source")
	}
}

func isUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), source.ErrUnsupportedSource.Error())
}
