package rendition

import (
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestGenerateVideo_DirectStreamAndClamping(t *testing.T) {
	src := domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 10_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 40, StartsWithKeyframe: true}

	renditions := GenerateVideo(src)

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
			if r.Bitrate != 4_000_000 { // ratio would be higher, should clamp to 4Mbps cap
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

	renditions := GenerateVideo(src)

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
	renditions := GenerateAudio(audio)

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
	r := GenerateVideo(domain.VideoStream{Codec: "h264", Width: 426, Height: 240, Bitrate: 400_000, PixelFormat: "yuv420p", Profile: "High", Level: 30, FrameRate: 24, StartsWithKeyframe: true})
	if len(r) != 1 {
		t.Fatalf("expected one source-sized rendition, got %#v", r)
	}
	if r[0].Name != "240p" || r[0].Width != 426 || r[0].Height != 240 {
		t.Fatalf("unexpected source rendition: %#v", r[0])
	}
}

func TestGenerateVideoIncludesNonstandardSourceRung(t *testing.T) {
	r := GenerateVideo(domain.VideoStream{Codec: "h264", Width: 2560, Height: 1440, Bitrate: 12_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 51, FrameRate: 60, StartsWithKeyframe: true})
	if len(r) == 0 || r[0].Name != "1440p" || r[0].Method != domain.DirectStream || r[0].Bitrate != 12_000_000 {
		t.Fatalf("expected unchanged source rung first, got %#v", r)
	}
}

func TestGenerateVideoRejectsIncompatibleDirectStream(t *testing.T) {
	r := GenerateVideo(domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 5_000_000, PixelFormat: "yuv420p10le", Profile: "High 10", Level: 51})
	if len(r) == 0 || r[0].Method != domain.Transcode {
		t.Fatalf("expected incompatible H264 to transcode, got %#v", r)
	}
}

func TestGenerateVideoRejectsUnknownOrExcessiveH264Level(t *testing.T) {
	for _, level := range []int{0, 60} {
		renditions := GenerateVideo(domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 5_000_000, PixelFormat: "yuv420p", Profile: "High", Level: level, StartsWithKeyframe: true})
		if len(renditions) == 0 || renditions[0].Method != domain.Transcode {
			t.Fatalf("level %d must not be remuxed directly: %#v", level, renditions)
		}
	}
}

func TestGenerateVideoRejectsRotatedDirectStream(t *testing.T) {
	renditions := GenerateVideo(domain.VideoStream{Codec: "h264", Width: 1080, Height: 1920, Bitrate: 5_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 40, Rotation: 90, StartsWithKeyframe: true})
	if len(renditions) == 0 || renditions[0].Method != domain.Transcode {
		t.Fatalf("rotated source must be normalized by transcoding: %#v", renditions)
	}
}

func TestGenerateVideoRejectsAnamorphicDirectStream(t *testing.T) {
	renditions := GenerateVideo(domain.VideoStream{Codec: "h264", Width: 768, Height: 576, Bitrate: 2_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 31, SampleAspect: "16:15", StartsWithKeyframe: true})
	if len(renditions) == 0 || renditions[0].Method != domain.Transcode {
		t.Fatalf("anamorphic source must be normalized by transcoding: %#v", renditions)
	}
}
