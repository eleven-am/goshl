package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestDeterministicJobIDs(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(60, []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54, 60})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{}
	svc := mustNew(Config{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	firstJobID := coord.enqueued[0].ID
	firstJobRunID := coord.enqueued[0].RunID
	svc.finishFlightRun(firstJobID, firstJobRunID)

	coord.enqueued = nil
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	secondJobID := coord.enqueued[0].ID

	if firstJobID != secondJobID {
		t.Fatalf("job IDs should be deterministic, got %q and %q", firstJobID, secondJobID)
	}

	coord.enqueued = nil
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "720p", 1)
	differentRenditionID := coord.enqueued[0].ID

	if firstJobID == differentRenditionID {
		t.Fatalf("different renditions should have different job IDs")
	}
}

func TestMediaKeyChangesWithSourceRevisionAndPipeline(t *testing.T) {
	base := Config{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, SourceRevision: func(context.Context, string) (string, error) { return "revision-a", nil }}
	first := mustNew(base)
	firstKey, err := first.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve first key: %v", err)
	}

	base.SourceRevision = func(context.Context, string) (string, error) { return "revision-b", nil }
	second := mustNew(base)
	secondKey, err := second.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve second key: %v", err)
	}
	if firstKey == secondKey {
		t.Fatalf("source revisions must not share a media key")
	}

	base.SourceRevision = func(context.Context, string) (string, error) { return "revision-a", nil }
	base.TargetDuration = 4
	third := mustNew(base)
	thirdKey, err := third.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve third key: %v", err)
	}
	if firstKey == thirdKey {
		t.Fatalf("pipeline changes must not share a media key")
	}
}

func TestLocalSourcePathHandlesFilesWithoutParsingRemoteURLs(t *testing.T) {
	for input, want := range map[string]string{
		"/media/video.mkv":             "/media/video.mkv",
		"file:///media/My%20Video.mkv": "/media/My Video.mkv",
		"file://localhost/media/a.mkv": "/media/a.mkv",
	} {
		got, local := localSourcePath(input)
		if !local || got != want {
			t.Fatalf("localSourcePath(%q) = %q, %v; want %q, true", input, got, local, want)
		}
	}
	if _, local := localSourcePath("https://[::1/video?token=secret"); local {
		t.Fatalf("remote URL must not be treated as a local file")
	}
}

func TestAssetIdentityIncludesRequestedConfiguration(t *testing.T) {
	first := assetIdentity("media", "preview", domain.PreviewConfig{MaxFrames: 20, Frameduration: 200, Width: 320})
	second := assetIdentity("media", "preview", domain.PreviewConfig{MaxFrames: 40, Frameduration: 200, Width: 320})
	if first == second {
		t.Fatalf("preview configurations must not share an artifact key")
	}
}

func TestPipelineVersionChangesWithTimelineOffset(t *testing.T) {
	config := &domain.HWAccelConfig{Accelerator: domain.AccelNone}
	base := pipelineIdentity(Config{TargetDuration: 6}, config, 10*time.Second, 1)
	shifted := pipelineIdentity(Config{TargetDuration: 6}, config, 1400*time.Millisecond, 1)
	if base == shifted {
		t.Fatalf("a different output timeline offset must change the pipeline version")
	}
	if pipelineVersion(Config{TargetDuration: 6}, config) != pipelineIdentity(Config{TargetDuration: 6}, config, domain.OutputTimelineOffset, domain.JobContractVersion) {
		t.Fatalf("the pipeline version must be derived from the domain timeline offset and job contract version")
	}
}

func TestPipelineVersionChangesWithJobContractVersion(t *testing.T) {
	config := &domain.HWAccelConfig{Accelerator: domain.AccelNone}
	base := pipelineIdentity(Config{TargetDuration: 6}, config, 10*time.Second, 1)
	bumped := pipelineIdentity(Config{TargetDuration: 6}, config, 10*time.Second, 2)
	if base == bumped {
		t.Fatalf("a different job contract version must change the pipeline version")
	}
}
