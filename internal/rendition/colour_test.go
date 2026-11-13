package rendition

import (
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func eligibleSource() domain.VideoStream {
	return domain.VideoStream{Codec: "h264", Profile: "High", Level: 40, PixelFormat: "yuv420p", BitDepth: 8, Width: 1920, Height: 1080, Bitrate: 5000000, FrameRate: 24, StartsWithKeyframe: true}
}

func TestDirectStreamRejectsHDRTransfer(t *testing.T) {
	if got := mustVideo(t, eligibleSource())[0].Method; got != domain.DirectStream {
		t.Fatalf("fixture must be direct stream eligible, got %s", got)
	}
	for _, transfer := range []string{"smpte2084", "arib-std-b67"} {
		source := eligibleSource()
		source.BitDepth = 10
		source.ColorTransfer = transfer
		for _, rendition := range mustVideo(t, source) {
			if rendition.Method != domain.Transcode {
				t.Errorf("%s source rendition %s uses %s, want transcode", transfer, rendition.Name, rendition.Method)
			}
			if !rendition.Colour.Tonemap {
				t.Errorf("%s source rendition %s is not tone mapped", transfer, rendition.Name)
			}
		}
	}
}

func TestEightBitHDRTransferIsRetaggedNotToneMapped(t *testing.T) {
	for _, transfer := range []string{"smpte2084", "arib-std-b67"} {
		source := eligibleSource()
		source.ColorTransfer = transfer
		for _, rendition := range mustVideo(t, source) {
			if rendition.Method != domain.Transcode {
				t.Errorf("%s source rendition %s uses %s, want transcode", transfer, rendition.Name, rendition.Method)
			}
			if rendition.Colour != (domain.ColourPlan{Retag: true}) {
				t.Errorf("%s source rendition %s colour %+v, want retag only", transfer, rendition.Name, rendition.Colour)
			}
		}
	}
}

func TestTranscodeRenditionsCarryColourPlan(t *testing.T) {
	source := eligibleSource()
	renditions := mustVideo(t, source)
	if renditions[0].Colour != (domain.ColourPlan{}) {
		t.Fatalf("copied rendition must carry no colour plan: %+v", renditions[0].Colour)
	}
	if got := renditions[1].Colour; got != (domain.ColourPlan{AssumeMatrix: "bt709"}) {
		t.Fatalf("untagged 1080p source plan %+v, want assumed bt709", got)
	}
	source.ColorSpace = "smpte170m"
	if got := mustVideo(t, source)[1].Colour; got != (domain.ColourPlan{}) {
		t.Fatalf("tagged source plan %+v, want no assumption", got)
	}
}
