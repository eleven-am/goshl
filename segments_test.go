package goshl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type segTestStorage struct {
	Storage
	meta   []byte
	ready  atomic.Bool
	mu     sync.Mutex
	checks []time.Time
	seen   []string
}

func (s *segTestStorage) MetadataExists(context.Context, string) (bool, error) { return true, nil }

func (s *segTestStorage) GetMetadata(context.Context, string) ([]byte, error) { return s.meta, nil }

func (s *segTestStorage) SegmentExists(_ context.Context, info domain.SegmentData) (bool, error) {
	s.mu.Lock()
	s.checks = append(s.checks, time.Now())
	s.seen = append(s.seen, info.Rendition)
	s.mu.Unlock()
	return s.ready.Load(), nil
}

func (s *segTestStorage) ReadSegment(context.Context, domain.SegmentData) ([]byte, error) {
	return []byte("segment"), nil
}

func (s *segTestStorage) snapshot() ([]time.Time, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.checks...), append([]string(nil), s.seen...)
}

type segTestCoordinator struct {
	Coordinator
	enqueues   atomic.Int32
	enqueueErr func(call int32) error
	acquire    func(call int32) (string, string, error)
	acquires   atomic.Int32
	released   chan string
	waitJobs   chan string
}

func newSegTestCoordinator() *segTestCoordinator {
	return &segTestCoordinator{released: make(chan string, 16), waitJobs: make(chan string, 16)}
}

func (c *segTestCoordinator) Subscribe(ctx context.Context, _ domain.StreamType, _ string) (<-chan domain.JobDelivery, error) {
	return make(chan domain.JobDelivery), nil
}

func (c *segTestCoordinator) Enqueue(_ context.Context, _ domain.Job) error {
	call := c.enqueues.Add(1)
	if c.enqueueErr != nil {
		return c.enqueueErr(call)
	}
	return nil
}

func (c *segTestCoordinator) WaitJob(_ context.Context, runID string) (<-chan domain.JobStatus, error) {
	select {
	case c.waitJobs <- runID:
	default:
	}
	return make(chan domain.JobStatus), nil
}

func (c *segTestCoordinator) WaitSegment(context.Context, domain.SegmentData) (<-chan domain.SegmentStatus, error) {
	return make(chan domain.SegmentStatus), nil
}

func (c *segTestCoordinator) AcquireJob(context.Context, string, string, time.Duration) (string, string, error) {
	call := c.acquires.Add(1)
	if c.acquire != nil {
		return c.acquire(call)
	}
	return "token", "", nil
}

func (c *segTestCoordinator) ReleaseJob(_ context.Context, jobID, _, _ string) error {
	select {
	case c.released <- jobID:
	default:
	}
	return nil
}

func newSegTestController(t *testing.T, store *segTestStorage, coord *segTestCoordinator, mutate func(*Options)) *Controller {
	t.Helper()
	keyframes := make([]float64, 20)
	for i := range keyframes {
		keyframes[i] = float64(i * 6)
	}
	meta := &domain.Metadata{Duration: 120, Keyframes: keyframes, Video: domain.VideoStream{Codec: "hevc", Width: 1280, Height: 720, FrameRate: 24}}
	raw, err := jsonMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	store.meta = raw
	opts := Options{
		Storage:        store,
		Coordinator:    coord,
		PathGen:        stubPathGen{},
		SegmentTimeout: 300 * time.Millisecond,
		JobTimeout:     5 * time.Second,
		SourceRevision: func(context.Context, string) (string, error) { return "r1", nil },
	}
	if mutate != nil {
		mutate(&opts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctl, err := NewControllerWithContext(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ctl
}

func TestSegmentEnqueueFailureReleasesFlightAndLock(t *testing.T) {
	store := &segTestStorage{}
	coord := newSegTestCoordinator()
	coord.enqueueErr = func(call int32) error {
		if call == 1 {
			return fmt.Errorf("queue down: %w", ErrEnqueueRejected)
		}
		return nil
	}
	ctl := newSegTestController(t, store, coord, nil)

	_, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0)
	if err == nil || !strings.Contains(err.Error(), "queue down") {
		t.Fatalf("first call error = %v, want enqueue failure", err)
	}
	select {
	case <-coord.released:
	case <-time.After(2 * time.Second):
		t.Fatal("job lock was not released after enqueue failure")
	}

	_, err = ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0)
	if !errors.Is(err, ErrSegmentTimeout) {
		t.Fatalf("second call error = %v, want ErrSegmentTimeout from a fresh run", err)
	}
	if got := coord.enqueues.Load(); got != 2 {
		t.Fatalf("enqueues = %d, want 2 (second request must re-enqueue)", got)
	}
}

func TestSegmentAmbiguousEnqueueErrorKeepsRun(t *testing.T) {
	store := &segTestStorage{}
	coord := newSegTestCoordinator()
	coord.enqueueErr = func(int32) error { return errors.New("connection lost") }
	ctl := newSegTestController(t, store, coord, nil)

	if _, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("error = %v, want enqueue error", err)
	}
	select {
	case id := <-coord.released:
		t.Fatalf("lock %s released after ambiguous enqueue error", id)
	case <-time.After(100 * time.Millisecond):
	}
	_, _ = ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0)
	if got := coord.enqueues.Load(); got != 1 {
		t.Fatalf("enqueues = %d, want 1 (second request joins the registered run)", got)
	}
}

func TestSegmentCancelledBeforeEnqueueReleasesFlightAndLock(t *testing.T) {
	store := &segTestStorage{}
	coord := newSegTestCoordinator()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coord.acquire = func(int32) (string, string, error) {
		cancel()
		return "token", "", nil
	}
	coord.enqueueErr = func(int32) error { return ctx.Err() }
	ctl := newSegTestController(t, store, coord, nil)

	if _, err := ctl.Segment(ctx, "/x.mkv", StreamVideo, "720p", 0); err == nil {
		t.Fatal("expected cancellation error")
	}
	select {
	case <-coord.released:
	case <-time.After(2 * time.Second):
		t.Fatal("job lock was not released after cancellation before enqueue")
	}
}

func TestSegmentJoinerFollowsActiveRunWhenCreatorFindsLockHeld(t *testing.T) {
	store := &segTestStorage{}
	coord := newSegTestCoordinator()
	entered := make(chan struct{})
	gate := make(chan struct{})
	coord.acquire = func(call int32) (string, string, error) {
		if call == 1 {
			close(entered)
			<-gate
		}
		return "", "other-run", ErrLockHeld
	}
	joined := make(chan struct{}, 4)
	ctl := newSegTestController(t, store, coord, func(o *Options) {
		o.SegmentTimeout = 5 * time.Second
		o.Observer = ObserverFunc(func(_ context.Context, event Event) {
			if event.Type == EventJobJoined {
				joined <- struct{}{}
			}
		})
	})

	type result struct {
		data []byte
		err  error
	}
	creator := make(chan result, 1)
	joiner := make(chan result, 1)
	go func() {
		data, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0)
		creator <- result{data, err}
	}()
	<-entered
	go func() {
		data, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 1)
		joiner <- result{data, err}
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("joiner did not join the creator flight")
	}
	close(gate)

	for i := 0; i < 2; i++ {
		select {
		case runID := <-coord.waitJobs:
			if runID != "other-run" {
				t.Fatalf("waited on run %q, want other-run", runID)
			}
		case r := <-joiner:
			t.Fatalf("joiner returned early: data=%q err=%v", r.data, r.err)
		case <-time.After(3 * time.Second):
			t.Fatal("creator and joiner did not both follow the active run")
		}
	}
	store.ready.Store(true)

	for name, ch := range map[string]chan result{"creator": creator, "joiner": joiner} {
		select {
		case r := <-ch:
			if r.err != nil || string(r.data) != "segment" {
				t.Fatalf("%s result = %q, %v", name, r.data, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not return", name)
		}
	}
}

func TestSegmentFlightExpiryIsErrSegmentTimeout(t *testing.T) {
	store := &segTestStorage{}
	coord := newSegTestCoordinator()
	ctl := newSegTestController(t, store, coord, func(o *Options) {
		o.SegmentTimeout = 10 * time.Second
		o.JobTimeout = 150 * time.Millisecond
	})

	_, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0)
	if !errors.Is(err, ErrSegmentTimeout) {
		t.Fatalf("error = %v, want ErrSegmentTimeout", err)
	}
}

func TestSegmentRejectsUnsafeRenditionNameBeforeStorage(t *testing.T) {
	for _, name := range []string{"../../../etc/passwd", "a/b", "", ".hidden", "a..b"} {
		store := &segTestStorage{}
		ctl := newSegTestController(t, store, newSegTestCoordinator(), nil)
		_, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, name, 0)
		if !errors.Is(err, ErrRenditionNotFound) {
			t.Fatalf("name %q: error = %v, want ErrRenditionNotFound", name, err)
		}
		if _, seen := store.snapshot(); len(seen) != 0 {
			t.Fatalf("name %q reached Storage: %v", name, seen)
		}
	}
}

func TestSegmentPollingBacksOff(t *testing.T) {
	store := &segTestStorage{}
	coord := newSegTestCoordinator()
	ctl := newSegTestController(t, store, coord, func(o *Options) {
		o.SegmentTimeout = 6 * time.Second
	})

	_, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0)
	if !errors.Is(err, ErrSegmentTimeout) {
		t.Fatalf("error = %v, want ErrSegmentTimeout", err)
	}
	checks, _ := store.snapshot()
	if len(checks) > 20 {
		t.Fatalf("storage polled %d times in 6s, want exponential backoff capped at 1s (<= 20)", len(checks))
	}
	var widest time.Duration
	for i := 1; i < len(checks); i++ {
		if gap := checks[i].Sub(checks[i-1]); gap > widest {
			widest = gap
		}
	}
	if widest < 900*time.Millisecond || widest > 1300*time.Millisecond {
		t.Fatalf("widest poll gap = %v, want the 1s cap (900ms..1.3s)", widest)
	}
}

type jobWatchProbe struct {
	ctx      context.Context
	runID    string
	statuses chan domain.JobStatus
}

type jobWatchProbeCoordinator struct {
	*segTestCoordinator
	watches chan jobWatchProbe
}

func (c *jobWatchProbeCoordinator) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	statuses := make(chan domain.JobStatus, 1)
	c.watches <- jobWatchProbe{ctx: ctx, runID: runID, statuses: statuses}
	return statuses, nil
}

func TestSegmentJobWatchIsCancelledWhenFlightEndsWhileControllerRuns(t *testing.T) {
	newProbe := func(t *testing.T) (*Controller, *jobWatchProbeCoordinator) {
		coord := &jobWatchProbeCoordinator{segTestCoordinator: newSegTestCoordinator(), watches: make(chan jobWatchProbe, 4)}
		ctl := newSegTestController(t, &segTestStorage{}, coord.segTestCoordinator, func(o *Options) {
			o.Coordinator = coord
			o.SegmentTimeout = 30 * time.Second
		})
		return ctl, coord
	}
	watchOf := func(t *testing.T, coord *jobWatchProbeCoordinator) jobWatchProbe {
		t.Helper()
		select {
		case watch := <-coord.watches:
			return watch
		case <-time.After(5 * time.Second):
			t.Fatal("the request never opened a job watch")
			return jobWatchProbe{}
		}
	}

	t.Run("terminal job status", func(t *testing.T) {
		ctl, coord := newProbe(t)
		result := make(chan error, 1)
		go func() {
			_, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0)
			result <- err
		}()
		watch := watchOf(t, coord)
		watch.statuses <- domain.JobStatus{RunID: watch.runID, State: domain.JobStateFailed, Error: "encoder crashed"}

		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "encoder crashed") {
				t.Fatalf("error = %v, want the job failure", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Segment did not return after the job failed")
		}
		if watch.ctx.Err() == nil {
			t.Fatal("the job watch stayed open after its flight finished")
		}
		if ctl.lifecycle.Stopped() {
			t.Fatal("the controller must still be running")
		}
	})

	t.Run("rejected enqueue", func(t *testing.T) {
		ctl, coord := newProbe(t)
		coord.enqueueErr = func(int32) error { return fmt.Errorf("queue down: %w", ErrEnqueueRejected) }

		if _, err := ctl.Segment(context.Background(), "/x.mkv", StreamVideo, "720p", 0); err == nil || !strings.Contains(err.Error(), "queue down") {
			t.Fatalf("error = %v, want enqueue failure", err)
		}
		watch := watchOf(t, coord)
		if watch.ctx.Err() == nil {
			t.Fatal("the job watch stayed open after its flight was abandoned")
		}
		if ctl.lifecycle.Stopped() {
			t.Fatal("the controller must still be running")
		}
	})
}
