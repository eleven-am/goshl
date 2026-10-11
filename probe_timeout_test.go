package goshl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

func installSlowFFprobe(t *testing.T, delaySeconds string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep " + delaySeconds + "\necho notjson\n"
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func probeController(t *testing.T, opts Options) (*Controller, *stubCoordinator) {
	t.Helper()
	cleanup := installFakeFFmpeg(t)
	t.Cleanup(cleanup)
	coord := &stubCoordinator{}
	opts.Storage = &stubStorage{}
	opts.Coordinator = coord
	opts.PathGen = stubPathGen{}
	return NewController(opts), coord
}

func TestProbeTimeoutDefaultsToJobTimeout(t *testing.T) {
	opts := Options{JobTimeout: 5 * time.Minute}
	opts.setDefaults()
	if opts.ProbeTimeout != 5*time.Minute {
		t.Fatalf("expected ProbeTimeout to follow JobTimeout, got %v", opts.ProbeTimeout)
	}

	defaults := Options{}
	defaults.setDefaults()
	if defaults.ProbeTimeout != 2*time.Minute || defaults.JobTimeout != 2*time.Minute {
		t.Fatalf("unexpected defaults probe=%v job=%v", defaults.ProbeTimeout, defaults.JobTimeout)
	}
}

func TestProbeTimeoutKeepsExplicitValueAndLeavesJobTimeout(t *testing.T) {
	opts := Options{JobTimeout: time.Minute, ProbeTimeout: 10 * time.Minute}
	opts.setDefaults()
	if opts.ProbeTimeout != 10*time.Minute || opts.JobTimeout != time.Minute {
		t.Fatalf("unexpected timeouts probe=%v job=%v", opts.ProbeTimeout, opts.JobTimeout)
	}
}

func TestOptionsRejectNegativeProbeTimeout(t *testing.T) {
	opts := Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, ProbeTimeout: -time.Second}
	if err := opts.validateError(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected invalid options, got %v", err)
	}
}

func TestMetadataProbeOutlivesJobTimeoutWithinProbeTimeout(t *testing.T) {
	installSlowFFprobe(t, "1")
	svc, _ := probeController(t, Options{JobTimeout: 200 * time.Millisecond, ProbeTimeout: 10 * time.Second})

	_, err := svc.Metadata(context.Background(), "file:///media")

	if err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("expected probe to finish and fail parsing, got %v", err)
	}
}

func TestMetadataProbeFailsPastProbeTimeout(t *testing.T) {
	installSlowFFprobe(t, "1")
	svc, _ := probeController(t, Options{JobTimeout: 10 * time.Second, ProbeTimeout: 200 * time.Millisecond})

	_, err := svc.Metadata(context.Background(), "file:///media")

	if err == nil || strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("expected probe timeout, got %v", err)
	}
}

func TestMetadataProbeUsesJobTimeoutWhenProbeTimeoutUnset(t *testing.T) {
	installSlowFFprobe(t, "1")
	svc, _ := probeController(t, Options{JobTimeout: 200 * time.Millisecond})

	_, err := svc.Metadata(context.Background(), "file:///media")

	if err == nil || strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("expected probe timeout at JobTimeout, got %v", err)
	}
}

func TestMetadataLockTTLFollowsProbeTimeout(t *testing.T) {
	installSlowFFprobe(t, "0")
	svc, coord := probeController(t, Options{JobTimeout: time.Minute, ProbeTimeout: 10 * time.Minute})

	_, _ = svc.Metadata(context.Background(), "file:///media")

	if len(coord.lockTTLs) != 1 || coord.lockTTLs[0] != 11*time.Minute {
		t.Fatalf("expected metadata lock ttl 11m, got %v", coord.lockTTLs)
	}
}

func TestTranscodeDeadlineIgnoresProbeTimeout(t *testing.T) {
	controller := &Controller{opts: Options{JobTimeout: time.Minute, ProbeTimeout: 10 * time.Minute}, pipelineVersion: "test"}
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6}}
	video := domain.VideoRendition{Name: "720p", Method: domain.Transcode}
	job := controller.buildJob("source", "key", StreamVideo, video.Name, 0, 0, 0, segments, &video, nil)
	if remaining := time.Until(job.Deadline); remaining > time.Minute || remaining < 50*time.Second {
		t.Fatalf("expected deadline near JobTimeout, got %v", remaining)
	}
}

func TestSpriteVTTReadsFromConfiguredSpriteStorage(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	metaBytes, _ := json.Marshal(testVideoMetadata(60, nil))
	main := &stubStorage{metaData: metaBytes, metaExists: true}
	sprites := &stubStorage{spriteVTT: []byte("sprite-store-vtt")}
	svc := NewController(Options{Storage: main, SpriteStorage: sprites, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})

	out, err := svc.SpriteVTT(context.Background(), "file:///media")

	if err != nil || string(out) != "sprite-store-vtt" {
		t.Fatalf("expected vtt from sprite storage, got %q %v", out, err)
	}
	if main.spriteVTT != nil {
		t.Fatalf("segment storage must not hold sprites")
	}
}

type lateMetadataStorage struct {
	*stubStorage
	readyAt time.Time
}

func (s *lateMetadataStorage) MetadataExists(ctx context.Context, key string) (bool, error) {
	return !time.Now().Before(s.readyAt), nil
}

func TestWaitForMetadataHonoursProbeTimeout(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	metaBytes, _ := json.Marshal(testVideoMetadata(60, nil))
	store := &lateMetadataStorage{stubStorage: &stubStorage{metaData: metaBytes}, readyAt: time.Now().Add(700 * time.Millisecond)}
	svc := NewController(Options{Storage: store, Coordinator: &stubCoordinator{lockErr: ErrLockHeld}, PathGen: stubPathGen{}, JobTimeout: 200 * time.Millisecond, ProbeTimeout: 5 * time.Second})

	if _, err := svc.Metadata(context.Background(), "file:///media"); err != nil {
		t.Fatalf("expected waiter to outlast JobTimeout, got %v", err)
	}
}

func TestWaitForMetadataTimesOutAtProbeTimeout(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	store := &lateMetadataStorage{stubStorage: &stubStorage{}, readyAt: time.Now().Add(time.Hour)}
	svc := NewController(Options{Storage: store, Coordinator: &stubCoordinator{lockErr: ErrLockHeld}, PathGen: stubPathGen{}, JobTimeout: 10 * time.Second, ProbeTimeout: 300 * time.Millisecond})

	if _, err := svc.Metadata(context.Background(), "file:///media"); !errors.Is(err, ErrMetadataTimeout) {
		t.Fatalf("expected ErrMetadataTimeout, got %v", err)
	}
}
