package ffmpeg

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/source"
	"github.com/eleven-am/goshl/internal/testmedia"
)

const channelTonemapChain = "zscale=w=%d:h=%d:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,setsar=1,format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv"

func channelSegment(index int) domain.Segment {
	start := domain.ChannelFrameTime(index*48, 24000, 1001)
	end := domain.ChannelFrameTime((index+1)*48, 24000, 1001)
	return domain.Segment{Index: index, Start: start, End: end, Duration: end - start}
}

func channelParams() ChannelParams {
	overlap, trailing := channelSegment(294), channelSegment(297)
	return ChannelParams{
		InputURL:         "/media/item.mp4",
		VideoStreamIndex: 0,
		Video:            domain.VideoRendition{Name: "channel-720p-a0-1a18aabd6d89", Width: 1280, Height: 720, Bitrate: 2_500_000, PeakBitrate: 2_500_000, BufferSize: 5_000_000, Level: 31, Method: domain.Transcode},
		Audio:            domain.AudioRendition{Name: "channel-720p-a0-1a18aabd6d89", Codec: "aac", Bitrate: 160_000, Channels: 2, StreamIndex: 1, Method: domain.Transcode},
		Spec:             domain.ChannelSpec{FormatVersion: 1, Width: 1280, Height: 720, PictureWidth: 1056, PictureHeight: 720, FrameRateNum: 24000, FrameRateDen: 1001, SegmentFrames: 48, SegmentsPerJob: 2, TotalFrames: 48000},
		Segments:         []domain.Segment{channelSegment(295), channelSegment(296)},
		Overlap:          &overlap,
		Trailing:         &trailing,
		OutputDir:        "/tmp/out",
	}
}

func channelArgs(t *testing.T, params ChannelParams) string {
	t.Helper()
	args, err := NewCommandBuilder(hwaccel.NewConfig(domain.AccelNone)).Channel(params)
	if err != nil {
		t.Fatal(err)
	}
	input, err := source.InputArgs(params.InputURL)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(strings.Join(args, " "), strings.Join(input, " "), "-i "+params.InputURL)
}

func TestChannelCommandMatchesTheProvenArguments(t *testing.T) {
	got := channelArgs(t, channelParams())
	want := "-nostats -hide_banner -loglevel warning -ss 588.587000 -i /media/item.mp4 -ss 588.586667 -i /media/item.mp4 -to 595.594000 " +
		"-copyts -start_at_zero -muxdelay 0 -output_ts_offset 10.000000 -map 0:0 -map 1:1 -sn -dn -map_metadata -1 -map_chapters -1 " +
		"-metadata service_provider=goshl -metadata service_name=channel -c:v libx264 -preset superfast " +
		"-vf fps=24000/1001:start_time=588.588000,scale=1056:720:out_color_matrix=bt709:out_range=tv,setsar=1,format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv,pad=1280:720:112:0:color=black,sidedata=mode=delete " +
		"-profile:v high -level:v 3.1 -pix_fmt yuv420p -b:v 2500000 -maxrate 2500000 -bufsize 5000000 -g 48 -bf 0 -sc_threshold 0 -a53cc 0 " +
		"-color_primaries bt709 -color_trc bt709 -colorspace bt709 -color_range tv -fps_mode passthrough " +
		"-force_key_frames 588.587000,590.589000,592.591000,594.593000 " +
		"-c:a aac -ac 2 -ar 48000 -b:a 160000 -af aresample=48000,aresample=48000:first_pts=28252160,apad " +
		"-fflags +bitexact -flags:v +bitexact -flags:a +bitexact -f segment -segment_time_delta 0.010000 -segment_format mpegts " +
		"-segment_format_options mpegts_flags=+initial_discontinuity -segment_list_type csv -segment_list pipe:1 -segment_start_number 294 " +
		"-segment_times 2.002000,4.004000,6.006000 /tmp/out/segment-%05d.ts"
	if got != want {
		t.Fatalf("channel command:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "-threads") {
		t.Fatal("the channel encoder must be multi-threaded")
	}
	if strings.Contains(got, "enc_time_base") {
		t.Fatal("the channel command must not use the demuxer time base")
	}
}

func TestChannelCommandVariants(t *testing.T) {
	first := channelParams()
	first.Overlap = nil
	first.Segments = []domain.Segment{channelSegment(0), channelSegment(1)}
	trailing := channelSegment(2)
	first.Trailing = &trailing
	got := channelArgs(t, first)
	for _, part := range []string{"-ss 0.000000 -i /media/item.mp4 -ss 0.000000 -i", "fps=24000/1001:start_time=0.000000,", "-force_key_frames 0.000000,2.001000,4.003000 ", "first_pts=0,apad", "-segment_start_number 0 ", "-segment_times 2.002000,4.004000 "} {
		if !strings.Contains(got, part) {
			t.Errorf("first batch lacks %q in %s", part, got)
		}
	}

	last := channelParams()
	last.Trailing = nil
	got = channelArgs(t, last)
	for _, part := range []string{"-to 594.593000 ", ",tpad=stop_mode=clone:stop_duration=3.002000,scale=", "-segment_times 2.002000,4.004000 "} {
		if !strings.Contains(got, part) {
			t.Errorf("last batch lacks %q in %s", part, got)
		}
	}

	shifted := channelParams()
	shifted.Spec.Phase = -0.020708
	if got = channelArgs(t, shifted); !strings.Contains(got, "-vf setpts=PTS+0.020708/TB,fps=") {
		t.Errorf("a late Matroska start must be shifted onto the grid: %s", got)
	}
	shifted.Spec.Phase = 0.001
	if got = channelArgs(t, shifted); !strings.Contains(got, "-vf setpts=PTS-0.001000/TB,fps=") {
		t.Errorf("a positive phase must be subtracted: %s", got)
	}

	full := channelParams()
	full.Spec.PictureWidth = 1280
	if got = channelArgs(t, full); strings.Contains(got, "pad=") || !strings.Contains(got, "scale=1280:720:") {
		t.Errorf("a picture that fills the canvas needs no pad: %s", got)
	}

	silent := channelParams()
	silent.Spec.Silent = true
	got = channelArgs(t, silent)
	for _, part := range []string{"-f lavfi -i anullsrc=r=48000:cl=stereo ", "-map 1:0 ", "-af asetpts=N+28252160 "} {
		if !strings.Contains(got, part) {
			t.Errorf("silent source lacks %q in %s", part, got)
		}
	}
	ended := channelParams()
	ended.Audio.SourceEnd = 300
	if got = channelArgs(t, ended); !strings.Contains(got, "anullsrc") {
		t.Errorf("a batch past the end of the audio must generate silence: %s", got)
	}

	late := channelParams()
	late.Video.SourceEnd = 500
	got = channelArgs(t, late)
	for _, part := range []string{"-ss 498.000000 -i /media/item.mp4 -ss 588.586667 ", "-vf fps=24000/1001,tpad=stop_mode=clone:stop_duration=", ",trim=start=588.587000,scale="} {
		if !strings.Contains(got, part) {
			t.Errorf("a batch past the end of the video lacks %q in %s", part, got)
		}
	}

	retagged := channelParams()
	retagged.Video.Colour = domain.ColourPlan{AssumeMatrix: "smpte170m"}
	if got = channelArgs(t, retagged); !strings.Contains(got, ",setparams=colorspace=smpte170m,scale=1056:720:") {
		t.Errorf("an untagged source must be tagged before scaling: %s", got)
	}
}

func TestChannelCommandToneMapsAtTheFittedSize(t *testing.T) {
	builder := NewCommandBuilder(hwaccel.NewConfig(domain.AccelNone))
	builder.DetectTonemap = func(context.Context) (string, error) { return channelTonemapChain, nil }
	params := channelParams()
	params.Video.Colour = domain.ColourPlan{Tonemap: true}
	args, err := builder.Channel(params)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, ",zscale=w=1056:h=720:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,setsar=1,format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv,pad=1280:720:112:0:color=black,sidedata=mode=delete ") {
		t.Fatalf("tone-mapped channel chain: %s", got)
	}
}

func TestChannelCommandRejectsIncompleteJobs(t *testing.T) {
	builder := NewCommandBuilder(hwaccel.NewConfig(domain.AccelNone))
	cases := map[string]struct {
		spoil func(*ChannelParams)
		want  error
	}{
		"no segments":         {func(p *ChannelParams) { p.Segments = nil }, ErrNoSegments},
		"detached overlap":    {func(p *ChannelParams) { p.Overlap.End -= 1 }, ErrOverlapNotAdjacent},
		"detached trailing":   {func(p *ChannelParams) { p.Trailing.Start += 1 }, ErrTrailingNotAdjacent},
		"no canvas":           {func(p *ChannelParams) { p.Spec.Width = 0 }, ErrChannelIncomplete},
		"picture over canvas": {func(p *ChannelParams) { p.Spec.PictureWidth = 1282 }, ErrChannelIncomplete},
		"no frame rate":       {func(p *ChannelParams) { p.Spec.FrameRateDen = 0 }, ErrChannelIncomplete},
		"no bitrate":          {func(p *ChannelParams) { p.Video.Bitrate = 0 }, ErrRenditionIncomplete},
		"no audio bitrate":    {func(p *ChannelParams) { p.Audio.Bitrate = 0 }, ErrRenditionIncomplete},
	}
	for name, c := range cases {
		params := channelParams()
		c.spoil(&params)
		if _, err := builder.Channel(params); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

func TestChannelCommandSnapsSourceTimestampsToTheSourceGrid(t *testing.T) {
	cases := map[float64]string{
		24:             "-vf settb=AVTB,setpts=round(PTS*TB*24/1)*1/(24*TB),fps=",
		25:             "-vf settb=AVTB,setpts=round(PTS*TB*25/1)*1/(25*TB),fps=",
		24000.0 / 1001: "-vf settb=AVTB,setpts=round(PTS*TB*24000/1001)*1001/(24000*TB),fps=",
		30000.0 / 1001: "-vf settb=AVTB,setpts=round(PTS*TB*30000/1001)*1001/(30000*TB),fps=",
		0:              "-vf fps=",
		23.5:           "-vf fps=",
	}
	for rate, want := range cases {
		params := channelParams()
		params.Video.FrameRate = rate
		if got := channelArgs(t, params); !strings.Contains(got, want) {
			t.Errorf("source rate %v lacks %q in %s", rate, want, got)
		}
	}
	params := channelParams()
	params.Video.FrameRate = 24
	params.Spec.Phase = 0.001
	if got := channelArgs(t, params); !strings.Contains(got, "-vf setpts=PTS-0.001000/TB,settb=AVTB,setpts=round(") {
		t.Errorf("the phase shift must come before the snap: %s", got)
	}
}

var fpsFilterReport = regexp.MustCompile(`(\d+) frames in, (\d+) frames out; (\d+) frames dropped, (\d+) frames duplicated`)

func channelFilterReport(t *testing.T, params ChannelParams) (in, out, dropped, duplicated int) {
	t.Helper()
	args, err := NewCommandBuilder(hwaccel.NewConfig(domain.AccelNone)).Channel(params)
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range args {
		if arg == "-loglevel" {
			args[i+1] = "verbose"
		}
	}
	output, err := exec.Command("ffmpeg", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg failed: %v: %s", err, output)
	}
	for _, match := range fpsFilterReport.FindAllStringSubmatch(string(output), -1) {
		values := make([]int, 4)
		for i := range values {
			values[i], _ = strconv.Atoi(match[i+1])
		}
		if values[0] > 0 {
			return values[0], values[1], values[2], values[3]
		}
	}
	t.Fatalf("no fps filter report in %s", output)
	return
}

func TestChannelDeJittersA24FpsMatroskaSource(t *testing.T) {
	path := testmedia.H264(t, testmedia.Spec{Container: "mkv", FPS: "24", Duration: 100, KeyframeFrames: []int{0}, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	build := func(rate float64) ChannelParams {
		params := channelParams()
		params.InputURL = path
		params.Video.FrameRate = rate
		params.Spec.TotalFrames = 100000
		params.Segments = nil
		for i := 0; i < 50; i++ {
			params.Segments = append(params.Segments, channelSegment(i))
		}
		params.Overlap, params.Trailing = nil, nil
		params.OutputDir = t.TempDir()
		return params
	}

	in, out, dropped, duplicated := channelFilterReport(t, build(0))
	if duplicated == 0 {
		t.Fatalf("the fixture must show the jitter: %d in, %d out, %d dropped, %d duplicated", in, out, dropped, duplicated)
	}

	in, out, dropped, duplicated = channelFilterReport(t, build(24))
	if duplicated != 0 {
		t.Errorf("de-jittered conversion duplicated %d frames", duplicated)
	}
	if want := int(math.Ceil(float64(in) * (1 - 1000.0/1001))); dropped != in-out || dropped > want {
		t.Errorf("de-jittered conversion dropped %d of %d frames (%d out), want only the %d the rate change needs", dropped, in, out, want)
	}
}

func TestChannelBatchJoinsStayExactWithAMultiThreadedEncoder(t *testing.T) {
	path := testmedia.H264(t, testmedia.Spec{Container: "mp4", Duration: 14, KeyframeFrames: []int{0, 100, 200}, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	build := func(segments []domain.Segment, overlap, trailing *domain.Segment, dir string) ChannelParams {
		params := channelParams()
		params.InputURL = path
		params.Video.Width, params.Video.Height = 320, 180
		params.Video.Bitrate, params.Video.PeakBitrate, params.Video.BufferSize = 400_000, 400_000, 800_000
		params.Spec.Width, params.Spec.Height, params.Spec.PictureWidth, params.Spec.PictureHeight = 320, 180, 320, 180
		params.Spec.TotalFrames = 336
		params.Segments, params.Overlap, params.Trailing, params.OutputDir = segments, overlap, trailing, dir
		return params
	}
	segment := func(i int) *domain.Segment { s := channelSegment(i); return &s }
	firstDir, secondDir := t.TempDir(), t.TempDir()
	first := build([]domain.Segment{*segment(0), *segment(1)}, nil, segment(2), firstDir)
	second := build([]domain.Segment{*segment(2), *segment(3)}, segment(1), segment(4), secondDir)

	for _, params := range []ChannelParams{first, second} {
		if joined := channelArgs(t, params); strings.Contains(joined, "-threads") {
			t.Fatalf("encoder threads must stay at the default: %s", joined)
		}
		args, err := NewCommandBuilder(hwaccel.NewConfig(domain.AccelNone)).Channel(params)
		runJob(t, args, err)
	}

	offset := domain.OutputTimelineOffset.Seconds()
	tolerance := 2 * integrationTick
	videoFrame := 1001.0 / 24000
	audioFrame := 1024.0 / 48000
	dirOf := func(index int) string {
		if index >= 2 {
			return secondDir
		}
		return firstDir
	}
	var previousVideo, previousAudio []float64
	for index := 1; index <= 3; index++ {
		video, keys := segmentPTS(t, dirOf(index), index, "v:0")
		audio, _ := segmentPTS(t, dirOf(index), index, "a:0")
		want := channelSegment(index).Start + offset
		if math.Abs(video[0]-want) > tolerance || len(keys) != 1 || math.Abs(keys[0]-want) > tolerance {
			t.Errorf("segment %d starts at %.6f with keyframes %v, want %.6f", index, video[0], keys, want)
		}
		for i := 1; i < len(video); i++ {
			if step := video[i] - video[i-1]; math.Abs(step-videoFrame) > tolerance {
				t.Fatalf("segment %d video step %d is %.6f, want %.6f", index, i, step, videoFrame)
			}
		}
		if len(video) != 48 {
			t.Errorf("segment %d holds %d frames, want 48", index, len(video))
		}
		if previousVideo != nil {
			if gap := video[0] - previousVideo[len(previousVideo)-1]; math.Abs(gap-videoFrame) > tolerance {
				t.Errorf("video join %d/%d advances by %.6f, want %.6f", index-1, index, gap, videoFrame)
			}
			if gap := audio[0] - previousAudio[len(previousAudio)-1]; math.Abs(gap-audioFrame) > tolerance {
				t.Errorf("audio join %d/%d advances by %.6f, want %.6f", index-1, index, gap, audioFrame)
			}
			seen := map[int64]bool{}
			for _, value := range append(append([]float64{}, previousVideo...), previousAudio...) {
				seen[int64(math.Round(value*90000))] = true
			}
			for _, value := range append(append([]float64{}, video...), audio...) {
				if seen[int64(math.Round(value*90000))] {
					t.Errorf("segments %d and %d share PTS %.6f", index-1, index, value)
				}
			}
		}
		previousVideo, previousAudio = video, audio
	}
}
