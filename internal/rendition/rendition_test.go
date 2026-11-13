package rendition

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func alignedMeta(video domain.VideoStream) *domain.Metadata {
	return &domain.Metadata{Duration: 18, Keyframes: []float64{0, 6, 12}, Video: video}
}

func mustVideo(t *testing.T, video domain.VideoStream) []domain.VideoRendition {
	t.Helper()
	renditions, _, err := GenerateVideo(alignedMeta(video), 6)
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	return renditions
}

func TestGenerateVideo_DirectStreamAndClamping(t *testing.T) {
	src := domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 10_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 40, StartsWithKeyframe: true}

	renditions := mustVideo(t, src)

	if len(renditions) == 0 || renditions[0].Name != "1080p" {
		t.Fatalf("expected first rendition to be 1080p, got %#v", renditions)
	}

	top := renditions[0]
	if top.Method != domain.DirectStream {
		t.Fatalf("expected direct stream when codec matches source, got %s", top.Method)
	}
	if top.Bitrate != 10_000_000 {
		t.Fatalf("direct stream bitrate should match the source, got %d", top.Bitrate)
	}

	var has720 bool
	for _, r := range renditions {
		if r.Name == "720p" {
			has720 = true
			if r.Width != 1280 {
				t.Fatalf("expected 720p width 1280, got %d", r.Width)
			}
			if r.Bitrate != 4_000_000 {
				t.Fatalf("expected 720p bitrate clamp to 4Mbps, got %d", r.Bitrate)
			}
		}
	}
	if !has720 {
		t.Fatalf("missing 720p rendition in %#v", renditions)
	}
}

func TestGenerateVideo_EstimatesBitrateAndEvenWidth(t *testing.T) {
	src := domain.VideoStream{Codec: "hevc", Width: 1919, Height: 800, Bitrate: 0}

	renditions := mustVideo(t, src)

	var r720 domain.VideoRendition
	for _, r := range renditions {
		if r.Name == "720p" {
			r720 = r
		}
	}
	if r720.Name == "" {
		t.Fatalf("expected 720p rendition to be generated")
	}
	if r720.Width%2 != 0 {
		t.Fatalf("width should be even, got %d", r720.Width)
	}
	if r720.Width != 1728 {
		t.Fatalf("width should be aspect-corrected to 1728, got %d", r720.Width)
	}
	if r720.Bitrate < 1_000_000 || r720.Bitrate > 4_000_000 {
		t.Fatalf("bitrate should respect 720p bounds, got %d", r720.Bitrate)
	}
}

func TestGenerateAudio_IncludesSurroundAndPassthrough(t *testing.T) {
	audio := domain.AudioStream{Codec: "ac3", Channels: 6, Bitrate: 640_000}
	renditions := GenerateAudioTracks([]domain.AudioStream{audio})

	if len(renditions) != 3 {
		t.Fatalf("expected stereo, surround, and passthrough, got %d renditions", len(renditions))
	}

	stereo := renditions[0]
	if stereo.Name != "aac_stereo" || stereo.Method != domain.Transcode || stereo.Bitrate != 128_000 || !stereo.Default {
		t.Fatalf("unexpected stereo rendition: %#v", stereo)
	}

	var surround, passthrough *domain.AudioRendition
	for i := range renditions {
		switch renditions[i].Name {
		case "aac_surround":
			surround = &renditions[i]
		case "ac3_passthrough":
			passthrough = &renditions[i]
		}
	}

	if surround == nil || surround.Bitrate != 384_000 || surround.Channels != 6 {
		t.Fatalf("unexpected surround rendition: %#v", surround)
	}
	if passthrough == nil || passthrough.Method != domain.DirectStream || passthrough.Bitrate != 640_000 {
		t.Fatalf("unexpected passthrough rendition: %#v", passthrough)
	}
}

func TestGenerateAudioTracksDisambiguatesMultipleStreams(t *testing.T) {
	renditions := GenerateAudioTracks([]domain.AudioStream{
		{Codec: "aac", Channels: 2, Bitrate: 128_000, Language: "eng"},
		{Codec: "ac3", Channels: 6, Bitrate: 640_000, Language: "spa"},
	})

	if len(renditions) == 0 {
		t.Fatalf("expected audio renditions")
	}

	var defaultCount int
	seen := map[string]bool{}
	for _, r := range renditions {
		if seen[r.Name] {
			t.Fatalf("duplicate rendition name %q in %#v", r.Name, renditions)
		}
		seen[r.Name] = true
		if r.Default {
			defaultCount++
			if r.StreamIndex != 0 {
				t.Fatalf("default should be on first stream, got %#v", r)
			}
		}
	}

	if !seen["audio0_aac_stereo"] || !seen["audio1_aac_surround"] || !seen["audio1_ac3_passthrough"] {
		t.Fatalf("missing expected prefixed renditions: %#v", renditions)
	}
	if defaultCount != 1 {
		t.Fatalf("expected exactly one default rendition, got %d", defaultCount)
	}
}

func TestGenerateVideoIncludesSourceSizedLowResolution(t *testing.T) {
	r := mustVideo(t, domain.VideoStream{Codec: "h264", Width: 426, Height: 240, Bitrate: 400_000, PixelFormat: "yuv420p", Profile: "High", Level: 30, FrameRate: 24, StartsWithKeyframe: true})
	if len(r) != 1 {
		t.Fatalf("expected one source-sized rendition, got %#v", r)
	}
	if r[0].Name != "240p" || r[0].Width != 426 || r[0].Height != 240 {
		t.Fatalf("unexpected source rendition: %#v", r[0])
	}
}

func TestGenerateVideoIncludesNonstandardSourceRung(t *testing.T) {
	r := mustVideo(t, domain.VideoStream{Codec: "h264", Width: 2560, Height: 1440, Bitrate: 12_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 51, FrameRate: 60, StartsWithKeyframe: true})
	if len(r) == 0 || r[0].Name != "1440p" || r[0].Method != domain.DirectStream || r[0].Bitrate != 12_000_000 {
		t.Fatalf("expected unchanged source rung first, got %#v", r)
	}
}

func TestGenerateVideoRejectsIncompatibleDirectStream(t *testing.T) {
	r := mustVideo(t, domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 5_000_000, PixelFormat: "yuv420p10le", Profile: "High 10", Level: 51})
	if len(r) == 0 || r[0].Method != domain.Transcode {
		t.Fatalf("expected incompatible H264 to transcode, got %#v", r)
	}
}

func TestGenerateVideoRejectsUnknownOrExcessiveH264Level(t *testing.T) {
	for _, level := range []int{0, 60} {
		renditions := mustVideo(t, domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 5_000_000, PixelFormat: "yuv420p", Profile: "High", Level: level, StartsWithKeyframe: true})
		if len(renditions) == 0 || renditions[0].Method != domain.Transcode {
			t.Fatalf("level %d must not be remuxed directly: %#v", level, renditions)
		}
	}
}

func TestGenerateVideoRejectsRotatedDirectStream(t *testing.T) {
	renditions := mustVideo(t, domain.VideoStream{Codec: "h264", Width: 1080, Height: 1920, Bitrate: 5_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 40, Rotation: 90, StartsWithKeyframe: true})
	if len(renditions) == 0 || renditions[0].Method != domain.Transcode {
		t.Fatalf("rotated source must be normalized by transcoding: %#v", renditions)
	}
}

func TestGenerateVideoRejectsAnamorphicDirectStream(t *testing.T) {
	renditions := mustVideo(t, domain.VideoStream{Codec: "h264", Width: 768, Height: 576, Bitrate: 2_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 31, SampleAspect: "16:15", StartsWithKeyframe: true})
	if len(renditions) == 0 || renditions[0].Method != domain.Transcode {
		t.Fatalf("anamorphic source must be normalized by transcoding: %#v", renditions)
	}
}

func h264Source(width, height, bitrate int) domain.VideoStream {
	return domain.VideoStream{Codec: "h264", Width: width, Height: height, Bitrate: bitrate, PixelFormat: "yuv420p", Profile: "High", Level: 40, FrameRate: 24, StartsWithKeyframe: true}
}

func hevcSource(width, height, bitrate int, frameRate float64) domain.VideoStream {
	return domain.VideoStream{Codec: "hevc", Width: width, Height: height, Bitrate: bitrate, PixelFormat: "yuv420p10le", Profile: "Main 10", BitDepth: 10, FrameRate: frameRate}
}

func TestGenerateVideoEvensTranscodeDimensions(t *testing.T) {
	sources := []domain.VideoStream{
		{Codec: "h264", Width: 320, Height: 181, PixelFormat: "yuv444p", Profile: "High 4:4:4 Predictive", Level: 13, FrameRate: 24, StartsWithKeyframe: true},
		hevcSource(1920, 817, 6_000_000, 24),
		hevcSource(853, 481, 1_000_000, 25),
		hevcSource(1081, 1921, 4_000_000, 30),
		hevcSource(3, 3, 0, 24),
	}
	for _, source := range sources {
		for _, r := range mustVideo(t, source) {
			if r.Method != domain.Transcode {
				t.Fatalf("%dx%d %s must transcode, got %#v", source.Width, source.Height, source.Codec, r)
			}
			if r.Width%2 != 0 || r.Height%2 != 0 || r.Width < 2 || r.Height < 2 {
				t.Errorf("%dx%d source: rendition %s is %dx%d, an encoder needs even dimensions", source.Width, source.Height, r.Name, r.Width, r.Height)
			}
		}
	}
	top := mustVideo(t, sources[0])[0]
	if top.Name != "180p" || top.Width != 320 || top.Height != 180 {
		t.Fatalf("320x181 source rung = %s %dx%d, want 180p 320x180", top.Name, top.Width, top.Height)
	}
}

func TestGenerateVideoKeepsOddDimensionsOnDirectStream(t *testing.T) {
	source := h264Source(1920, 817, 6_000_000)
	top := mustVideo(t, source)[0]
	if top.Method != domain.DirectStream || top.Width != 1920 || top.Height != 817 || top.Name != "817p" {
		t.Fatalf("a copied source rung must advertise the source picture, got %#v", top)
	}
}

func TestGenerateVideoNamesAreUnique(t *testing.T) {
	for _, source := range []domain.VideoStream{hevcSource(641, 361, 0, 24), hevcSource(1921, 1081, 0, 24), hevcSource(1280, 721, 0, 24), h264Source(640, 360, 800_000)} {
		seen := map[string]bool{}
		for _, r := range mustVideo(t, source) {
			if seen[r.Name] {
				t.Fatalf("%dx%d source offers %s twice", source.Width, source.Height, r.Name)
			}
			seen[r.Name] = true
		}
	}
}

func TestLevelPolicyStaysWithinEncoderRange(t *testing.T) {
	small := mustVideo(t, hevcSource(426, 240, 400_000, 24))
	if len(small) != 1 || small[0].Level != 30 || small[0].Codec != "avc1.64001e" {
		t.Fatalf("426x240 transcode = %#v, want level 3.0 advertised as avc1.64001e", small)
	}
	rates := []float64{0, 23.976, 24, 25, 29.97, 30, 50, 59.94, 60, 120}
	shapes := [][2]int{{176, 144}, {426, 240}, {640, 360}, {720, 576}, {854, 480}, {1280, 720}, {1920, 800}, {1920, 1080}, {1080, 1920}, {2560, 1440}, {3840, 2160}, {4096, 2160}, {7680, 4320}}
	for _, shape := range shapes {
		for _, rate := range rates {
			renditions, _, err := GenerateVideo(alignedMeta(hevcSource(shape[0], shape[1], 0, rate)), 6)
			if err != nil {
				t.Fatalf("%dx%d at %.3f: %v", shape[0], shape[1], rate, err)
			}
			for _, r := range renditions {
				if r.Level < 30 || r.Level > 52 {
					t.Errorf("%dx%d at %.3f: rendition %s asks for level %d, outside 3.0 to 5.2", shape[0], shape[1], rate, r.Name, r.Level)
				}
				if want := fmt.Sprintf("avc1.6400%02x", r.Level); r.Codec != want {
					t.Errorf("%dx%d at %.3f: rendition %s advertises %s, encodes level %d (%s)", shape[0], shape[1], rate, r.Name, r.Codec, r.Level, want)
				}
			}
		}
	}
}

func TestRungAboveLevelCeilingIsDropped(t *testing.T) {
	eightK := mustVideo(t, hevcSource(7680, 4320, 80_000_000, 30))
	if len(eightK) == 0 || eightK[0].Name != "2160p" || eightK[0].Width != 3840 {
		t.Fatalf("an 8K source must start its ladder at 2160p, got %#v", eightK)
	}
	fast := mustVideo(t, hevcSource(3840, 2160, 40_000_000, 120))
	if len(fast) == 0 || fast[0].Name != "1080p" {
		t.Fatalf("a 2160p120 source must start its ladder at 1080p, got %#v", fast)
	}
}

func TestGenerateVideoFailsWhenNoRungIsEncodable(t *testing.T) {
	renditions, _, err := GenerateVideo(alignedMeta(hevcSource(1920, 1080, 5_000_000, 5000)), 6)
	if !errors.Is(err, ErrNoEncodableVideo) || len(renditions) != 0 {
		t.Fatalf("expected ErrNoEncodableVideo and no renditions, got %v and %#v", err, renditions)
	}
	for _, name := range []string{"1080p", "720p", "480p", "360p", "5.2"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the error must give the reason for %s: %v", name, err)
		}
	}
}

func TestGenerateVideoOffersNothingWithoutAVideoStream(t *testing.T) {
	renditions, _, err := GenerateVideo(&domain.Metadata{Duration: 10, Audios: []domain.AudioStream{{Codec: "aac", Channels: 2}}}, 6)
	if err != nil || len(renditions) != 0 {
		t.Fatalf("an audio-only source has no video ladder and no error, got %v and %#v", err, renditions)
	}
}

func TestBufferSizeWithinLevelCPB(t *testing.T) {
	limits := map[int]int{30: 12_500_000, 31: 17_500_000, 32: 25_000_000, 40: 31_250_000, 41: 78_125_000, 42: 78_125_000, 50: 168_750_000, 51: 300_000_000, 52: 300_000_000}
	sources := []domain.VideoStream{hevcSource(1920, 1080, 8_000_000, 24), hevcSource(3840, 2160, 60_000_000, 60), hevcSource(640, 480, 9_000_000, 30), hevcSource(1280, 720, 30_000_000, 60)}
	for _, source := range sources {
		for _, r := range mustVideo(t, source) {
			limit, known := limits[r.Level]
			if !known {
				t.Fatalf("rendition %s uses level %d", r.Name, r.Level)
			}
			if r.BufferSize <= 0 || r.BufferSize > limit {
				t.Errorf("%dx%d source: rendition %s buffers %d bits at level %d, the level allows %d", source.Width, source.Height, r.Name, r.BufferSize, r.Level, limit)
			}
			if want := min(2*r.PeakBitrate, limit); r.BufferSize != want {
				t.Errorf("rendition %s buffers %d bits, want min(2 x peak, level CPB) = %d", r.Name, r.BufferSize, want)
			}
		}
	}
	top := mustVideo(t, sources[0])[0]
	if top.Level != 40 || top.Bitrate != 8_000_000 || top.PeakBitrate != 12_000_000 || top.BufferSize != 24_000_000 {
		t.Fatalf("1080p at 8 Mb/s = %#v, want level 4.0, peak 12 Mb/s, buffer 24 Mb", top)
	}
}

func TestLadderMonotonicAndSourceRungClamped(t *testing.T) {
	heavy := mustVideo(t, hevcSource(3840, 2160, 60_000_000, 24))
	if heavy[0].Name != "2160p" || heavy[0].Bitrate != 20_000_000 {
		t.Fatalf("a transcoded 60 Mb/s source rung must be clamped to 20 Mb/s, got %s at %d", heavy[0].Name, heavy[0].Bitrate)
	}
	copied := mustVideo(t, h264Source(3840, 2160, 60_000_000))
	if copied[0].Method != domain.DirectStream || copied[0].Bitrate != 60_000_000 {
		t.Fatalf("a copied source rung reports the source bitrate, got %#v", copied[0])
	}
	sources := []domain.VideoStream{
		h264Source(1920, 1080, 900_000),
		h264Source(1280, 720, 350_000),
		hevcSource(3840, 2160, 60_000_000, 24),
		hevcSource(1920, 800, 0, 24),
		h264Source(1080, 1920, 1_200_000),
	}
	for _, source := range sources {
		ladder := mustVideo(t, source)
		for index := 1; index < len(ladder); index++ {
			if ladder[index].Bitrate > ladder[index-1].Bitrate {
				t.Errorf("%dx%d at %d: %s at %d follows %s at %d", source.Width, source.Height, source.Bitrate, ladder[index].Name, ladder[index].Bitrate, ladder[index-1].Name, ladder[index-1].Bitrate)
			}
			if ladder[index].Width*ladder[index].Height >= ladder[index-1].Width*ladder[index-1].Height {
				t.Errorf("%s is not smaller than %s", ladder[index].Name, ladder[index-1].Name)
			}
		}
	}
	thin := mustVideo(t, h264Source(1920, 1080, 900_000))
	if len(thin) != 4 || thin[1].Bitrate != 900_000 || thin[2].Bitrate != 500_000 || thin[3].Bitrate != 300_000 {
		t.Fatalf("a 900 kb/s 1080p source must cap its 720p rung at 900 kb/s, got %#v", thin)
	}
}

func TestGenerateVideoUsesFormatBitrateFallback(t *testing.T) {
	video := h264Source(1920, 1080, 0)
	cases := []struct {
		name   string
		format int
		audios []domain.AudioStream
		want   int
	}{
		{"container minus audio", 40_000_000, []domain.AudioStream{{Bitrate: 640_000}, {Bitrate: 192_000}}, 39_168_000},
		{"unknown audio is not subtracted", 40_000_000, []domain.AudioStream{{Bitrate: 0}, {Bitrate: 640_000}}, 39_360_000},
		{"no container bitrate", 0, []domain.AudioStream{{Bitrate: 640_000}}, 5_000_000},
		{"audio exceeds container", 500_000, []domain.AudioStream{{Bitrate: 640_000}}, 5_000_000},
	}
	for _, c := range cases {
		meta := alignedMeta(video)
		meta.Bitrate = c.format
		meta.Audios = c.audios
		renditions, _, err := GenerateVideo(meta, 6)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if renditions[0].Bitrate != c.want {
			t.Errorf("%s: source rung bitrate = %d, want %d", c.name, renditions[0].Bitrate, c.want)
		}
	}
	tagged := alignedMeta(h264Source(1920, 1080, 7_000_000))
	tagged.Bitrate = 40_000_000
	renditions, _, err := GenerateVideo(tagged, 6)
	if err != nil || renditions[0].Bitrate != 7_000_000 {
		t.Fatalf("a declared stream bitrate wins over the container, got %v and %#v", err, renditions)
	}
}

func TestPortraitLadderUsesShortSide(t *testing.T) {
	ladder := mustVideo(t, h264Source(1080, 1920, 6_000_000))
	want := []struct {
		name          string
		width, height int
	}{{"1080p", 1080, 1920}, {"720p", 720, 1280}, {"480p", 480, 854}, {"360p", 360, 640}}
	if len(ladder) != len(want) {
		t.Fatalf("portrait ladder = %#v", ladder)
	}
	for index, rung := range want {
		if ladder[index].Name != rung.name || ladder[index].Width != rung.width || ladder[index].Height != rung.height {
			t.Errorf("rung %d = %s %dx%d, want %s %dx%d", index, ladder[index].Name, ladder[index].Width, ladder[index].Height, rung.name, rung.width, rung.height)
		}
	}
	if ladder[1].Bitrate != 2_666_666 || ladder[0].Method != domain.DirectStream {
		t.Fatalf("portrait 720p must be priced as a 720p picture (2666666), got %d; top %s", ladder[1].Bitrate, ladder[0].Method)
	}
}

func TestDirectStreamRequiresKeyframeAlignedPlan(t *testing.T) {
	video := h264Source(1920, 1080, 6_000_000)
	cases := []struct {
		name      string
		keyframes []float64
		duration  float64
		target    float64
		want      domain.PlaybackMethod
	}{
		{"aligned", []float64{0, 6, 12}, 18, 6, domain.DirectStream},
		{"long keyframe gap is split", []float64{0, 20, 26}, 32, 6, domain.Transcode},
		{"no keyframes", nil, 18, 6, domain.Transcode},
		{"first keyframe is late", []float64{0.5, 6, 12}, 18, 6, domain.DirectStream},
		{"same source, longer target", []float64{0, 20, 26}, 32, 12, domain.DirectStream},
	}
	for _, c := range cases {
		renditions, _, err := GenerateVideo(&domain.Metadata{Duration: c.duration, Keyframes: c.keyframes, Video: video}, c.target)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if renditions[0].Method != c.want {
			t.Errorf("%s: source rung method = %s, want %s", c.name, renditions[0].Method, c.want)
		}
		if c.want == domain.Transcode && (renditions[0].Level != 40 || renditions[0].Codec != "avc1.640028" || renditions[0].BufferSize == 0) {
			t.Errorf("%s: a transcoded source rung carries encoder facts, got %#v", c.name, renditions[0])
		}
	}
}

func TestDirectStreamCodecStringFollowsSourceProfile(t *testing.T) {
	cases := map[string]string{
		"Constrained Baseline": "avc1.42401f",
		"Baseline":             "avc1.42001f",
		"Main":                 "avc1.4d001f",
		"High":                 "avc1.64001f",
	}
	for profile, want := range cases {
		video := h264Source(1280, 720, 3_000_000)
		video.Profile = profile
		video.Level = 31
		top := mustVideo(t, video)[0]
		if top.Method != domain.DirectStream || top.Codec != want || top.Level != 31 {
			t.Errorf("%s level 3.1 = %s %s level %d, want direct stream %s", profile, top.Method, top.Codec, top.Level, want)
		}
	}
}

func TestExactlyOneDefaultAudioTrack(t *testing.T) {
	cases := []struct {
		name   string
		audios []domain.AudioStream
		want   int
	}{
		{"no flag", []domain.AudioStream{{Index: 1, Codec: "ac3", Channels: 6}, {Index: 2, Codec: "aac", Channels: 2}}, 1},
		{"later track flagged", []domain.AudioStream{{Index: 1, Codec: "ac3", Channels: 6}, {Index: 2, Codec: "ac3", Channels: 6, Default: true}, {Index: 3, Codec: "aac", Channels: 2}}, 2},
		{"several flagged", []domain.AudioStream{{Index: 1, Codec: "aac", Channels: 2}, {Index: 2, Codec: "eac3", Channels: 6, Default: true}, {Index: 3, Codec: "ac3", Channels: 6, Default: true}}, 2},
		{"every track flagged", []domain.AudioStream{{Index: 1, Codec: "aac", Channels: 2, Default: true}, {Index: 2, Codec: "aac", Channels: 2, Default: true}}, 1},
	}
	for _, c := range cases {
		tracks := map[int]int{}
		shapes := map[string]int{}
		for _, r := range GenerateAudioTracks(c.audios) {
			if !r.Default {
				continue
			}
			tracks[r.StreamIndex]++
			shapes[fmt.Sprintf("%s/%d", r.CodecString, r.Channels)]++
		}
		if len(tracks) != 1 || tracks[c.want] == 0 {
			t.Errorf("%s: default renditions per stream = %v, want only stream %d", c.name, tracks, c.want)
		}
		for shape, count := range shapes {
			if count != 1 {
				t.Errorf("%s: %d default renditions share codec and channels %s", c.name, count, shape)
			}
		}
	}
	flagged := GenerateAudioTracks(cases[1].audios)
	var defaults []string
	for _, r := range flagged {
		if r.Default {
			defaults = append(defaults, r.Name)
		}
	}
	if strings.Join(defaults, ",") != "audio2_aac_stereo,audio2_aac_surround,audio2_ac3_passthrough" {
		t.Fatalf("every rendition of the default track is default, got %v", defaults)
	}
}

func TestPassthroughWithoutBitrateUsesCodecCeiling(t *testing.T) {
	cases := []struct {
		codec   string
		bitrate int
		want    int
	}{{"ac3", 0, 640_000}, {"eac3", 0, 1_536_000}, {"ac3", 448_000, 448_000}, {"eac3", 768_000, 768_000}}
	for _, c := range cases {
		renditions := GenerateAudioTracks([]domain.AudioStream{domain.AudioStream{Codec: c.codec, Channels: 6, Bitrate: c.bitrate}})
		last := renditions[len(renditions)-1]
		if last.Method != domain.DirectStream || last.Bitrate != c.want {
			t.Errorf("%s declared at %d: passthrough bitrate = %d, want %d", c.codec, c.bitrate, last.Bitrate, c.want)
		}
	}
}

func TestAudioRenditionsCarrySourceEndAndCodecString(t *testing.T) {
	renditions := GenerateAudioTracks([]domain.AudioStream{{Index: 1, Codec: "eac3", Channels: 6, End: 7.5}, {Index: 2, Codec: "dts", Channels: 6}})
	want := map[string]struct {
		codec string
		end   float64
	}{
		"audio1_aac_stereo":       {"mp4a.40.2", 7.5},
		"audio1_aac_surround":     {"mp4a.40.2", 7.5},
		"audio1_eac3_passthrough": {"ec-3", 7.5},
		"audio2_aac_stereo":       {"mp4a.40.2", 0},
		"audio2_aac_surround":     {"mp4a.40.2", 0},
	}
	if len(renditions) != len(want) {
		t.Fatalf("renditions = %#v", renditions)
	}
	for _, r := range renditions {
		expected, known := want[r.Name]
		if !known || r.CodecString != expected.codec || r.SourceEnd != expected.end {
			t.Errorf("rendition %s = codec %s, source end %.3f; want %+v", r.Name, r.CodecString, r.SourceEnd, expected)
		}
	}
}

func TestTranscodedRenditionCapsBufferAtLevelCPB(t *testing.T) {
	capped, err := transcodedRendition(640, 360, 6_000_000, 24)
	if err != nil || capped.Level != 30 || capped.PeakBitrate != 9_000_000 || capped.BufferSize != 12_500_000 {
		t.Fatalf("640x360 at 6 Mb/s = %#v (%v), want level 3.0 with the buffer capped at its 12.5 Mb CPB", capped, err)
	}
	free, err := transcodedRendition(640, 360, 800_000, 24)
	if err != nil || free.BufferSize != 2_400_000 {
		t.Fatalf("640x360 at 800 kb/s = %#v (%v), want a buffer of twice the peak", free, err)
	}
	if _, err := transcodedRendition(7680, 4320, 20_000_000, 24); err == nil || !strings.Contains(err.Error(), "4320p") || !strings.Contains(err.Error(), "5.2") {
		t.Fatalf("an 8K rung must be rejected with its name and the ceiling, got %v", err)
	}
}

func TestAudioRenditionsCarryTrackTitle(t *testing.T) {
	renditions := GenerateAudioTracks([]domain.AudioStream{
		{Index: 1, Codec: "ac3", Channels: 6, Language: "eng", Title: "Main"},
		{Index: 2, Codec: "aac", Channels: 2, Language: "jpn"},
	})
	if len(renditions) == 0 {
		t.Fatal("no renditions")
	}
	for _, r := range renditions {
		want := ""
		if r.StreamIndex == 1 {
			want = "Main"
		}
		if r.Title != want {
			t.Fatalf("rendition %q Title = %q, want %q", r.Name, r.Title, want)
		}
	}
}

func TestGenerateAudioTracksCarriesSampleRate(t *testing.T) {
	tracks := GenerateAudioTracks([]domain.AudioStream{{Index: 1, Codec: "aac", Channels: 2, SampleRate: 44100}})
	if len(tracks) == 0 {
		t.Fatal("no audio renditions")
	}
	for _, track := range tracks {
		if track.SampleRate != 44100 {
			t.Errorf("%s SampleRate = %d, want 44100", track.Name, track.SampleRate)
		}
	}
}

func TestGenerateVideoReportsEveryDroppedRung(t *testing.T) {
	cases := []struct {
		name   string
		video  domain.VideoStream
		want   []string
		failed bool
	}{
		{"nothing dropped", hevcSource(1920, 1080, 5_000_000, 24), nil, false},
		{"8K source rung", hevcSource(7680, 4320, 80_000_000, 30), []string{"4320p"}, false},
		{"2160p at 120 fps", hevcSource(3840, 2160, 40_000_000, 120), []string{"2160p"}, false},
		{"no rung left", hevcSource(1920, 1080, 5_000_000, 5000), []string{"1080p", "720p", "480p", "360p"}, true},
	}
	for _, c := range cases {
		renditions, dropped, err := GenerateVideo(alignedMeta(c.video), 6)
		if (err != nil) != c.failed {
			t.Fatalf("%s: error = %v", c.name, err)
		}
		var names []string
		for _, rung := range dropped {
			names = append(names, rung.Name)
			if !strings.Contains(rung.Reason, rung.Name) || !strings.Contains(rung.Reason, "5.2") {
				t.Errorf("%s: reason for %s must name the rung and the level ceiling, got %q", c.name, rung.Name, rung.Reason)
			}
			for _, kept := range renditions {
				if kept.Name == rung.Name {
					t.Errorf("%s: %s is both offered and reported as dropped", c.name, rung.Name)
				}
			}
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: dropped rungs = %v, want %v", c.name, names, c.want)
		}
	}
}

func TestVideoEndingBeforeThePlanIsPaddedNotCopied(t *testing.T) {
	cases := []struct {
		name     string
		videoEnd float64
		duration float64
		want     domain.PlaybackMethod
		padFrom  float64
	}{
		{"video ends 13 s early", 40, 53, domain.Transcode, 40},
		{"video ends just over the slack early", 17.49, 18, domain.Transcode, 17.49},
		{"video ends within the slack", 17.5, 18, domain.DirectStream, 0},
		{"video outlasts the plan", 18.02, 18, domain.DirectStream, 0},
		{"video end unknown", 0, 18, domain.DirectStream, 0},
	}
	for _, c := range cases {
		video := h264Source(1920, 1080, 6_000_000)
		video.End = c.videoEnd
		ladder, _, err := GenerateVideo(&domain.Metadata{Duration: c.duration, Keyframes: []float64{0, 6, 12}, Video: video}, 6)
		if err != nil || len(ladder) < 2 {
			t.Fatalf("%s: ladder %v, %v", c.name, ladder, err)
		}
		if ladder[0].Method != c.want {
			t.Errorf("%s: source rung method = %s, want %s", c.name, ladder[0].Method, c.want)
		}
		for _, rung := range ladder {
			if rung.SourceEnd != c.padFrom {
				t.Errorf("%s: rendition %s carries video end %.3f, want %.3f", c.name, rung.Name, rung.SourceEnd, c.padFrom)
			}
		}
	}
}
