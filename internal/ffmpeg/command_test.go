package ffmpeg

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

var testHW = &domain.HWAccelConfig{
	Accelerator:  domain.AccelNone,
	DecodeFlags:  []string{"-hwaccel", "none"},
	EncodeFlags:  []string{"-c:v", "libx264"},
	Encoder:      "libx264",
	KeyframeFlag: "-force_key_frames",
	ScaleFilter:  "scale=%d:%d",
}

func TestVideoCommand_TranscodeIncludesSegmentTimes(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{
		{Index: 0, Start: 12.0, End: 18.5},
		{Index: 1, Start: 18.5, End: 24.0},
		{Index: 2, Start: 24.0, End: 30.0},
	}

	args := builder.Video(VideoParams{
		InputURL:    "input.mp4",
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: domain.Transcode, Width: 1280, Height: 720, Bitrate: 2_000_000},
		Segments:    segments,
		OutputDir:   "/tmp/out",
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-segment_times 6.500000,12.000000") {
		t.Fatalf("expected relative segment times, got %s", joined)
	}
	if !strings.Contains(joined, "-vf scale=1280:720") {
		t.Fatalf("missing scale filter: %s", joined)
	}
	if !strings.Contains(joined, filepath.Join("/tmp/out", "segment-%05d.ts")) {
		t.Fatalf("output pattern missing: %s", joined)
	}
}

func TestVideoCommand_DirectStreamUsesCopyAndOffsetTimes(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{
		{Index: 5, Start: 30.0, End: 36.0},
		{Index: 6, Start: 36.0, End: 42.5},
	}

	args := builder.Video(VideoParams{
		InputURL:           "input.mp4",
		StreamIndex:        0,
		Rendition:          domain.VideoRendition{Method: domain.DirectStream, Width: 1920, Height: 1080, Bitrate: 8_000_000},
		Segments:           segments,
		OutputDir:          "/tmp/out",
		ActualSeekKeyframe: 29.5,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v copy") {
		t.Fatalf("direct stream should copy video: %s", joined)
	}
	if !strings.Contains(joined, "-segment_times 6.500000") {
		t.Fatalf("expected offset segment times when seek offset present: %s", joined)
	}
}

func TestAudioCommand_TranscodeAndCopy(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 5}, {Index: 1, Start: 5, End: 10}}

	transcode := builder.Audio(AudioParams{
		InputURL:    "in.mkv",
		StreamIndex: 1,
		Rendition:   domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 192000},
		Segments:    segments,
		OutputDir:   "/tmp/a",
	})

	joined := strings.Join(transcode, " ")
	if !strings.Contains(joined, "-c:a aac") || !strings.Contains(joined, "-ac 2") || !strings.Contains(joined, "-b:a 192000") {
		t.Fatalf("transcode audio args missing: %s", joined)
	}
	if !strings.Contains(joined, "-segment_times 5.000000") {
		t.Fatalf("audio segment times missing: %s", joined)
	}

	copyArgs := builder.Audio(AudioParams{
		InputURL:    "in.mkv",
		StreamIndex: 1,
		Rendition:   domain.AudioRendition{Method: domain.DirectStream},
		Segments:    segments,
		OutputDir:   "/tmp/a",
	})
	if strings.Join(copyArgs, " ") == strings.Join(transcode, " ") {
		t.Fatalf("copy args should differ from transcode")
	}
	if !strings.Contains(strings.Join(copyArgs, " "), "-c:a copy") {
		t.Fatalf("copy should set codec copy: %v", copyArgs)
	}
}

func TestVideoStreamArgsIncludeKeyframesAndForcedIDRForCUDA(t *testing.T) {
	builder := NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelCUDA,
		DecodeFlags:  []string{"-hwaccel", "cuda"},
		EncodeFlags:  []string{"-c:v", "h264_nvenc"},
		Encoder:      "h264_nvenc",
		KeyframeFlag: "-force_idr",
		ScaleFilter:  "scale_cuda=%d:%d:format=nv12",
	})

	args := builder.VideoStream(VideoStreamParams{
		StreamParams:  StreamParams{InputURL: "file.mkv", StreamIndex: 2, StartTime: 10, EndTime: 20},
		Rendition:     domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600_000},
		KeyframeTimes: []float64{0, 2.5, 5.0},
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-force_idr 0.000000,2.500000,5.000000") {
		t.Fatalf("cuda keyframes missing: %s", joined)
	}
	if !strings.Contains(joined, "-vf scale_cuda=640:360:format=nv12") {
		t.Fatalf("cuda scale missing: %s", joined)
	}
}

func TestHelpersHandleEdgeCases(t *testing.T) {
	if got := formatSegmentTimes(nil); got != "" {
		t.Fatalf("expected empty for nil segments, got %q", got)
	}
	if got := formatKeyframeTimes(nil); got != "" {
		t.Fatalf("expected empty for no segments, got %q", got)
	}
	segs := []domain.Segment{{Start: 5}, {Start: 8}, {Start: 11}}
	if got := formatSegmentTimesWithOffset(segs, 4); got != "4.000000,7.000000" {
		t.Fatalf("unexpected offset times %q", got)
	}
}
