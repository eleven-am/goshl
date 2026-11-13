package goshl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

type jobCapturingCoordinator struct {
	*segTestCoordinator
	jobs chan domain.Job
}

func (c *jobCapturingCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.jobs <- job
	return c.segTestCoordinator.Enqueue(ctx, job)
}

func renditionTestController(t *testing.T, meta *domain.Metadata, target float64) (*Controller, *jobCapturingCoordinator) {
	t.Helper()
	raw, err := jsonMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &jobCapturingCoordinator{segTestCoordinator: newSegTestCoordinator(), jobs: make(chan domain.Job, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	controller, err := NewControllerWithContext(ctx, Options{
		Storage:        &segTestStorage{meta: raw},
		Coordinator:    coordinator,
		PathGen:        stubPathGen{},
		TargetDuration: target,
		SegmentTimeout: 200 * time.Millisecond,
		JobTimeout:     5 * time.Second,
		SourceRevision: func(context.Context, string) (string, error) { return "r1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller, coordinator
}

func capturedJob(t *testing.T, controller *Controller, coordinator *jobCapturingCoordinator, name string, index int) domain.Job {
	t.Helper()
	if _, err := controller.Segment(context.Background(), "/x.mp4", StreamVideo, name, index); !errors.Is(err, ErrSegmentTimeout) {
		t.Fatalf("segment %d: error = %v, want a timeout after the job is enqueued", index, err)
	}
	select {
	case job := <-coordinator.jobs:
		return job
	default:
		t.Fatalf("segment %d enqueued no job", index)
		return domain.Job{}
	}
}

func TestSplitPlanSourceRungIsTranscodedWithoutCopyNeighbours(t *testing.T) {
	meta := testVideoMetadata(32, []float64{0, 20, 26})
	meta.Video.Level = 41

	controller, coordinator := renditionTestController(t, meta, 6)
	if playlist.KeyframeAligned(meta.Keyframes, meta.Duration, 6) {
		t.Fatalf("fixture must need a split plan at a 6 s target")
	}
	job := capturedJob(t, controller, coordinator, "1080p", 2)
	if job.VideoRendition == nil || job.VideoRendition.Method != domain.Transcode || job.Trailing != nil || job.Overlap != nil {
		t.Fatalf("a split plan must transcode the source rung with no copy neighbours, got rendition %+v overlap %+v trailing %+v", job.VideoRendition, job.Overlap, job.Trailing)
	}
	if job.VideoRendition.Level != 40 || job.VideoRendition.BufferSize == 0 || job.VideoRendition.PeakBitrate == 0 {
		t.Fatalf("the job must carry the encoder facts, got %+v", job.VideoRendition)
	}
	master, err := controller.MasterPlaylist(context.Background(), "/x.mp4")
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	if !strings.Contains(master, "RESOLUTION=1920x1080,CODECS=\"avc1.640028\"") {
		t.Fatalf("master must advertise the transcoded source rung: %s", master)
	}

	aligned, alignedCoordinator := renditionTestController(t, meta, 12)
	if !playlist.KeyframeAligned(meta.Keyframes, meta.Duration, 12) {
		t.Fatalf("fixture must be keyframe aligned at a 12 s target")
	}
	job = capturedJob(t, aligned, alignedCoordinator, "1080p", 1)
	if job.VideoRendition == nil || job.VideoRendition.Method != domain.DirectStream || job.Overlap == nil {
		t.Fatalf("the same source at a 12 s target must copy the source rung, got rendition %+v overlap %+v", job.VideoRendition, job.Overlap)
	}
	master, err = aligned.MasterPlaylist(context.Background(), "/x.mp4")
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	if !strings.Contains(master, "RESOLUTION=1920x1080,CODECS=\"avc1.640029\"") {
		t.Fatalf("master must advertise the copied source rung at the source level: %s", master)
	}
}

func TestSourceWithoutEncodableVideoFailsWithReason(t *testing.T) {
	meta := testVideoMetadata(24, []float64{0, 6, 12, 18})
	meta.Video.Codec = "hevc"
	meta.Video.FrameRate = 5000
	controller, coordinator := renditionTestController(t, meta, 6)

	_, err := controller.MasterPlaylist(context.Background(), "/x.mp4")
	if !errors.Is(err, rendition.ErrNoEncodableVideo) || !strings.Contains(err.Error(), "360p") {
		t.Fatalf("master error = %v, want the no-encodable-rendition error naming the rejected rungs", err)
	}
	if _, err := controller.VariantPlaylist(context.Background(), "/x.mp4", StreamVideo, "1080p"); !errors.Is(err, ErrRenditionNotFound) {
		t.Fatalf("variant error = %v, want ErrRenditionNotFound", err)
	}
	if _, err := controller.Segment(context.Background(), "/x.mp4", StreamVideo, "1080p", 0); !errors.Is(err, ErrRenditionNotFound) || !strings.Contains(err.Error(), "5.2") {
		t.Fatalf("segment error = %v, want ErrRenditionNotFound with the level reason", err)
	}
	if len(coordinator.jobs) != 0 {
		t.Fatalf("no job may be enqueued for a source without an encodable rendition")
	}
}

func droppedRenditionEvents(t *testing.T, meta *domain.Metadata) ([]domain.Event, string, error) {
	t.Helper()
	raw, err := jsonMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	var events []domain.Event
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	controller, err := NewControllerWithContext(ctx, Options{
		Storage:        &segTestStorage{meta: raw},
		Coordinator:    newSegTestCoordinator(),
		PathGen:        stubPathGen{},
		SourceRevision: func(context.Context, string) (string, error) { return "r1", nil },
		Observer: ObserverFunc(func(_ context.Context, event Event) {
			if event.Type == EventRenditionDropped {
				events = append(events, event)
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	master, err := controller.MasterPlaylist(context.Background(), "/x.mkv")
	return events, master, err
}

func TestMasterPlaylistReportsEveryDroppedRendition(t *testing.T) {
	meta := testVideoMetadata(24, []float64{0, 6, 12, 18})
	meta.Video.Codec = "hevc"

	events, _, err := droppedRenditionEvents(t, meta)
	if err != nil || len(events) != 0 {
		t.Fatalf("a 1080p source drops nothing, got %v and %+v", err, events)
	}

	meta.Video.Width, meta.Video.Height, meta.Video.Bitrate = 7680, 4320, 80_000_000
	events, master, err := droppedRenditionEvents(t, meta)
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	if len(events) != 1 || events[0].Rendition != "4320p" || events[0].StreamType != StreamVideo || events[0].MediaKey == "" || !strings.Contains(events[0].Error, "5.2") || events[0].Time.IsZero() {
		t.Fatalf("expected one dropped-rendition event for 4320p with its reason, got %+v", events)
	}
	if strings.Contains(master, "7680x4320") || !strings.Contains(master, "RESOLUTION=3840x2160") {
		t.Fatalf("master must offer 2160p and not the dropped rung: %s", master)
	}

	meta.Video.Width, meta.Video.Height, meta.Video.FrameRate = 1920, 1080, 5000
	events, _, err = droppedRenditionEvents(t, meta)
	if !errors.Is(err, rendition.ErrNoEncodableVideo) {
		t.Fatalf("master error = %v, want the no-encodable-rendition error", err)
	}
	var names []string
	for _, event := range events {
		names = append(names, event.Rendition)
	}
	if strings.Join(names, ",") != "1080p,720p,480p,360p" {
		t.Fatalf("every dropped rung must be reported before the failure, got %v", names)
	}
}
