package transcode

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type ackFailingCoordinator struct {
	*stubCoordinator
	ackCalls atomic.Int32
}

func (c *ackFailingCoordinator) Ack(ctx context.Context, receipt string) error {
	c.ackCalls.Add(1)
	return errors.New("ack unavailable")
}

type countingSubscribeCoordinator struct {
	*stubCoordinator
	subscribes atomic.Int32
}

func (c *countingSubscribeCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	c.subscribes.Add(1)
	return make(chan domain.JobDelivery), nil
}

func TestInvalidDeliveriesWithEmptyRunIDAreEachRejected(t *testing.T) {
	coord := &stubCoordinator{}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	if !pool.beginJob("", func(error) {}) {
		t.Fatal("expected the empty run id slot to be free")
	}

	first := validPoolDelivery("first", "")
	first.Job.ID = ""
	second := validPoolDelivery("second", "")
	second.Job.ID = ""
	pool.processJob(context.Background(), first)
	pool.processJob(context.Background(), second)

	if recorder.count(domain.EventJobDeliveryRejected) != 2 {
		t.Fatalf("every invalid delivery must be rejected even while an empty run id is active: %v", recorder.snapshot())
	}
	if len(coord.acks) != 2 || coord.acks[0] != "first" || coord.acks[1] != "second" {
		t.Fatalf("every invalid delivery must be acknowledged: %v", coord.acks)
	}
}

func TestRejectedDeliveryAckFailureStillEmitsRejection(t *testing.T) {
	coord := &ackFailingCoordinator{stubCoordinator: &stubCoordinator{}}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	delivery := validPoolDelivery("receipt", "")
	delivery.Job.ID = ""

	pool.processJob(context.Background(), delivery)

	if coord.ackCalls.Load() == 0 {
		t.Fatal("the rejected delivery must be acknowledged")
	}
	if recorder.count(domain.EventJobAckFailed) != 1 {
		t.Fatalf("the ack failure must be reported: %v", recorder.snapshot())
	}
	rejected := recorder.count(domain.EventJobDeliveryRejected)
	if rejected != 1 {
		t.Fatalf("a rejection must be reported even when its ack fails, got %d: %v", rejected, recorder.snapshot())
	}
	for _, event := range recorder.snapshot() {
		if event.Type == domain.EventJobDeliveryRejected && !strings.Contains(event.Error, "invalid job contract") {
			t.Fatalf("rejection must carry its reason: %q", event.Error)
		}
	}
}

func TestStaleSubscriptionGenerationDoesNotResubscribe(t *testing.T) {
	coord := &countingSubscribeCoordinator{stubCoordinator: &stubCoordinator{}}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	pool.newBackoff = fastBackoff
	sub := &subscription{ch: make(chan domain.JobDelivery), gen: 3, delays: fastBackoff()}

	if !pool.resubscribe(context.Background(), sub, 2) {
		t.Fatal("a worker holding a stale generation must keep running")
	}

	if coord.subscribes.Load() != 0 {
		t.Fatalf("a stale generation must not subscribe again, got %d subscribes", coord.subscribes.Load())
	}
	if recorder.count(domain.EventPoolSubscriptionLost) != 0 {
		t.Fatalf("a stale generation must not report a lost subscription: %v", recorder.snapshot())
	}
	if _, gen := sub.current(); gen != 3 {
		t.Fatalf("generation must be untouched, got %d", gen)
	}

	if !pool.resubscribe(context.Background(), sub, 3) {
		t.Fatal("the current generation must resubscribe")
	}
	if coord.subscribes.Load() != 1 {
		t.Fatalf("the current generation must subscribe exactly once, got %d", coord.subscribes.Load())
	}
	if _, gen := sub.current(); gen != 4 {
		t.Fatalf("resubscribe must advance the generation, got %d", gen)
	}
}

func TestErrJobExpiredNamesStartBy(t *testing.T) {
	if !strings.Contains(ErrJobExpired.Error(), "start-by") || strings.Contains(ErrJobExpired.Error(), "deadline") {
		t.Fatalf("unexpected expiry text %q", ErrJobExpired.Error())
	}
}

func TestLeaseIsLostWithinTwoThirdsOfTTLAfterLastRenewal(t *testing.T) {
	coord := &renewScriptCoordinator{
		stubCoordinator: &stubCoordinator{},
		fail:            func(int32) bool { return true },
		reached:         -1,
	}
	pool := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	baseCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ttl := 900 * time.Millisecond
	start := time.Now()
	go pool.renewLease(baseCtx, validPoolDelivery("receipt", "run"), ttl, cancel, start, &leaseSettlement{})

	select {
	case <-baseCtx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("persistent renew failure never cancelled the job")
	}
	elapsed := time.Since(start)
	if elapsed < ttl/2 {
		t.Fatalf("lease cancelled after %v, before two thirds of the ttl could elapse", elapsed)
	}
	if elapsed > ttl+ttl/2 {
		t.Fatalf("lease cancelled after %v, later than the ttl %v allows", elapsed, ttl)
	}
}

func TestPoolRejectsJobWithoutExpectedIndices(t *testing.T) {
	coord := &stubCoordinator{}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	delivery := validPoolDelivery("receipt", "run")
	delivery.Job.ExpectedIndices = nil

	pool.processJob(context.Background(), delivery)

	if recorder.count(domain.EventJobDeliveryRejected) != 1 {
		t.Fatalf("job without expected indices must be rejected: %v", recorder.snapshot())
	}
}
