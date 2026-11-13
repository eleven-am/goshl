package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
)

func hdrMetadata() *domain.Metadata {
	return &domain.Metadata{
		Duration:  60,
		Keyframes: []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54},
		Video: domain.VideoStream{
			Codec: "hevc", Width: 1920, Height: 1080, Bitrate: 8_000_000, PixelFormat: "yuv420p10le", BitDepth: 10,
			FrameRate: 24, StartsWithKeyframe: true, ColorTransfer: domain.TransferPQ,
		},
	}
}

func gatedController(t *testing.T, meta *domain.Metadata, detect func(context.Context) (string, error)) *Engine {
	t.Helper()
	controller := playlistController(t, meta)
	controller.tonemap.detect = detect
	return controller
}

func missingZscale(context.Context) (string, error) {
	return "", fmt.Errorf("%w: ffmpeg has no zscale filter", hwaccel.ErrTonemapUnavailable)
}

func TestMasterPlaylistFailsForHDRWithoutTonemapFilter(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	controller := gatedController(t, hdrMetadata(), missingZscale)

	out, err := controller.MasterPlaylist(context.Background(), "file:///hdr")
	if !errors.Is(err, ErrTonemapUnavailable) || out != "" {
		t.Fatalf("master = %q, err = %v, want ErrTonemapUnavailable", out, err)
	}
}

func TestVariantPlaylistFailsForHDRVideoWithoutTonemapFilter(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	controller := gatedController(t, hdrMetadata(), missingZscale)

	out, err := controller.VariantPlaylist(context.Background(), "file:///hdr", domain.StreamVideo, "360p")
	if !errors.Is(err, ErrTonemapUnavailable) || out != "" {
		t.Fatalf("variant = %q, err = %v, want ErrTonemapUnavailable", out, err)
	}
}

func TestHDRPlaylistsServeWhenTonemapFilterExists(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	controller := gatedController(t, hdrMetadata(), func(context.Context) (string, error) { return "zscale", nil })

	if out, err := controller.MasterPlaylist(context.Background(), "file:///hdr"); err != nil || out == "" {
		t.Fatalf("master = %q, err = %v", out, err)
	}
	if out, err := controller.VariantPlaylist(context.Background(), "file:///hdr", domain.StreamVideo, "360p"); err != nil || out == "" {
		t.Fatalf("variant = %q, err = %v", out, err)
	}
}

func TestSDRPlaylistsNeverProbeTonemapCapability(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	var calls atomic.Int32
	controller := gatedController(t, testVideoMetadata(60, []float64{0, 6, 12}), func(context.Context) (string, error) {
		calls.Add(1)
		return "", errors.New("must not run")
	})

	if _, err := controller.MasterPlaylist(context.Background(), "file:///sdr"); err != nil {
		t.Fatalf("master: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("detector ran %d times for an SDR source", calls.Load())
	}
}

func TestTonemapCapabilityCachesVerdictsButNotRunFailures(t *testing.T) {
	var calls atomic.Int32
	failure := errors.New("ffmpeg -filters failed")
	capability := tonemapCapability{detect: func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			return "", failure
		}
		return "", fmt.Errorf("%w: ffmpeg has no zscale filter", hwaccel.ErrTonemapUnavailable)
	}}

	if err := capability.check(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("first check = %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := capability.check(context.Background()); !errors.Is(err, ErrTonemapUnavailable) {
			t.Fatalf("check %d = %v", i, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("detector ran %d times, want 2", calls.Load())
	}
}

func eightBitHDRMetadata(transfer, primaries string) *domain.Metadata {
	meta := testVideoMetadata(60, []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54})
	meta.Video.BitDepth = 8
	meta.Video.ColorSpace = "bt2020nc"
	meta.Video.ColorTransfer = transfer
	meta.Video.ColorPrimaries = primaries
	return meta
}

func TestEightBitHDRPlaylistsFailWithoutTonemapFilter(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	for _, transfer := range []string{domain.TransferPQ, domain.TransferHLG} {
		for _, primaries := range []string{"bt2020", "", "unknown"} {
			controller := gatedController(t, eightBitHDRMetadata(transfer, primaries), missingZscale)
			if out, err := controller.MasterPlaylist(context.Background(), "file:///hdr8"); !errors.Is(err, ErrTonemapUnavailable) || out != "" {
				t.Errorf("%s/%q master = %q, err = %v, want ErrTonemapUnavailable", transfer, primaries, out, err)
			}
			if out, err := controller.VariantPlaylist(context.Background(), "file:///hdr8", domain.StreamVideo, "360p"); !errors.Is(err, ErrTonemapUnavailable) || out != "" {
				t.Errorf("%s/%q variant = %q, err = %v, want ErrTonemapUnavailable", transfer, primaries, out, err)
			}
		}
	}
}

func TestHDRTransferWithSDRPrimariesNeverProbesTonemapCapability(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	for _, primaries := range []string{"bt709", "smpte170m", "bt470bg", "bt470m"} {
		for _, depth := range []int{8, 10} {
			var calls atomic.Int32
			meta := eightBitHDRMetadata(domain.TransferPQ, primaries)
			meta.Video.BitDepth = depth
			controller := gatedController(t, meta, func(context.Context) (string, error) {
				calls.Add(1)
				return "", errors.New("must not run")
			})
			if out, err := controller.MasterPlaylist(context.Background(), "file:///mistagged"); err != nil || out == "" {
				t.Errorf("%s %d-bit master = %q, err = %v", primaries, depth, out, err)
			}
			if out, err := controller.VariantPlaylist(context.Background(), "file:///mistagged", domain.StreamVideo, "360p"); err != nil || out == "" {
				t.Errorf("%s %d-bit variant = %q, err = %v", primaries, depth, out, err)
			}
			if calls.Load() != 0 {
				t.Errorf("%s %d-bit: detector ran %d times for a retagged source", primaries, depth, calls.Load())
			}
		}
	}
}
