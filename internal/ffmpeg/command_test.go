package ffmpeg

import (
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/source"
)

var testHW = &domain.HWAccelConfig{
	Accelerator:  domain.AccelNone,
	DecodeFlags:  []string{"-hwaccel", "none"},
	EncodeFlags:  []string{"-c:v", "libx264"},
	Encoder:      "libx264",
	KeyframeFlag: "-force_key_frames",
	ScaleFilter:  "scale=%d:%d",
}

func landing(value float64) *float64 {
	return &value
}

func mustArgs(t *testing.T, args []string, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	return strings.Join(args, " ")
}

func optionValue(t *testing.T, joined, option string) string {
	t.Helper()
	fields := strings.Fields(joined)
	for index, field := range fields {
		if field == option && index+1 < len(fields) {
			return fields[index+1]
		}
	}
	t.Fatalf("option %s missing: %s", option, joined)
	return ""
}

func TestVideoCommand_TranscodeIncludesSegmentTimes(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{
		{Index: 0, Start: 12.0, End: 18.5},
		{Index: 1, Start: 18.5, End: 24.0},
		{Index: 2, Start: 24.0, End: 30.0},
	}

	args, err := builder.Video(VideoParams{
		InputURL:    "input.mp4",
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: domain.Transcode, Width: 1280, Height: 720, Bitrate: 2_000_000},
		Segments:    segments,
		OutputDir:   "/tmp/out",
	})
	joined := mustArgs(t, args, err)

	if !strings.Contains(joined, "-segment_times 6.500000,12.000000") {
		t.Fatalf("expected relative segment times, got %s", joined)
	}
	if !strings.Contains(joined, "-force_key_frames 11.999000,18.499000,23.999000") {
		t.Fatalf("expected keyframe times one millisecond ahead of each segment start, got %s", joined)
	}
	if !strings.Contains(joined, "-to 29.999000") || strings.Contains(joined, "-t 18.000000") {
		t.Fatalf("expected absolute end-time clipping one millisecond ahead of the job end, got %s", joined)
	}
	if !strings.Contains(joined, "-hwaccel none -ss 11.999000 -protocol_whitelist "+source.ProtocolWhitelist+" -i file:input.mp4") {
		t.Fatalf("decode flags must precede the seek: %s", joined)
	}
	if !strings.Contains(joined, "-vf scale=1280:720") {
		t.Fatalf("missing scale filter: %s", joined)
	}
	if !strings.Contains(joined, filepath.Join("/tmp/out", "segment-%05d.ts")) {
		t.Fatalf("output pattern missing: %s", joined)
	}
	assertTimestampsPreservedAfterSeek(t, joined)
}

func TestVideoCommand_DirectStreamUsesCopyAndOffsetTimes(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{
		{Index: 5, Start: 30.0, End: 36.0},
		{Index: 6, Start: 36.0, End: 42.5},
	}

	args, err := builder.Video(VideoParams{
		InputURL:    "input.mp4",
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: domain.DirectStream, Width: 1920, Height: 1080, Bitrate: 8_000_000},
		Segments:    segments,
		OutputDir:   "/tmp/out",
		Landing:     landing(29.5),
	})
	joined := mustArgs(t, args, err)

	if !strings.Contains(joined, "-c:v copy") {
		t.Fatalf("direct stream should copy video: %s", joined)
	}
	if strings.Contains(joined, "-hwaccel") {
		t.Fatalf("direct stream must not decode: %s", joined)
	}
	if !strings.Contains(joined, "-bsf:v h264_mp4toannexb,dump_extra") {
		t.Fatalf("direct stream should repeat H.264 parameter sets: %s", joined)
	}
	if !strings.Contains(joined, "-segment_times 6.500000 ") {
		t.Fatalf("expected segment times measured from the landing: %s", joined)
	}
	assertTimestampsPreservedAfterSeek(t, joined)
}

func TestAudioCommand_TranscodeAndCopy(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 5}, {Index: 1, Start: 5, End: 10}}

	args, err := builder.Audio(AudioParams{
		InputURL:    "in.mkv",
		StreamIndex: 1,
		Rendition:   domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 192000},
		Segments:    segments,
		OutputDir:   "/tmp/a",
	})
	joined := mustArgs(t, args, err)

	if !strings.Contains(joined, "-c:a aac") || !strings.Contains(joined, "-ac 2") || !strings.Contains(joined, "-b:a 192000") || !strings.Contains(joined, "-ar 48000") {
		t.Fatalf("transcode audio args missing: %s", joined)
	}
	if !strings.Contains(joined, "-segment_times 5.021333 ") {
		t.Fatalf("audio cut must be measured from the priming packet one frame before the seek: %s", joined)
	}
	if !strings.Contains(joined, "-to 10.000000") || strings.Contains(joined, "-t 10.000000") {
		t.Fatalf("audio should clip at the absolute end time, got %s", joined)
	}
	assertTimestampsPreservedAfterSeek(t, joined)

	copyArgs, err := builder.Audio(AudioParams{
		InputURL:    "in.mkv",
		StreamIndex: 1,
		Rendition:   domain.AudioRendition{Method: domain.DirectStream},
		Segments:    segments,
		OutputDir:   "/tmp/a",
		Landing:     landing(0),
	})
	copied := mustArgs(t, copyArgs, err)
	if !strings.Contains(copied, "-c:a copy") || strings.Contains(copied, "-c:a aac") {
		t.Fatalf("copy should set codec copy: %s", copied)
	}
	if !strings.Contains(copied, "-segment_times 5.000000 ") {
		t.Fatalf("copied audio cut must be measured from the landing: %s", copied)
	}
}

func TestAACJobSeeksOnTheFrameGrid(t *testing.T) {
	frame := 1024.0 / 48000
	overlap := domain.Segment{Index: 3, Start: 6.214542, End: 8.341667}
	segments := []domain.Segment{{Index: 4, Start: 8.341667, End: 10.468792}, {Index: 5, Start: 10.468792, End: 12.5}}
	args, err := NewCommandBuilder(testHW).Audio(AudioParams{
		InputURL:  "in.mp4",
		Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000},
		Segments:  segments,
		Overlap:   &overlap,
		OutputDir: "/tmp/a",
	})
	joined := mustArgs(t, args, err)

	seek, err := strconv.ParseFloat(optionValue(t, joined, "-ss"), 64)
	if err != nil {
		t.Fatalf("parse seek: %v", err)
	}
	frames := seek / frame
	if math.Abs(frames-math.Round(frames)) > 0.0001 || seek > overlap.Start || overlap.Start-seek >= frame {
		t.Fatalf("seek %.6f is not the frame grid position at or before %.6f", seek, overlap.Start)
	}
	times := strings.Split(optionValue(t, joined, "-segment_times"), ",")
	if len(times) != 2 {
		t.Fatalf("expected a cut after the overlap and one between the segments, got %v", times)
	}
	for index, boundary := range []float64{segments[0].Start, segments[1].Start} {
		got, err := strconv.ParseFloat(times[index], 64)
		if err != nil {
			t.Fatalf("parse cut: %v", err)
		}
		if want := boundary - (seek - frame); math.Abs(got-want) > 0.000002 {
			t.Fatalf("cut %d is %.6f, want %.6f measured from the priming packet", index, got, want)
		}
	}
	if want := "aresample=48000:first_pts=" + strconv.FormatInt(int64(math.Round(seek*48000)), 10); optionValue(t, joined, "-af") != want {
		t.Fatalf("the encoder input must be pinned to the frame grid with %s: %s", want, joined)
	}
	if optionValue(t, joined, "-segment_start_number") != "3" {
		t.Fatalf("numbering must start at the overlap segment: %s", joined)
	}
}

func TestAudioJobsCutOnExactBoundaries(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 5}, {Index: 1, Start: 5, End: 10}}
	for _, rendition := range []domain.AudioRendition{{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, {Method: domain.DirectStream}} {
		args, err := builder.Audio(AudioParams{InputURL: "in.mkv", Rendition: rendition, Segments: segments, OutputDir: "/tmp/a", Landing: landing(0)})
		joined := mustArgs(t, args, err)
		delta, err := strconv.ParseFloat(optionValue(t, joined, "-segment_time_delta"), 64)
		if err != nil {
			t.Fatalf("parse delta: %v", err)
		}
		if delta > 0.000001 {
			t.Fatalf("%s audio may cut up to %.6fs before its boundary", rendition.Method, delta)
		}
	}
}

func TestKeyframeTimesNeverRoundPastTheirFrame(t *testing.T) {
	for _, rate := range []float64{24000.0 / 1001, 30000.0 / 1001} {
		for frame := 0; frame <= 20000; frame++ {
			exact := float64(frame) / rate
			plan := math.Round(exact*1e6) / 1e6
			printed, err := strconv.ParseFloat(keyframeTimes([]domain.Segment{{Start: plan}}), 64)
			if err != nil {
				t.Fatalf("parse keyframe time: %v", err)
			}
			if printed > exact {
				t.Fatalf("frame %d at %.3f fps: keyframe time %.6f is after the frame at %.9f", frame, rate, printed, exact)
			}
			if previous := float64(frame-1) / rate; frame > 0 && printed <= previous {
				t.Fatalf("frame %d at %.3f fps: keyframe time %.6f reaches back to the previous frame at %.9f", frame, rate, printed, previous)
			}
			if printed < 0 {
				t.Fatalf("frame %d: keyframe time %.6f is negative", frame, printed)
			}
		}
	}
}

func TestEveryJobCarriesTheTimelineOffset(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 5}, {Index: 1, Start: 5, End: 10}}
	want := "-output_ts_offset 10.000000"
	if domain.OutputTimelineOffset.Seconds() != 10 {
		t.Fatalf("test expects a 10 s offset, domain has %v", domain.OutputTimelineOffset)
	}

	jobs := map[string]func() ([]string, error){
		"copy video": func() ([]string, error) {
			return builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(0)})
		},
		"transcoded video": func() ([]string, error) {
			return builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000}, Segments: segments})
		},
		"aac audio": func() ([]string, error) {
			return builder.Audio(AudioParams{InputURL: "in", Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, Segments: segments})
		},
		"copy audio": func() ([]string, error) {
			return builder.Audio(AudioParams{InputURL: "in", Rendition: domain.AudioRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(0)})
		},
	}
	for name, build := range jobs {
		args, err := build()
		joined := mustArgs(t, args, err)
		if strings.Count(joined, want) != 1 {
			t.Errorf("%s job must carry %q exactly once: %s", name, want, joined)
		}
		assertTimestampsPreservedAfterSeek(t, joined)
	}
}

func TestCopyJobCutsTrailingSegmentAndExtendsEnd(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	overlap := domain.Segment{Index: 3, Start: 18, End: 24}
	segments := []domain.Segment{{Index: 4, Start: 24, End: 30}, {Index: 5, Start: 30, End: 36.5}}

	short := domain.Segment{Index: 6, Start: 36.5, End: 37}
	args, err := builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Overlap: &overlap, Trailing: &short, Landing: landing(17.5)})
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "-segment_times 6.500000,12.500000,19.000000 ") {
		t.Fatalf("expected cuts after the overlap, between segments and before the trailing segment: %s", joined)
	}
	if !strings.Contains(joined, "-to 37.000000") {
		t.Fatalf("a short trailing segment must end the job at its own end: %s", joined)
	}
	if !strings.Contains(joined, "-ss 18.000000") || !strings.Contains(joined, "-segment_start_number 3") {
		t.Fatalf("the job must start at the overlap segment: %s", joined)
	}

	long := domain.Segment{Index: 6, Start: 36.5, End: 44}
	args, err = builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Trailing: &long, Landing: landing(24)})
	joined = mustArgs(t, args, err)
	if !strings.Contains(joined, "-to 37.500000") {
		t.Fatalf("a long trailing segment must be read for one second only: %s", joined)
	}
	if !strings.Contains(joined, "-segment_times 6.000000,12.500000 ") {
		t.Fatalf("expected a cut at the trailing segment: %s", joined)
	}

	args, err = builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(24)})
	joined = mustArgs(t, args, err)
	if !strings.Contains(joined, "-to 36.500000") || !strings.Contains(joined, "-segment_times 6.000000 ") {
		t.Fatalf("without a trailing segment the job ends at its last segment: %s", joined)
	}
}

func TestCopyJobRequiresMeasuredLanding(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 2, Start: 4, End: 6}}
	if _, err := builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments}); !errors.Is(err, ErrLandingRequired) {
		t.Fatalf("copied video without a landing: got %v", err)
	}
	if _, err := builder.Audio(AudioParams{InputURL: "in", Rendition: domain.AudioRendition{Method: domain.DirectStream}, Segments: segments}); !errors.Is(err, ErrLandingRequired) {
		t.Fatalf("copied audio without a landing: got %v", err)
	}
}

func TestTrailingRejectedForTranscodeAndAudio(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 2, Start: 4, End: 6}}
	trailing := domain.Segment{Index: 3, Start: 6, End: 8}
	if _, err := builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000}, Segments: segments, Trailing: &trailing}); !errors.Is(err, ErrTrailingNotSupported) {
		t.Fatalf("transcoded video with a trailing segment: got %v", err)
	}
	for _, method := range []domain.PlaybackMethod{domain.Transcode, domain.DirectStream} {
		if _, err := builder.Audio(AudioParams{InputURL: "in", Rendition: domain.AudioRendition{Method: method, Channels: 2, Bitrate: 128000}, Segments: segments, Trailing: &trailing, Landing: landing(4)}); !errors.Is(err, ErrTrailingNotSupported) {
			t.Fatalf("%s audio with a trailing segment: got %v", method, err)
		}
	}
}

func TestJobNeighboursMustBeAdjacent(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 2, Start: 4, End: 6}}
	copyRendition := domain.VideoRendition{Method: domain.DirectStream}
	cases := map[string]struct {
		params VideoParams
		want   error
	}{
		"no segments":          {VideoParams{Rendition: copyRendition, Landing: landing(0)}, ErrNoSegments},
		"overlap wrong index":  {VideoParams{Rendition: copyRendition, Segments: segments, Overlap: &domain.Segment{Index: 0, Start: 2, End: 4}, Landing: landing(2)}, ErrOverlapNotAdjacent},
		"overlap wrong end":    {VideoParams{Rendition: copyRendition, Segments: segments, Overlap: &domain.Segment{Index: 1, Start: 2, End: 3.5}, Landing: landing(2)}, ErrOverlapNotAdjacent},
		"trailing wrong index": {VideoParams{Rendition: copyRendition, Segments: segments, Trailing: &domain.Segment{Index: 4, Start: 6, End: 8}, Landing: landing(4)}, ErrTrailingNotAdjacent},
		"trailing wrong start": {VideoParams{Rendition: copyRendition, Segments: segments, Trailing: &domain.Segment{Index: 3, Start: 6.5, End: 8}, Landing: landing(4)}, ErrTrailingNotAdjacent},
	}
	for name, test := range cases {
		if _, err := builder.Video(test.params); !errors.Is(err, test.want) {
			t.Errorf("%s: got %v, want %v", name, err, test.want)
		}
	}
	if _, err := builder.Audio(AudioParams{Rendition: domain.AudioRendition{Method: domain.Transcode}}); !errors.Is(err, ErrNoSegments) {
		t.Errorf("audio without segments: got %v", err)
	}
	if _, err := builder.Audio(AudioParams{Rendition: domain.AudioRendition{Method: domain.Transcode}, Segments: segments, Overlap: &domain.Segment{Index: 0, Start: 0, End: 4}}); !errors.Is(err, ErrOverlapNotAdjacent) {
		t.Errorf("audio with a distant overlap: got %v", err)
	}
}

func TestSingleFileJobStillDeclaresACutBeyondItsEnd(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6}}
	jobs := map[string]func() ([]string, error){
		"copy video": func() ([]string, error) {
			return builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(0)})
		},
		"transcoded video": func() ([]string, error) {
			return builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000}, Segments: segments})
		},
		"aac audio": func() ([]string, error) {
			return builder.Audio(AudioParams{InputURL: "in", Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, Segments: segments})
		},
		"copy audio": func() ([]string, error) {
			return builder.Audio(AudioParams{InputURL: "in", Rendition: domain.AudioRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(0)})
		},
	}
	for name, build := range jobs {
		args, err := build()
		joined := mustArgs(t, args, err)
		cut, err := strconv.ParseFloat(optionValue(t, joined, "-segment_times"), 64)
		if err != nil {
			t.Fatalf("%s: parse cut: %v", name, err)
		}
		if cut <= 6.1 {
			t.Errorf("%s: the only cut %.6f must lie beyond the job end so the muxer never splits: %s", name, cut, joined)
		}
	}
}

func TestVideoCommandDoesNotMutateHardwareConfig(t *testing.T) {
	config := &domain.HWAccelConfig{
		EncodeFlags:  make([]string, 2, 16),
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d",
	}
	copy(config.EncodeFlags, []string{"-c:v", "libx264"})
	builder := NewCommandBuilder(config)
	if _, err := builder.Video(VideoParams{
		InputURL:  "input.mp4",
		Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600_000},
		Segments:  []domain.Segment{{Index: 0, Start: 0, End: 6}},
		OutputDir: "/tmp/out",
	}); err != nil {
		t.Fatalf("build command: %v", err)
	}
	if len(config.EncodeFlags) != 2 || config.EncodeFlags[0] != "-c:v" || config.EncodeFlags[1] != "libx264" {
		t.Fatalf("command construction mutated shared hardware configuration: %#v", config.EncodeFlags)
	}
}

func TestCUDATranscodeForcesIDRFrames(t *testing.T) {
	builder := NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelCUDA,
		DecodeFlags:  []string{"-hwaccel", "cuda"},
		EncodeFlags:  []string{"-c:v", "h264_nvenc"},
		Encoder:      "h264_nvenc",
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale_cuda=%d:%d:format=nv12",
	})
	args, err := builder.Video(VideoParams{
		InputURL:    "file.mkv",
		StreamIndex: 2,
		Rendition:   domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600_000},
		Segments:    []domain.Segment{{Index: 0, Start: 0, End: 2.5}, {Index: 1, Start: 2.5, End: 5}},
	})
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "-force_key_frames 0.000000,2.499000") {
		t.Fatalf("cuda keyframes missing: %s", joined)
	}
	if !strings.Contains(joined, "-forced-idr 1") {
		t.Fatalf("cuda forced IDR mode missing: %s", joined)
	}
	if !strings.Contains(joined, "-vf scale_cuda=640:360:format=nv12") {
		t.Fatalf("cuda scale missing: %s", joined)
	}
}

func assertTimestampsPreservedAfterSeek(t *testing.T, joined string) {
	t.Helper()
	if strings.Count(joined, "-copyts") != 1 || strings.Count(joined, "-start_at_zero") != 1 {
		t.Fatalf("seek output must preserve the media timeline: %s", joined)
	}
}

func TestParseFramecrcFirstPTS(t *testing.T) {
	output := "#format: frame checksums\n#version: 2\n#hash: CRC32\n#tb 0: 1/12800\n#media_type 0: video\n#stream#, dts,        pts, duration,     size, crc\n0,      25600,      25600,      512,     1234, 0xdeadbeef\n"
	got, err := parseFramecrcFirstPTS(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2.0 {
		t.Fatalf("expected 2.0, got %v", got)
	}
}

func TestParseFramecrcFirstPTSRejectsMissingPacket(t *testing.T) {
	if _, err := parseFramecrcFirstPTS("#tb 0: 1/25\n#stream#, dts, pts, duration, size, crc\n"); err == nil {
		t.Fatal("expected error when no packet was written")
	}
}

func TestParseFramecrcFirstPTSRejectsMalformedPacket(t *testing.T) {
	if _, err := parseFramecrcFirstPTS("#tb 0: 1/25\n0, x, y, 1, 2, 0x0\n"); err == nil {
		t.Fatal("expected error for malformed pts")
	}
}

func TestSeekLandingSharesSeekInputPrefixWithVideo(t *testing.T) {
	input, err := source.InputArgs("input.mp4")
	if err != nil {
		t.Fatalf("input args: %v", err)
	}
	prefix := strings.Join(seekInputArgs(input, 4), " ")
	args, err := NewCommandBuilder(testHW).Video(VideoParams{
		InputURL:  "input.mp4",
		Rendition: domain.VideoRendition{Method: domain.DirectStream},
		Segments:  []domain.Segment{{Index: 2, Start: 4, End: 6}},
		OutputDir: "/tmp/out",
		Landing:   landing(4),
	})
	if joined := mustArgs(t, args, err); !strings.Contains(joined, prefix) {
		t.Fatalf("Video args must contain %q: %s", prefix, joined)
	}
	landingArgs, err := seekLandingArgs("input.mp4", 4, 1)
	if err != nil {
		t.Fatalf("seek landing args: %v", err)
	}
	if measured := strings.Join(landingArgs, " "); !strings.Contains(measured, prefix) || !strings.Contains(measured, "-map 0:1") {
		t.Fatalf("seek landing must seek exactly like the job: %s", measured)
	}
}

func TestTranscodedVideoKeepsSourceFrameTiming(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6}}
	args, err := builder.Video(VideoParams{InputURL: "in.mkv", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000}, Segments: segments})
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "-fps_mode passthrough") || !strings.Contains(joined, "-enc_time_base demux") {
		t.Fatalf("transcoded video must keep source frame timestamps instead of re-timing them to a frame grid: %s", joined)
	}
	args, err = builder.Video(VideoParams{InputURL: "in.mkv", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(0)})
	if copied := mustArgs(t, args, err); strings.Contains(copied, "-fps_mode") || strings.Contains(copied, "-enc_time_base") {
		t.Fatalf("copied video has no encoder timing to set: %s", copied)
	}
}
