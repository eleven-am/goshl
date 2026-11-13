package goshl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestStartAndStop(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	svc := NewController(Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start err: %v", err)
	}
	svc.Stop()
}

func TestStartStopsVideoPoolWhenAudioPoolFails(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{
		subErrByType: map[domain.StreamType]error{
			domain.StreamAudio: errors.New("audio subscribe failed"),
		},
	}
	svc := NewController(Options{
		Storage:     &stubStorage{},
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	if err := svc.Start(context.Background()); err == nil {
		t.Fatalf("expected audio pool start failure")
	}

	delete(coord.subErrByType, domain.StreamAudio)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("expected controller to restart after partial start failure: %v", err)
	}
	svc.Stop()
}

func TestControllerUsesExplicitHardwareMode(t *testing.T) {
	svc := NewController(Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
		HWAccelMode: AccelVideoToolbox,
	})

	cfg := svc.HWAccelConfig()
	if cfg.Accelerator != AccelVideoToolbox || cfg.Requested != AccelVideoToolbox {
		t.Fatalf("unexpected hw config: %#v", cfg)
	}
}

func TestHWAccelConfigReturnsIndependentSlices(t *testing.T) {
	svc := NewController(Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	first := svc.HWAccelConfig()
	first.EncodeFlags[0] = "mutated"
	second := svc.HWAccelConfig()
	if second.EncodeFlags[0] == "mutated" {
		t.Fatalf("returned hardware configuration mutated the controller")
	}
}

type hardwareEventRecorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *hardwareEventRecorder) Observe(ctx context.Context, event domain.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *hardwareEventRecorder) ofType(eventType domain.EventType) []domain.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matched []domain.Event
	for _, event := range r.events {
		if event.Type == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

func TestControllerEmitsFallbackWhenDetectionErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	recorder := &hardwareEventRecorder{}
	controller, err := NewControllerWithContext(context.Background(), Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
		HWAccel:     true,
		Observer:    recorder,
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if controller.HWAccelConfig().Error == "" {
		t.Fatalf("expected detection error on the hardware config")
	}
	fallbacks := recorder.ofType(domain.EventHardwareFallback)
	if len(fallbacks) != 1 {
		t.Fatalf("expected exactly one hardware fallback event, got %d", len(fallbacks))
	}
	if fallbacks[0].Error == "" || fallbacks[0].Accelerator != domain.AccelNone {
		t.Fatalf("unexpected fallback event: %+v", fallbacks[0])
	}
}

func lifecycleController(t *testing.T, coordinator Coordinator, jobTimeout time.Duration) *Controller {
	t.Helper()
	return lifecycleControllerWithSegmentTimeout(t, coordinator, jobTimeout, 50*time.Millisecond)
}

func lifecycleControllerWithSegmentTimeout(t *testing.T, coordinator Coordinator, jobTimeout, segmentTimeout time.Duration) *Controller {
	t.Helper()
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	return NewController(Options{
		Storage:           &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)},
		Coordinator:       coordinator,
		PathGen:           stubPathGen{},
		SegmentTimeout:    segmentTimeout,
		JobTimeout:        jobTimeout,
		GenerationTimeout: time.Minute,
	})
}

func TestCallsAfterStopReturnErrControllerStopped(t *testing.T) {
	controller := lifecycleController(t, &stubCoordinator{}, time.Minute)
	controller.Stop()
	controller.Stop()

	calls := map[string]func() error{
		"Segment": func() error {
			_, err := controller.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
			return err
		},
		"Metadata": func() error {
			_, err := controller.Metadata(context.Background(), "file:///media")
			return err
		},
		"ProbeMetadata": func() error {
			_, err := controller.ProbeMetadata(context.Background(), "file:///media")
			return err
		},
		"MasterPlaylist": func() error {
			_, err := controller.MasterPlaylist(context.Background(), "file:///media")
			return err
		},
		"Thumbnail": func() error {
			_, err := controller.Thumbnail(context.Background(), "file:///media", nil)
			return err
		},
		"SubtitlesVTT": func() error {
			_, err := controller.SubtitlesVTT(context.Background(), "file:///media", []string{"s0"})
			return err
		},
		"Start": func() error { return controller.Start(context.Background()) },
		"VariantPlaylist": func() error {
			_, err := controller.VariantPlaylist(context.Background(), "file:///media", domain.StreamVideo, "1080p")
			return err
		},
		"SubtitlePlaylist": func() error {
			_, err := controller.SubtitlePlaylist(context.Background(), "file:///media", "s0")
			return err
		},
		"SpriteVTT": func() error {
			_, err := controller.SpriteVTT(context.Background(), "file:///media")
			return err
		},
		"Sprite": func() error {
			_, err := controller.Sprite(context.Background(), "file:///media", 0)
			return err
		},
		"SpriteNegativeIndex": func() error {
			_, err := controller.Sprite(context.Background(), "file:///media", -1)
			return err
		},
		"SubtitleVTT": func() error {
			_, err := controller.SubtitleVTT(context.Background(), "file:///media", "s0")
			return err
		},
		"Preview": func() error {
			_, err := controller.Preview(context.Background(), "file:///media", nil)
			return err
		},
		"PreviewInvalidConfig": func() error {
			_, err := controller.Preview(context.Background(), "file:///media", &PreviewConfig{MaxFrames: -1})
			return err
		},
		"ThumbnailInvalidConfig": func() error {
			_, err := controller.Thumbnail(context.Background(), "file:///media", &ThumbnailConfig{Position: -1})
			return err
		},
		"SegmentInvalidStream": func() error {
			_, err := controller.Segment(context.Background(), "file:///media", "bogus", "1080p", 0)
			return err
		},
		"MediaKey": func() error {
			_, err := controller.MediaKey(context.Background(), "file:///media")
			return err
		},
		"Invalidate": func() error { return controller.Invalidate(context.Background(), "file:///media") },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrControllerStopped) {
			t.Errorf("%s after Stop: got %v, want ErrControllerStopped", name, err)
		}
	}
}

func TestControllerDelegatesRequestsToTheEngine(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	meta.Audios = []domain.AudioStream{{Index: 1, Codec: "aac", Channels: 2, Bitrate: 128_000, Default: true}}
	meta.Subtitles = []domain.SubtitleStream{{Index: 2, ID: "s0", Codec: "subrip", Language: "eng"}}
	metaBytes, err := jsonMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{1: []byte("seg1")}}
	controller := NewController(Options{
		Storage:        store,
		Coordinator:    &stubCoordinator{},
		PathGen:        stubPathGen{},
		HWAccelMode:    AccelNone,
		CacheVersion:   "release-7",
		SourceRevision: func(context.Context, string) (string, error) { return "revision-a", nil },
	})
	defer controller.Stop()
	ctx := context.Background()
	const source = "https://media.example/video.mkv"

	if out, err := controller.MasterPlaylist(ctx, source); err != nil || !strings.Contains(out, "#EXT-X-STREAM-INF") || !strings.Contains(out, "TYPE=SUBTITLES") {
		t.Errorf("MasterPlaylist = %q, %v", out, err)
	}
	if out, err := controller.VariantPlaylist(ctx, source, StreamVideo, "1080p"); err != nil || strings.Count(out, "#EXTINF") != 2 {
		t.Errorf("VariantPlaylist = %q, %v", out, err)
	}
	if _, err := controller.VariantPlaylist(ctx, source, StreamVideo, "missing"); !errors.Is(err, ErrRenditionNotFound) {
		t.Errorf("VariantPlaylist for an unknown rendition = %v, want ErrRenditionNotFound", err)
	}
	if out, err := controller.SubtitlePlaylist(ctx, source, "s0"); err != nil || !strings.Contains(out, "#EXTM3U") {
		t.Errorf("SubtitlePlaylist = %q, %v", out, err)
	}
	if _, err := controller.SubtitlePlaylist(ctx, source, "missing"); !errors.Is(err, ErrRenditionNotFound) {
		t.Errorf("SubtitlePlaylist for an unknown track = %v, want ErrRenditionNotFound", err)
	}
	if data, err := controller.Segment(ctx, source, StreamVideo, "1080p", 1); err != nil || string(data) != "seg1" {
		t.Errorf("Segment = %q, %v", data, err)
	}
	if _, err := controller.Segment(ctx, source, StreamVideo, "1080p", -1); !errors.Is(err, ErrSegmentOutOfRange) {
		t.Errorf("Segment with a negative index = %v, want ErrSegmentOutOfRange", err)
	}
	if _, err := controller.Segment(ctx, source, StreamSubtitle, "1080p", 0); !errors.Is(err, ErrInvalidStreamType) {
		t.Errorf("Segment with a subtitle stream = %v, want ErrInvalidStreamType", err)
	}
	if got, err := controller.Metadata(ctx, source); err != nil || got.Duration != 12 || len(got.Subtitles) != 1 {
		t.Errorf("Metadata = %+v, %v", got, err)
	}
	if key, err := controller.MediaKey(ctx, source); err != nil || key != "goshl:4156f0d49f186fe08eed0023a012364c1e51c7b20c571518c30c84f137560520" {
		t.Errorf("MediaKey = %s, %v", key, err)
	}
	results, err := controller.SubtitlesVTT(ctx, source, []string{"missing", "absent"})
	if err != nil || len(results) != 2 {
		t.Fatalf("SubtitlesVTT = %v, %v", results, err)
	}
	for _, trackID := range []string{"missing", "absent"} {
		if result := results[trackID]; result.Data != nil || !errors.Is(result.Err, ErrRenditionNotFound) {
			t.Errorf("SubtitlesVTT[%s] = %+v, want ErrRenditionNotFound", trackID, result)
		}
	}
	if _, err := controller.SubtitleVTT(ctx, source, "missing"); !errors.Is(err, ErrRenditionNotFound) {
		t.Errorf("SubtitleVTT for an unknown track = %v, want ErrRenditionNotFound", err)
	}
	if err := controller.Invalidate(ctx, source); !errors.Is(err, ErrCacheInvalidationUnsupported) {
		t.Errorf("Invalidate = %v, want ErrCacheInvalidationUnsupported", err)
	}
	if _, err := controller.Sprite(ctx, source, -1); err == nil || !strings.Contains(err.Error(), "sprite index out of range") {
		t.Errorf("Sprite with a negative index = %v", err)
	}
	if _, err := controller.Preview(ctx, source, &PreviewConfig{Width: -1}); !errors.Is(err, ErrInvalidPreviewConfig) {
		t.Errorf("Preview = %v, want ErrInvalidPreviewConfig", err)
	}
	if _, err := controller.Thumbnail(ctx, source, &ThumbnailConfig{Position: -1}); !errors.Is(err, ErrInvalidThumbnailConfig) {
		t.Errorf("Thumbnail = %v, want ErrInvalidThumbnailConfig", err)
	}
	if _, err := controller.ProbeMetadata(ctx, "rtmp://host/live"); !errors.Is(err, ErrUnsupportedSource) {
		t.Errorf("ProbeMetadata = %v, want ErrUnsupportedSource", err)
	}
	if config := controller.HWAccelConfig(); config.Accelerator != AccelNone {
		t.Errorf("HWAccelConfig = %+v", config)
	}
	if config := (&Controller{}).HWAccelConfig(); config.Accelerator != AccelNone || config.Requested != AccelNone {
		t.Errorf("HWAccelConfig of an unbuilt controller = %+v", config)
	}
}

func TestMediaKeysMatchGoldenValues(t *testing.T) {
	base := Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, HWAccelMode: AccelNone}
	versioned := base
	versioned.CacheVersion = "release-7"
	versioned.SourceRevision = func(context.Context, string) (string, error) { return "revision-a", nil }
	retimed := base
	retimed.TargetDuration = 4
	retimed.HWAccelDecodeMode = DecodeSafe
	for want, opts := range map[string]Options{
		"goshl:4156f0d49f186fe08eed0023a012364c1e51c7b20c571518c30c84f137560520": versioned,
		"goshl:4da71953e024652237f0699c44cb48c9a9b71aa72752e643836f1c2eb324a1b9": retimed,
	} {
		key, err := NewController(opts).MediaKey(context.Background(), "https://media.example/video.mkv")
		if err != nil || key != want {
			t.Errorf("MediaKey = %s, %v, want %s", key, err, want)
		}
	}
}
