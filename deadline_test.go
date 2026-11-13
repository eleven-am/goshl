package goshl

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type countingCoordinator struct {
	*clusterCoordinator
	enqueues atomic.Int32
}

func (c *countingCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.enqueues.Add(1)
	return c.clusterCoordinator.Enqueue(ctx, job)
}

func TestRunningJobIsNotEnqueuedTwice(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := &countingCoordinator{clusterCoordinator: newClusterCoordinator()}
	jobTimeout := time.Second
	requester := NewController(Options{
		Storage:        store,
		Coordinator:    coordinator,
		PathGen:        stubPathGen{},
		SegmentTimeout: 50 * time.Millisecond,
		JobTimeout:     jobTimeout,
	})

	enqueuedAt := time.Now()
	if _, err := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0); err == nil {
		t.Fatal("expected a timeout while no worker is running")
	}
	coordinator.mu.Lock()
	var expires time.Time
	for _, lock := range coordinator.locks {
		expires = lock.expires
	}
	coordinator.mu.Unlock()
	time.Sleep(jobTimeout + 100*time.Millisecond)
	if _, err := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0); err == nil {
		t.Fatal("expected a timeout while no worker is running")
	}
	if got := coordinator.enqueues.Load(); got != 1 {
		t.Fatalf("a second request after the first job's start budget must not enqueue a duplicate, got %d enqueues", got)
	}
	if minimum := enqueuedAt.Add(2*jobTimeout + time.Minute - 20*time.Millisecond); expires.Before(minimum) {
		t.Fatalf("lock must outlive queue wait plus a full execution: expires in %v, need at least %v", expires.Sub(enqueuedAt), minimum.Sub(enqueuedAt))
	}
}
