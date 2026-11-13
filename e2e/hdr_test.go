package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func TestControllerServesHDRSourceAsTaggedSDROrFailsExplicitly(t *testing.T) {
	testmedia.Require(t)
	source := testmedia.Colour(t, testmedia.ColourSpec{
		Container:      "mkv",
		Width:          320,
		Height:         180,
		PixelFormat:    "yuv420p10le",
		Primaries:      "bt2020",
		Transfer:       "smpte2084",
		Matrix:         "bt2020nc",
		Range:          "tv",
		KeyframeFrames: []int{0, 48, 96},
		Duration:       6,
	})
	if got := testmedia.Tags(t, source); got.Transfer != "smpte2084" || got.PixelFormat != "yuv420p10le" {
		t.Fatalf("fixture is not HDR: %+v", got)
	}
	controller := goshl.NewController(goshl.Options{
		Storage:        newTimelineStorage(),
		Coordinator:    newClusterCoordinator(),
		PathGen:        bandwidthPathGen{},
		TargetDuration: 2,
		SegmentsPerJob: 2,
		SegmentTimeout: 2 * time.Minute,
		JobTimeout:     2 * time.Minute,
	})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("start controller: %v", err)
	}
	defer controller.Stop()

	meta, err := controller.Metadata(ctx, source)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.Video.ColorTransfer != "smpte2084" || meta.Video.BitDepth != 10 {
		t.Fatalf("metadata must report a 10-bit PQ source, got transfer %q depth %d", meta.Video.ColorTransfer, meta.Video.BitDepth)
	}

	master, err := controller.MasterPlaylist(ctx, source)
	if errors.Is(err, goshl.ErrTonemapUnavailable) {
		if master != "" {
			t.Fatalf("a refused HDR source must not publish a master playlist, got:\n%s", master)
		}
		t.Logf("ffmpeg cannot tonemap, the HDR source is refused explicitly: %v", err)
		return
	}
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	var video *masterVariant
	for _, variant := range parseMasterVariants(t, master) {
		if variant.stream == goshl.StreamVideo {
			video = &variant
			break
		}
	}
	if video == nil {
		t.Fatalf("master offers no video variant:\n%s", master)
	}
	data, err := controller.Segment(ctx, source, goshl.StreamVideo, video.name, 0)
	if err != nil {
		t.Fatalf("%s segment 0: %v", video.name, err)
	}
	path := filepath.Join(t.TempDir(), "segment.ts")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	want := testmedia.ColourTags{PixelFormat: "yuv420p", Range: "tv", Matrix: "bt709", Transfer: "bt709", Primaries: "bt709"}
	if got := testmedia.Tags(t, path); got != want {
		t.Fatalf("%s segment colour %+v, want %+v", video.name, got, want)
	}
}
