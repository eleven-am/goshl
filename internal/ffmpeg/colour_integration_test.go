package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/testmedia"
)

var sdrTags = testmedia.ColourTags{PixelFormat: "yuv420p", Range: "tv", Matrix: "bt709", Transfer: "bt709", Primaries: "bt709"}

const standInTonemap = "scale=%d:%d:out_color_matrix=bt709:out_range=tv:out_primaries=bt709:out_transfer=bt709,setsar=1,format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv"

func colourRendition(spec testmedia.ColourSpec, width, height int) domain.VideoRendition {
	bitDepth := 8
	if strings.Contains(spec.PixelFormat, "10") {
		bitDepth = 10
	}
	stream := domain.VideoStream{Width: spec.Width, Height: spec.Height, BitDepth: bitDepth, ColorSpace: spec.Matrix, ColorTransfer: spec.Transfer, ColorPrimaries: spec.Primaries}
	return domain.VideoRendition{Method: domain.Transcode, Width: width, Height: height, Bitrate: 400000, PeakBitrate: 600000, BufferSize: 1200000, Level: 30, Colour: domain.PlanColour(stream)}
}

func softwareBuilder(detect func(context.Context) (string, error)) *CommandBuilder {
	builder := NewCommandBuilder(hwaccel.NewConfig(domain.AccelNone))
	if detect != nil {
		builder.DetectTonemap = detect
	}
	return builder
}

func TestSDROutputsCarryColourTags(t *testing.T) {
	cases := map[string]testmedia.ColourSpec{
		"untagged standard definition": {Width: 320, Height: 180},
		"untagged high definition":     {Width: 1280, Height: 720},
		"tagged bt601":                 {Width: 320, Height: 180, Primaries: "smpte170m", Transfer: "smpte170m", Matrix: "smpte170m", Range: "tv"},
		"full range":                   {Width: 320, Height: 180, PixelFormat: "yuvj420p"},
		"ten bit":                      {Width: 320, Height: 180, PixelFormat: "yuv420p10le", Container: "mkv"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			spec.Duration = 2
			input := testmedia.Colour(t, spec)
			dir := t.TempDir()
			args, err := softwareBuilder(nil).Video(VideoParams{InputURL: input, Rendition: colourRendition(spec, 320, 180), Segments: []domain.Segment{{Index: 0, Start: 0, End: 2, Duration: 2}}, OutputDir: dir})
			runJob(t, args, err)
			if got := testmedia.Tags(t, filepath.Join(dir, "segment-00000.ts")); got != sdrTags {
				t.Fatalf("output colour %+v, want %+v", got, sdrTags)
			}
		})
	}
}

func TestUntaggedHighDefinitionSourceKeepsItsPixels(t *testing.T) {
	spec := testmedia.ColourSpec{Width: 1280, Height: 720, Duration: 1}
	input := testmedia.Colour(t, spec)
	filter := videoFilter(hwaccel.NewConfig(domain.AccelNone), hwaccel.NewConfig(domain.AccelNone).ScaleFilter, colourRendition(spec, 1280, 720))
	hash := func(chain string) string {
		out, err := exec.Command("ffmpeg", "-v", "error", "-i", input, "-vf", chain, "-frames:v", "5", "-f", "md5", "-").Output()
		if err != nil {
			t.Fatalf("hash %s: %v", chain, err)
		}
		return string(out)
	}
	if got, want := hash(filter), hash("scale=1280:720,setsar=1,format=yuv420p"); got != want {
		t.Fatalf("untagged 720p pixels changed: %s vs %s", got, want)
	}
}

func TestHDRSourceFailsExplicitlyWithoutTonemapFilter(t *testing.T) {
	spec := testmedia.ColourSpec{Width: 320, Height: 180, PixelFormat: "yuv420p10le", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}
	missing := func(context.Context) (string, error) {
		return "", fmt.Errorf("%w: ffmpeg has no zscale filter", hwaccel.ErrTonemapUnavailable)
	}
	_, err := softwareBuilder(missing).Video(VideoParams{InputURL: "/media/hdr.mkv", Rendition: colourRendition(spec, 320, 180), Segments: []domain.Segment{{Index: 0, Start: 0, End: 2, Duration: 2}}, OutputDir: t.TempDir()})
	if !errors.Is(err, hwaccel.ErrTonemapUnavailable) {
		t.Fatalf("HDR job without a tonemap filter returned %v, want ErrTonemapUnavailable", err)
	}
}

func TestDeviceFrameAcceleratorRoutesHDRToSoftware(t *testing.T) {
	spec := testmedia.ColourSpec{Width: 320, Height: 180, PixelFormat: "yuv420p10le", Primaries: "bt2020", Transfer: "arib-std-b67", Matrix: "bt2020nc"}
	builder := NewCommandBuilder(hwaccel.NewConfig(domain.AccelVAAPI))
	builder.DetectTonemap = func(context.Context) (string, error) { return standInTonemap, nil }
	params := VideoParams{InputURL: "/media/hlg.mkv", Rendition: colourRendition(spec, 320, 180), Segments: []domain.Segment{{Index: 0, Start: 0, End: 2, Duration: 2}}, OutputDir: t.TempDir()}
	args, err := builder.Video(params)
	if err != nil {
		t.Fatal(err)
	}
	software := softwareBuilder(func(context.Context) (string, error) { return standInTonemap, nil })
	want, err := software.Video(params)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, want) {
		t.Fatalf("device-frame HDR job must equal the software job\n got %v\nwant %v", args, want)
	}
	params.Rendition.Colour = domain.ColourPlan{}
	sdr, err := builder.Video(params)
	if err != nil || !slices.Contains(sdr, "h264_vaapi") {
		t.Fatalf("SDR job must stay on the accelerator: %v %v", sdr, err)
	}
}

func TestTonemappedJobsKeepThePlannedTimeline(t *testing.T) {
	help, _ := exec.Command("ffmpeg", "-hide_banner", "-h", "filter=scale").Output()
	if !strings.Contains(string(help), "out_transfer") {
		t.Skip("stand-in conversion needs the scale filter of ffmpeg 8")
	}
	for _, transfer := range []string{"smpte2084", "arib-std-b67"} {
		t.Run(transfer, func(t *testing.T) {
			spec := testmedia.ColourSpec{Container: "mkv", Width: 320, Height: 180, PixelFormat: "yuv420p10le", Primaries: "bt2020", Transfer: transfer, Matrix: "bt2020nc", Range: "tv", KeyframeFrames: []int{0, 50, 101, 149, 200, 251}, Duration: 12}
			input := testmedia.Colour(t, spec)
			if got := testmedia.Tags(t, input); got.Transfer != transfer || got.PixelFormat != "yuv420p10le" {
				t.Fatalf("fixture is not HDR: %+v", got)
			}
			start := testmedia.FormatStart(t, input)
			var keyframes, frames []float64
			for _, packet := range testmedia.Packets(t, input, "v:0") {
				frames = append(frames, packet.PTS-start)
				if packet.Key {
					keyframes = append(keyframes, packet.PTS-start)
				}
			}
			sort.Float64s(keyframes)
			sort.Float64s(frames)
			segments := playlist.CalculateSegments(keyframes, 12, 2)
			builder := softwareBuilder(func(context.Context) (string, error) { return standInTonemap, nil })
			dir := t.TempDir()
			for _, job := range [][]domain.Segment{segments[0:2], segments[2:4], segments[4:]} {
				args, err := builder.Video(VideoParams{InputURL: input, Rendition: colourRendition(spec, 320, 180), Segments: job, OutputDir: dir})
				if err == nil && !slices.Contains(args, videoFilter(softwareBuilder(nil).HWAccel, standInTonemap, colourRendition(spec, 320, 180))) {
					t.Fatalf("job -vf does not carry the injected colour chain: %v", args)
				}
				if listed := runJob(t, args, err); len(listed) != len(job) {
					t.Fatalf("job listed %v, want %d segments", listed, len(job))
				}
			}
			offset := domain.OutputTimelineOffset.Seconds()
			var all []float64
			for index, segment := range segments {
				file := filepath.Join(dir, fmt.Sprintf("segment-%05d.ts", index))
				if got := testmedia.Tags(t, file); got != sdrTags {
					t.Fatalf("segment %d colour %+v, want %+v", index, got, sdrTags)
				}
				pts, keys := segmentPTS(t, dir, index, "v:0")
				if len(keys) != 1 || keys[0] != pts[0] || math.Abs(keys[0]-offset-segment.Start) > 0.001 {
					t.Errorf("segment %d keyframes %v, want one at %.6f", index, keys, segment.Start+offset)
				}
				all = append(all, pts...)
			}
			if len(all) != len(frames) {
				t.Fatalf("output has %d frames, source %d", len(all), len(frames))
			}
			for i := range all {
				if math.Abs(all[i]-offset-frames[i]) > 0.001 {
					t.Fatalf("frame %d at %.6f, source %.6f", i, all[i]-offset, frames[i])
				}
			}
		})
	}
}

func TestHDRSourceTranscodesToTaggedSDR(t *testing.T) {
	testmedia.Require(t)
	if _, err := hwaccel.DetectTonemap(context.Background()); err != nil {
		t.Skipf("tone mapping is unavailable on this host: %v", err)
	}
	for _, transfer := range []string{"smpte2084", "arib-std-b67"} {
		t.Run(transfer, func(t *testing.T) {
			spec := testmedia.ColourSpec{Container: "mkv", Width: 320, Height: 180, PixelFormat: "yuv420p10le", Primaries: "bt2020", Transfer: transfer, Matrix: "bt2020nc", Range: "tv", Duration: 2}
			input := testmedia.Colour(t, spec)
			dir := t.TempDir()
			args, err := softwareBuilder(nil).Video(VideoParams{InputURL: input, Rendition: colourRendition(spec, 320, 180), Segments: []domain.Segment{{Index: 0, Start: 0, End: 2, Duration: 2}}, OutputDir: dir})
			runJob(t, args, err)
			if got := testmedia.Tags(t, filepath.Join(dir, "segment-00000.ts")); got != sdrTags {
				t.Fatalf("output colour %+v, want %+v", got, sdrTags)
			}
		})
	}
}

func TestHDRToneMapDarkensShadowsBelowRetag(t *testing.T) {
	testmedia.Require(t)
	if _, err := hwaccel.DetectTonemap(context.Background()); err != nil {
		t.Skipf("tone mapping is unavailable on this host: %v", err)
	}
	spec := testmedia.ColourSpec{Container: "mkv", Width: 320, Height: 180, PixelFormat: "yuv420p10le", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv", Duration: 2, FlatLuma10: 327}
	input := testmedia.Colour(t, spec)
	dir := t.TempDir()
	args, err := softwareBuilder(nil).Video(VideoParams{InputURL: input, Rendition: colourRendition(spec, 320, 180), Segments: []domain.Segment{{Index: 0, Start: 0, End: 2, Duration: 2}}, OutputDir: dir})
	runJob(t, args, err)
	raw, err := exec.Command("ffmpeg", "-v", "error", "-i", filepath.Join(dir, "segment-00000.ts"), "-frames:v", "1", "-vf", "format=gray", "-f", "rawvideo", "-").Output()
	if err != nil || len(raw) == 0 {
		t.Fatalf("decode tone mapped frame: %v", err)
	}
	var sum int
	for _, value := range raw {
		sum += int(value)
	}
	mean := float64(sum) / float64(len(raw))
	if mean >= 65 || mean <= 16 {
		t.Fatalf("tone mapped 10-nit PQ patch has mean luma %.1f, want within (16, 65); a plain retag measures 75 on this fixture", mean)
	}
}

func TestVideoToolboxKeepsHardwareCodecsForColourWork(t *testing.T) {
	config := hwaccel.NewConfig(domain.AccelVideoToolbox)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if available, err := hwaccel.Detect(ctx); err != nil || !slices.Contains(available, domain.AccelVideoToolbox) || hwaccel.Validate(ctx, config) != nil {
		t.Skip("VideoToolbox is unavailable")
	}
	help, _ := exec.Command("ffmpeg", "-hide_banner", "-h", "filter=scale").Output()
	cases := map[string]testmedia.ColourSpec{
		"sdr": {Width: 640, Height: 360},
	}
	if strings.Contains(string(help), "out_transfer") {
		cases["hdr"] = testmedia.ColourSpec{Container: "mkv", Width: 640, Height: 360, PixelFormat: "yuv420p10le", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"}
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			spec.Duration = 2
			input := testmedia.Colour(t, spec)
			builder := NewCommandBuilder(config)
			builder.DetectTonemap = func(context.Context) (string, error) { return standInTonemap, nil }
			rendition := colourRendition(spec, 640, 360)
			rendition.Level = 31
			dir := t.TempDir()
			args, err := builder.Video(VideoParams{InputURL: input, Rendition: rendition, Segments: []domain.Segment{{Index: 0, Start: 0, End: 2, Duration: 2}}, OutputDir: dir})
			if !slices.Contains(args, "h264_videotoolbox") || !slices.Contains(args, "videotoolbox") {
				t.Fatalf("job left the accelerator: %v", args)
			}
			runJob(t, args, err)
			if got := testmedia.Tags(t, filepath.Join(dir, "segment-00000.ts")); got != sdrTags {
				t.Fatalf("output colour %+v, want %+v", got, sdrTags)
			}
		})
	}
}
