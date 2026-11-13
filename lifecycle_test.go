package goshl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

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

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestStopKillsDetachedAssetGeneration(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then exit 0; fi\necho $$ > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	controller := lifecycleController(t, newClusterCoordinator(), time.Minute)
	requestDone := make(chan error, 1)
	go func() {
		_, err := controller.Thumbnail(context.Background(), "file:///media", nil)
		requestDone <- err
	}()

	var pid int
	waitUntil(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(string(data)))
		pid = parsed
		return err == nil
	})
	if !processAlive(pid) {
		t.Fatalf("fake ffmpeg %d is not running", pid)
	}

	started := time.Now()
	controller.Stop()
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("Stop took %v", elapsed)
	}
	waitUntil(t, 5*time.Second, func() bool { return !processAlive(pid) })
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("expected the in-flight request to fail once the controller stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not return after Stop")
	}
}

func TestCallsAfterStopReturnErrControllerStopped(t *testing.T) {
	controller := lifecycleController(t, newClusterCoordinator(), time.Minute)
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
