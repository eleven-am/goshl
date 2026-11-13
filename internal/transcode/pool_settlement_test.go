package transcode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestFailJobReportsWhetherTheFailureWasSettled(t *testing.T) {
	delivery := domain.JobDelivery{Job: domain.Job{ID: "job", RunID: "run", MediaKey: "media", Rendition: "720p", StreamType: domain.StreamVideo, StartIndex: 4, EndIndex: 7}, Receipt: "receipt"}
	reason := errors.New("ffmpeg failed")

	recorder := newEventRecorder()
	settling := NewPool(&stubCoordinator{}, 1, domain.StreamVideo, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	if !settling.failJob(context.Background(), delivery, reason, time.Now()) {
		t.Fatal("a failure the coordinator accepted must count as settled")
	}
	failed := recorder.snapshot()
	if len(failed) != 1 || failed[0].Type != domain.EventJobFailed || failed[0].JobID != "job" || failed[0].MediaKey != "media" || failed[0].StartIndex != 4 || failed[0].EndIndex != 7 || failed[0].Error != "ffmpeg failed" {
		t.Fatalf("a settled failure must emit one complete EventJobFailed, got %+v", failed)
	}

	recorder = newEventRecorder()
	refusing := NewPool(&stubCoordinator{jobErr: errors.New("coordinator down")}, 1, domain.StreamVideo, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	if refusing.failJob(context.Background(), delivery, reason, time.Now()) {
		t.Fatal("a failure the coordinator did not accept must not count as settled")
	}
	if recorder.count(domain.EventJobFailed) != 0 || recorder.count(domain.EventJobSettleFailed) != 1 {
		t.Fatalf("an unsettled failure must emit EventJobSettleFailed only, got %+v", recorder.snapshot())
	}
}
