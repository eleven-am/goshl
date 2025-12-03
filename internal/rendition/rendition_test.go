package rendition

import (
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestGenerateVideo_DirectStreamAndClamping(t *testing.T) {
	src := domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 10_000_000}

	renditions := GenerateVideo(src)

	if len(renditions) == 0 || renditions[0].Name != "1080p" {
		t.Fatalf("expected first rendition to be 1080p, got %#v", renditions)
	}

	top := renditions[0]
	if top.Method != domain.DirectStream {
		t.Fatalf("expected direct stream when codec matches source, got %s", top.Method)
	}
	if top.Bitrate != 8_000_000 { // clamp to max for 1080p
		t.Fatalf("bitrate should be clamped to 8Mbps, got %d", top.Bitrate)
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
	if stereo.Name != "aac_stereo" || stereo.Method != domain.Transcode || stereo.Bitrate != 128_000 {
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
