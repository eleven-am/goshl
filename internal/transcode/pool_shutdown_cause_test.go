package transcode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

func failJobSettlements(t *testing.T, ctx context.Context) *stubCoordinator {
	t.Helper()
	coord := &stubCoordinator{}
	pool := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	delivery := domain.JobDelivery{Job: domain.Job{ID: "job", RunID: "run", Rendition: "720p"}, Receipt: "receipt"}
	pool.failJob(ctx, delivery, errors.New("ffmpeg interrupted"), time.Now())
	return coord
}

func TestShutdownWithCancelCauseLeavesRunningJobPending(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("host shutting down"))

	coord := failJobSettlements(t, ctx)

	if len(coord.jobPublishes) != 0 || len(coord.acks) != 0 {
		t.Fatalf("a job cancelled by its parent with a cause must stay pending: statuses=%v acks=%v", coord.jobPublishes, coord.acks)
	}
}

func TestShutdownWithPlainCancelLeavesRunningJobPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	coord := failJobSettlements(t, ctx)

	if len(coord.jobPublishes) != 0 || len(coord.acks) != 0 {
		t.Fatalf("a plainly cancelled job must stay pending: statuses=%v acks=%v", coord.jobPublishes, coord.acks)
	}
}

func TestLostLeaseLeavesRunningJobPending(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrDeliveryLeaseLost)

	coord := failJobSettlements(t, ctx)

	if len(coord.jobPublishes) != 0 || len(coord.acks) != 0 {
		t.Fatalf("a job that lost its lease must stay pending: statuses=%v acks=%v", coord.jobPublishes, coord.acks)
	}
}

func TestJobTimeoutIsSettledAsFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	coord := failJobSettlements(t, ctx)

	if len(coord.jobPublishes) != 1 || coord.jobPublishes[0].State != domain.JobStateFailed {
		t.Fatalf("a job that hit its own timeout must be settled as failed: %v", coord.jobPublishes)
	}
	if len(coord.acks) != 1 {
		t.Fatalf("a failed job must be acknowledged: %v", coord.acks)
	}
}
