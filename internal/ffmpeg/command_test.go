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
		Rendition:   domain.VideoRendition{Method: domain.Transcode, Width: 1280, Height: 720, Bitrate: 2_000_000, PeakBitrate: 3000000, BufferSize: 6000000, Level: 31},
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
	if !strings.Contains(joined, "-hwaccel none -ss 11.999000 -format_whitelist "+source.FormatWhitelist+" -protocol_whitelist "+source.ProtocolWhitelist+" -i file:input.mp4") {
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
	if want := "aresample=48000,aresample=48000:first_pts=" + strconv.FormatInt(int64(math.Round(seek*48000)), 10) + ",apad"; optionValue(t, joined, "-af") != want {
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
			return builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000, PeakBitrate: 900000, BufferSize: 1800000, Level: 30}, Segments: segments})
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

func TestTrailingRejectedForTranscode(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 2, Start: 4, End: 6}}
	trailing := domain.Segment{Index: 3, Start: 6, End: 8}
	if _, err := builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000, PeakBitrate: 900000, BufferSize: 1800000, Level: 30}, Segments: segments, Trailing: &trailing}); !errors.Is(err, ErrTrailingNotSupported) {
		t.Fatalf("transcoded video with a trailing segment: got %v", err)
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
			return builder.Video(VideoParams{InputURL: "in", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000, PeakBitrate: 900000, BufferSize: 1800000, Level: 30}, Segments: segments})
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
		Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600_000, PeakBitrate: 900000, BufferSize: 1800000, Level: 30},
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
		Rendition:   domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600_000, PeakBitrate: 900000, BufferSize: 1800000, Level: 30},
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
	args, err := builder.Video(VideoParams{InputURL: "in.mkv", Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000, PeakBitrate: 900000, BufferSize: 1800000, Level: 30}, Segments: segments})
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "-fps_mode passthrough") || !strings.Contains(joined, "-enc_time_base demux") {
		t.Fatalf("transcoded video must keep source frame timestamps instead of re-timing them to a frame grid: %s", joined)
	}
	args, err = builder.Video(VideoParams{InputURL: "in.mkv", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(0)})
	if copied := mustArgs(t, args, err); strings.Contains(copied, "-fps_mode") || strings.Contains(copied, "-enc_time_base") {
		t.Fatalf("copied video has no encoder timing to set: %s", copied)
	}
}

func completeRendition() domain.VideoRendition {
	return domain.VideoRendition{Name: "720p", Method: domain.Transcode, Width: 1280, Height: 720, Bitrate: 2_000_000, PeakBitrate: 3_100_000, BufferSize: 5_300_000, Level: 30, FrameRate: 60}
}

func TestTranscodeArgsUseRenditionFactsVerbatim(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}}
	cases := []struct {
		level int
		want  string
	}{{30, "3.0"}, {31, "3.1"}, {40, "4.0"}, {42, "4.2"}, {52, "5.2"}}
	for _, c := range cases {
		rendition := completeRendition()
		rendition.Level = c.level
		args, err := builder.Video(VideoParams{InputURL: "in.mkv", Rendition: rendition, Segments: segments, OutputDir: "/tmp/v"})
		joined := mustArgs(t, args, err)
		for option, want := range map[string]string{"-level:v": c.want, "-b:v": "2000000", "-maxrate": "3100000", "-bufsize": "5300000", "-profile:v": "high"} {
			if got := optionValue(t, joined, option); got != want {
				t.Errorf("level %d: %s = %s, want the rendition value %s", c.level, option, got, want)
			}
		}
		if !strings.Contains(joined, "scale=1280:720") {
			t.Errorf("level %d: scale must use the rendition size: %s", c.level, joined)
		}
	}
}

func TestTranscodeRejectsIncompleteRendition(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}}
	strip := map[string]func(*domain.VideoRendition){
		"width":   func(r *domain.VideoRendition) { r.Width = 0 },
		"height":  func(r *domain.VideoRendition) { r.Height = 0 },
		"bitrate": func(r *domain.VideoRendition) { r.Bitrate = 0 },
		"peak":    func(r *domain.VideoRendition) { r.PeakBitrate = 0 },
		"buffer":  func(r *domain.VideoRendition) { r.BufferSize = 0 },
		"level":   func(r *domain.VideoRendition) { r.Level = 0 },
	}
	for name, remove := range strip {
		rendition := completeRendition()
		remove(&rendition)
		if _, err := builder.Video(VideoParams{InputURL: "in.mkv", Rendition: rendition, Segments: segments, OutputDir: "/tmp/v"}); !errors.Is(err, ErrRenditionIncomplete) {
			t.Errorf("a transcode rendition without %s must be rejected, got %v", name, err)
		}
	}
	if _, err := builder.Video(VideoParams{InputURL: "in.mkv", Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(0), OutputDir: "/tmp/v"}); err != nil {
		t.Fatalf("a copied rendition needs no encoder facts: %v", err)
	}
}

func audioEndSegments() []domain.Segment {
	segments := make([]domain.Segment, 10)
	for index := range segments {
		segments[index] = domain.Segment{Index: index, Start: float64(index), End: float64(index + 1), Duration: 1}
	}
	return segments
}

func TestAudioJobCoverageFollowsSourceEnd(t *testing.T) {
	segments := audioEndSegments()
	cases := []struct {
		name      string
		sourceEnd float64
		from, to  int
		overlap   bool
		want      audioCoverage
	}{
		{"unknown end", 0, 8, 9, true, audioCovered},
		{"inside", 7.5, 2, 5, true, audioCovered},
		{"ends exactly at the guard", 7.05, 5, 6, true, audioCovered},
		{"reaches past the guard", 7.049, 5, 6, true, audioPadded},
		{"straddles the end", 7.5, 6, 9, true, audioPadded},
		{"overlap is inside", 7.5, 8, 9, true, audioPadded},
		{"overlap starts just inside the guard", 8.051, 9, 9, true, audioPadded},
		{"overlap starts on the guard", 8.05, 9, 9, true, audioSilent},
		{"wholly past", 7.5, 9, 9, true, audioSilent},
		{"wholly past without overlap", 7.5, 8, 9, false, audioSilent},
		{"end over the plan", 12, 8, 9, true, audioCovered},
	}
	for _, c := range cases {
		first := segments[c.from]
		if c.overlap {
			first = segments[c.from-1]
		}
		if got := audioCoverageOf(domain.AudioRendition{SourceEnd: c.sourceEnd}, first, segments[c.to]); got != c.want {
			t.Errorf("%s: coverage = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestAACJobAlwaysPadsItsInput(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := audioEndSegments()
	for _, sourceEnd := range []float64{0, 7.5, 20} {
		rendition := domain.AudioRendition{Method: domain.Transcode, Codec: "aac", Channels: 2, Bitrate: 128000, SourceEnd: sourceEnd}
		args, err := builder.Audio(AudioParams{InputURL: "in.mkv", StreamIndex: 1, Rendition: rendition, Segments: segments[6:8], Overlap: &segments[5], OutputDir: "/tmp/a"})
		joined := mustArgs(t, args, err)
		if got := optionValue(t, joined, "-af"); got != "aresample=48000,aresample=48000:first_pts=239616,apad" {
			t.Errorf("source end %.1f: -af = %s, want the grid pin followed by apad", sourceEnd, got)
		}
		if optionValue(t, joined, "-to") != "8.000000" || !strings.Contains(joined, "-ss 4.992000 ") || strings.Contains(joined, "lavfi") {
			t.Errorf("source end %.1f: a job that starts inside the audio reads the source: %s", sourceEnd, joined)
		}
	}
}

func TestAACJobPastSourceEndGeneratesSilence(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	rendition := domain.AudioRendition{Method: domain.Transcode, Codec: "aac", Channels: 6, Bitrate: 384000, SourceEnd: 7.5}
	overlap := domain.Segment{Index: 8, Start: 8.1, End: 9.1, Duration: 1}
	last := domain.Segment{Index: 9, Start: 9.1, End: 10, Duration: 0.9}
	params := AudioParams{InputURL: "in.mkv", StreamIndex: 1, Rendition: rendition, Segments: []domain.Segment{last}, Overlap: &overlap, OutputDir: "/tmp/a"}
	args, err := builder.Audio(params)
	joined := mustArgs(t, args, err)

	real := params
	real.Rendition.SourceEnd = 0
	realArgs, err := builder.Audio(real)
	realJoined := mustArgs(t, realArgs, err)

	if !strings.Contains(joined, "-f lavfi -i anullsrc=r=48000:cl=stereo ") || strings.Contains(joined, "in.mkv") || strings.Contains(joined, "-ss ") {
		t.Fatalf("a job wholly past the audio end must read generated silence only: %s", joined)
	}
	if strings.Contains(joined, "-copyts") || optionValue(t, joined, "-map") != "0:0" {
		t.Fatalf("generated silence is timed by the offset alone: %s", joined)
	}
	seek := 379.0 * 1024 / 48000
	if got := optionValue(t, joined, "-output_ts_offset"); got != formatSeconds(domain.OutputTimelineOffset.Seconds()+seek) {
		t.Fatalf("offset = %s, want the timeline offset plus the grid seek %.6f", got, seek)
	}
	if got := optionValue(t, joined, "-to"); got != formatSeconds(10-seek) {
		t.Fatalf("-to = %s, want the job length %.6f", got, 10-seek)
	}
	for _, option := range []string{"-segment_times", "-segment_start_number", "-segment_time_delta", "-c:a", "-ac", "-b:a", "-ar"} {
		if got, want := optionValue(t, joined, option), optionValue(t, realJoined, option); got != want {
			t.Errorf("%s = %s, a job over real audio uses %s", option, got, want)
		}
	}
	if strings.Contains(joined, "-af") {
		t.Fatalf("generated silence needs no filter: %s", joined)
	}
}

func TestPassthroughJobInsideSourceCopies(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := audioEndSegments()
	for _, sourceEnd := range []float64{0, 7.5} {
		rendition := domain.AudioRendition{Method: domain.DirectStream, Codec: "ac3", Channels: 6, Bitrate: 640000, SourceEnd: sourceEnd}
		params := AudioParams{InputURL: "in.mkv", StreamIndex: 2, Rendition: rendition, Segments: segments[3:6], Overlap: &segments[2], Landing: landing(1.984), OutputDir: "/tmp/a"}
		args, err := builder.Audio(params)
		joined := mustArgs(t, args, err)
		if optionValue(t, joined, "-c:a") != "copy" || strings.Contains(joined, "apad") || strings.Contains(joined, "-noaccurate_seek") || strings.Contains(joined, "lavfi") {
			t.Errorf("source end %.1f: a job inside the audio must copy: %s", sourceEnd, joined)
		}
		if optionValue(t, joined, "-output_ts_offset") != "10.000000" {
			t.Errorf("source end %.1f: copied audio keeps the plain timeline offset: %s", sourceEnd, joined)
		}
		if got := params.LandingStart(); got != 2 {
			t.Errorf("source end %.1f: landing is measured at %.3f, want the job seek 2", sourceEnd, got)
		}
	}
}

func TestPassthroughJobReachingSourceEndReencodesAndPads(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := audioEndSegments()
	rendition := domain.AudioRendition{Method: domain.DirectStream, Codec: "eac3", Channels: 8, Bitrate: 1_536_000, SourceEnd: 7.5}
	params := AudioParams{InputURL: "in.mkv", StreamIndex: 2, Rendition: rendition, Segments: segments[7:9], Overlap: &segments[6], Landing: landing(5.984), OutputDir: "/tmp/a"}
	if got := params.LandingStart(); got != 6 {
		t.Fatalf("landing is measured at %.3f, want the job seek 6", got)
	}
	args, err := builder.Audio(params)
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "-noaccurate_seek -flags2 +skip_manual -ss 6.000000 ") {
		t.Fatalf("the decode must start on the landing packet, as the copy does: %s", joined)
	}
	for option, want := range map[string]string{"-c:a": "eac3", "-ac": "6", "-b:a": "1536000", "-af": "apad", "-to": "9.000000", "-output_ts_offset": "10.005333", "-segment_times": "1.016000,2.016000", "-map": "0:2"} {
		if got := optionValue(t, joined, option); got != want {
			t.Errorf("%s = %s, want %s", option, got, want)
		}
	}
	assertTimestampsPreservedAfterSeek(t, joined)

	params.Segments, params.Overlap, params.Landing = segments[0:9], nil, landing(-0.005333)
	args, err = builder.Audio(params)
	if got := optionValue(t, mustArgs(t, args, err), "-to"); got != "9.005333" {
		t.Fatalf("-to = %s, a negative landing must not shorten the job (want 9.005333)", got)
	}
	if _, err := builder.Audio(AudioParams{InputURL: "in.mkv", Rendition: rendition, Segments: segments[7:9], OutputDir: "/tmp/a"}); !errors.Is(err, ErrLandingRequired) {
		t.Fatalf("a padded passthrough job still needs its landing, got %v", err)
	}
}

func TestPassthroughJobPastSourceEndGeneratesSilenceOnTheSourceGrid(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := audioEndSegments()
	rendition := domain.AudioRendition{Method: domain.DirectStream, Codec: "ac3", Channels: 2, Bitrate: 192000, SourceEnd: 7.494}
	params := AudioParams{InputURL: "in.mkv", StreamIndex: 2, Rendition: rendition, Segments: segments[9:10], Overlap: &segments[8], OutputDir: "/tmp/a"}
	if got := params.LandingStart(); math.Abs(got-6.994) > 1e-9 {
		t.Fatalf("landing is measured at %.6f, want half a second inside the audio (6.994)", got)
	}
	params.Landing = landing(6.970667)
	args, err := builder.Audio(params)
	joined := mustArgs(t, args, err)
	if !strings.Contains(joined, "-f lavfi -i anullsrc=r=48000:cl=stereo ") || strings.Contains(joined, "in.mkv") || strings.Contains(joined, "-ss ") || strings.Contains(joined, "apad") {
		t.Fatalf("a job wholly past the audio end must read generated silence only: %s", joined)
	}
	grid := 6.970667 + 32*0.032
	for option, want := range map[string]string{
		"-c:a":              "ac3",
		"-ac":               "2",
		"-b:a":              "192000",
		"-map":              "0:0",
		"-to":               formatSeconds(10 - grid),
		"-output_ts_offset": formatSeconds(domain.OutputTimelineOffset.Seconds() + grid + 256.0/48000),
		"-segment_times":    formatSeconds(9 - grid),
	} {
		if got := optionValue(t, joined, option); got != want {
			t.Errorf("%s = %s, want %s", option, got, want)
		}
	}

	short := params
	short.Rendition.SourceEnd = 0.2
	if got := short.LandingStart(); got != 0 {
		t.Fatalf("landing is measured at %.3f, want the stream start for a very short track", got)
	}
	params.Landing = nil
	if _, err := builder.Audio(params); !errors.Is(err, ErrLandingRequired) {
		t.Fatalf("silence for a passthrough rendition needs a grid anchor, got %v", err)
	}
}

func TestPassthroughTailFollowsSourceSampleRate(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := audioEndSegments()
	for _, rate := range []int{44100, 32000, 48000} {
		rendition := domain.AudioRendition{Method: domain.DirectStream, Codec: "ac3", Channels: 2, Bitrate: 192000, SourceEnd: 7.5, SampleRate: rate}
		frame := 1536.0 / float64(rate)
		delay := 256.0 / float64(rate)
		offset := domain.OutputTimelineOffset.Seconds()

		padded, err := builder.Audio(AudioParams{InputURL: "in.mkv", StreamIndex: 2, Rendition: rendition, Segments: segments[7:9], Overlap: &segments[6], Landing: landing(5.99), OutputDir: "/tmp/a"})
		joined := mustArgs(t, padded, err)
		if got, want := optionValue(t, joined, "-output_ts_offset"), formatSeconds(offset+delay); got != want {
			t.Errorf("%d Hz padded job: offset = %s, want %s (256 samples at the source rate)", rate, got, want)
		}
		if strings.Contains(joined, "-ar ") {
			t.Errorf("%d Hz padded job must keep the source sample rate: %s", rate, joined)
		}

		anchor := 6.9
		silent, err := builder.Audio(AudioParams{InputURL: "in.mkv", StreamIndex: 2, Rendition: rendition, Segments: segments[9:10], Overlap: &segments[8], Landing: landing(anchor), OutputDir: "/tmp/a"})
		joined = mustArgs(t, silent, err)
		grid := anchor + math.Floor((8-anchor)/frame+domain.BoundaryTolerance)*frame
		for option, want := range map[string]string{
			"-i":                "anullsrc=r=" + strconv.Itoa(rate) + ":cl=stereo",
			"-output_ts_offset": formatSeconds(offset + delay + grid),
			"-to":               formatSeconds(10 - grid),
			"-segment_times":    formatSeconds(9 - grid),
		} {
			if got := optionValue(t, joined, option); got != want {
				t.Errorf("%d Hz silent job: %s = %s, want %s", rate, option, got, want)
			}
		}
	}
	stereo := domain.AudioRendition{Method: domain.Transcode, Codec: "aac", Channels: 2, Bitrate: 128000, SourceEnd: 7.5, SampleRate: 44100}
	resampled, err := builder.Audio(AudioParams{InputURL: "in.mkv", StreamIndex: 2, Rendition: stereo, Segments: segments[9:10], Overlap: &segments[8], OutputDir: "/tmp/a"})
	if got := optionValue(t, mustArgs(t, resampled, err), "-i"); got != "anullsrc=r=48000:cl=stereo" {
		t.Fatalf("AAC renditions are always 48000 Hz, silence was generated from %s", got)
	}
	unknown := domain.AudioRendition{Method: domain.DirectStream, Codec: "eac3", Channels: 2, Bitrate: 192000, SourceEnd: 7.5}
	args, err := builder.Audio(AudioParams{InputURL: "in.mkv", StreamIndex: 2, Rendition: unknown, Segments: segments[9:10], Overlap: &segments[8], Landing: landing(6.9), OutputDir: "/tmp/a"})
	if got := optionValue(t, mustArgs(t, args, err), "-i"); got != "anullsrc=r=48000:cl=stereo" {
		t.Fatalf("an unknown sample rate falls back to 48000, got %s", got)
	}
}

func TestVideoJobPastSourceEndClonesTheLastFrame(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{
		{Index: 6, Start: 30, End: 36, Duration: 6},
		{Index: 7, Start: 36, End: 42, Duration: 6},
		{Index: 8, Start: 42, End: 48, Duration: 6},
		{Index: 9, Start: 48, End: 53, Duration: 5},
	}
	rendition := completeRendition()
	rendition.SourceEnd = 40
	scale := "scale=1280:720"
	build := func(r domain.VideoRendition, job []domain.Segment) string {
		args, err := builder.Video(VideoParams{InputURL: "in.mkv", Rendition: r, Segments: job, OutputDir: "/tmp/v"})
		return mustArgs(t, args, err)
	}

	covered := build(rendition, segments[0:1])
	if filter := optionValue(t, covered, "-vf"); strings.Contains(filter, "tpad") || !strings.HasPrefix(filter, scale) {
		t.Fatalf("a job inside the video needs no padding: %s", filter)
	}
	exact := rendition
	exact.SourceEnd = 36
	if filter := optionValue(t, build(exact, segments[0:1]), "-vf"); strings.Contains(filter, "tpad") {
		t.Fatalf("a job that ends exactly with the video needs no padding: %s", filter)
	}

	padded := build(rendition, segments[0:3])
	if got, want := optionValue(t, padded, "-vf"), "tpad=stop_mode=clone:stop_duration=9.000000,"+scale; !strings.HasPrefix(got, want) || strings.Contains(got, "trim") {
		t.Fatalf("padded job filter = %s, want it to start with %s and not trim", got, want)
	}
	if !strings.Contains(padded, "-ss 29.999000 ") || optionValue(t, padded, "-to") != "47.999000" {
		t.Fatalf("a padded job seeks and ends like any transcode job: %s", padded)
	}

	near := build(rendition, segments[1:2])
	if got := optionValue(t, near, "-vf"); !strings.HasPrefix(got, "tpad=stop_mode=clone:stop_duration=3.000000,"+scale) || !strings.Contains(near, "-ss 35.999000 ") {
		t.Fatalf("a job starting well inside the video seeks to its own start: %s", near)
	}

	frozen := build(rendition, segments[2:4])
	if got, want := optionValue(t, frozen, "-vf"), "tpad=stop_mode=clone:stop_duration=14.000000,trim=start=41.999000,"+scale; !strings.HasPrefix(got, want) {
		t.Fatalf("frozen job filter = %s, want it to start with %s", got, want)
	}
	if !strings.Contains(frozen, "-ss 38.000000 ") || optionValue(t, frozen, "-to") != "52.999000" {
		t.Fatalf("a job past the video end must seek two seconds before that end: %s", frozen)
	}
	for _, option := range []string{"-segment_times", "-segment_start_number", "-force_key_frames", "-output_ts_offset"} {
		real := rendition
		real.SourceEnd = 0
		if got, want := optionValue(t, frozen, option), optionValue(t, build(real, segments[2:4]), option); got != want {
			t.Errorf("%s = %s, a job over real video uses %s", option, got, want)
		}
	}

	short := rendition
	short.SourceEnd = 1.5
	if early := build(short, []domain.Segment{{Index: 1, Start: 6, End: 12, Duration: 6}}); !strings.Contains(early, "-ss 0.000000 ") {
		t.Fatalf("the seek before a very early video end must not go negative: %s", early)
	}

	unknown := rendition
	unknown.SourceEnd = 0
	if filter := optionValue(t, build(unknown, segments[2:4]), "-vf"); strings.Contains(filter, "tpad") {
		t.Fatalf("without a known early end nothing is padded: %s", filter)
	}
}

func TestJobsKeepOnlyTheirOwnKindOfStream(t *testing.T) {
	builder := NewCommandBuilder(testHW)
	segments := []domain.Segment{{Index: 2, Start: 4, End: 6}}
	video, err := builder.Video(VideoParams{InputURL: "in", StreamIndex: 1, Rendition: domain.VideoRendition{Method: domain.Transcode, Width: 640, Height: 360, Bitrate: 600000, PeakBitrate: 900000, BufferSize: 1800000, Level: 30}, Segments: segments})
	if err != nil {
		t.Fatalf("video args: %v", err)
	}
	copied, err := builder.Video(VideoParams{InputURL: "in", StreamIndex: 1, Rendition: domain.VideoRendition{Method: domain.DirectStream}, Segments: segments, Landing: landing(4)})
	if err != nil {
		t.Fatalf("copied video args: %v", err)
	}
	audio, err := builder.Audio(AudioParams{InputURL: "in", StreamIndex: 1, Rendition: domain.AudioRendition{Method: domain.Transcode, Channels: 2, Bitrate: 128000}, Segments: segments})
	if err != nil {
		t.Fatalf("audio args: %v", err)
	}
	for name, test := range map[string]struct {
		args []string
		want string
	}{"video": {video, "-map 0:1 -an -sn -dn"}, "copied video": {copied, "-map 0:1 -an -sn -dn"}, "audio": {audio, "-map 0:1 -vn -sn -dn"}} {
		if joined := strings.Join(test.args, " "); !strings.Contains(joined, test.want) {
			t.Errorf("%s args must disable every other stream kind (%q): %s", name, test.want, joined)
		}
	}
}
