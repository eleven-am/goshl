package rendition

import (
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestLive(t *testing.T) {
	cases := []struct {
		name          string
		video         domain.VideoStream
		display       int
		maxHeight     int
		width, height int
		bitrate       int
	}{
		{"hevc 1080p50 capped", domain.VideoStream{Codec: "hevc", Width: 1920, Height: 1080, FrameRate: 50}, 1920, 720, 1280, 720, 2_500_000},
		{"mpeg2 576i anamorphic", domain.VideoStream{Codec: "mpeg2video", Width: 720, Height: 576, FrameRate: 25}, 1024, 720, 1024, 576, 2_500_000},
		{"odd height rounds down", domain.VideoStream{Codec: "h264", Width: 853, Height: 481, FrameRate: 30}, 853, 720, 850, 480, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := Live(test.video, test.display, test.maxHeight)
			if err != nil {
				t.Fatal(err)
			}
			if got.Width != test.width || got.Height != test.height {
				t.Errorf("size = %dx%d, want %dx%d", got.Width, got.Height, test.width, test.height)
			}
			if got.Width%2 != 0 || got.Height%2 != 0 {
				t.Errorf("odd size %dx%d", got.Width, got.Height)
			}
			if test.bitrate != 0 && got.Bitrate != test.bitrate {
				t.Errorf("bitrate = %d, want %d", got.Bitrate, test.bitrate)
			}
			if got.Method != domain.Transcode || got.Level == 0 {
				t.Errorf("method %v level %d", got.Method, got.Level)
			}
		})
	}
}

func TestLiveRejectsMissingDimensions(t *testing.T) {
	if _, err := Live(domain.VideoStream{Codec: "h264", Width: 1280}, 1280, 720); err == nil {
		t.Fatal("expected error for zero height")
	}
}
