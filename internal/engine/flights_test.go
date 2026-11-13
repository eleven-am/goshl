package engine

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

type faultCoordinator struct {
	*clusterCoordinator
	acquire     func(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error)
	release     func(ctx context.Context, jobID, runID, token string) error
	waitJob     func(ctx context.Context, runID string) (<-chan domain.JobStatus, error)
	waitSegment func(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error)
	enqueue     func(ctx context.Context, job domain.Job) error
	subscribes  atomic.Int32
	claims      atomic.Int32
	probes      chan struct{}
}

func (c *faultCoordinator) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	if ttl < time.Minute {
		if c.probes != nil {
			select {
			case c.probes <- struct{}{}:
			default:
			}
		}
	} else {
		c.claims.Add(1)
	}
	if c.acquire != nil {
		return c.acquire(ctx, jobID, runID, ttl)
	}
	return c.clusterCoordinator.AcquireJob(ctx, jobID, runID, ttl)
}

func (c *faultCoordinator) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	if c.release != nil {
		return c.release(ctx, jobID, runID, token)
	}
	return c.clusterCoordinator.ReleaseJob(ctx, jobID, runID, token)
}

func (c *faultCoordinator) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	if c.waitJob != nil {
		return c.waitJob(ctx, runID)
	}
	return c.clusterCoordinator.WaitJob(ctx, runID)
}

func (c *faultCoordinator) WaitSegment(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error) {
	if c.waitSegment != nil {
		return c.waitSegment(ctx, info)
	}
	return c.clusterCoordinator.WaitSegment(ctx, info)
}

func (c *faultCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	if c.enqueue != nil {
		return c.enqueue(ctx, job)
	}
	return c.clusterCoordinator.Enqueue(ctx, job)
}

func (c *faultCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	c.subscribes.Add(1)
	return c.clusterCoordinator.Subscribe(ctx, streamType, profile)
}

func (c *faultCoordinator) heldLocks() int {
	c.clusterCoordinator.mu.Lock()
	defer c.clusterCoordinator.mu.Unlock()
	return len(c.clusterCoordinator.locks)
}

func newFaultCoordinator() *faultCoordinator {
	return &faultCoordinator{clusterCoordinator: newClusterCoordinator()}
}

func pathController(t *testing.T, coordinator domain.Coordinator, store domain.Storage, segmentTimeout, jobTimeout time.Duration, observer domain.Observer) *Engine {
	t.Helper()
	return mustNew(Config{
		Storage:        store,
		Coordinator:    coordinator,
		PathGen:        stubPathGen{},
		SegmentTimeout: segmentTimeout,
		JobTimeout:     jobTimeout,
		Observer:       observer,
	})
}

func registeredFlights(controller *Engine) int {
	controller.flights.mu.Lock()
	defer controller.flights.mu.Unlock()
	return len(controller.flights.flights)
}

func pathRequest(controller *Engine, ctx context.Context) chan segmentOutcome {
	result := make(chan segmentOutcome, 1)
	go func() {
		data, err := controller.Segment(ctx, "file:///media", domain.StreamVideo, "1080p", 0)
		result <- segmentOutcome{data: data, err: err}
	}()
	return result
}

type segmentOutcome struct {
	data []byte
	err  error
}

func awaitOutcome(t *testing.T, result chan segmentOutcome, what string) segmentOutcome {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the request did not return", what)
		return segmentOutcome{}
	}
}

func requireFlightGone(t *testing.T, controller *Engine, coordinator *faultCoordinator, what string) {
	t.Helper()
	if flights := registeredFlights(controller); flights != 0 {
		t.Fatalf("%s: %d flights stay registered", what, flights)
	}
	if locks := coordinator.heldLocks(); locks != 0 {
		t.Fatalf("%s: %d job locks stay held", what, locks)
	}
}

func deliverSegment(t *testing.T, controller *Engine, coordinator *clusterCoordinator, store domain.Storage, data string) {
	t.Helper()
	info := raceSegmentInfo(t, controller, 0)
	if err := store.WriteSegment(context.Background(), info, []byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.NotifySegment(context.Background(), info, domain.SegmentStatus{State: domain.SegmentStateReady}); err != nil {
		t.Fatal(err)
	}
}

func TestStopCancelsAnEnqueueInProgress(t *testing.T) {
	coordinator := newFaultCoordinator()
	entered := make(chan struct{})
	coordinator.enqueue = func(ctx context.Context, job domain.Job) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	controller := pathController(t, coordinator, raceStore(t), time.Minute, time.Hour, nil)
	result := pathRequest(controller, context.Background())
	<-entered
	controller.Stop()
	if outcome := awaitOutcome(t, result, "Stop during Enqueue"); outcome.err == nil {
		t.Fatal("a request whose enqueue was cancelled by Stop must fail")
	}
}

func TestEnqueueOutlivingTheSetupTimeoutFreesTheBatch(t *testing.T) {
	coordinator := newFaultCoordinator()
	coordinator.enqueue = func(ctx context.Context, job domain.Job) error {
		<-ctx.Done()
		return ctx.Err()
	}
	controller := pathController(t, coordinator, raceStore(t), 150*time.Millisecond, time.Hour, nil)
	defer controller.Stop()
	outcome := awaitOutcome(t, pathRequest(controller, context.Background()), "Enqueue past the setup timeout")
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "enqueue") || !errors.Is(outcome.err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the enqueue deadline error", outcome.err)
	}
	requireFlightGone(t, controller, coordinator, "enqueue past the setup timeout")
}

func TestLockHeldWithoutAnActiveRunIsAnError(t *testing.T) {
	coordinator := newFaultCoordinator()
	coordinator.acquire = func(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
		return "", "", domain.ErrLockHeld
	}
	controller := pathController(t, coordinator, raceStore(t), 300*time.Millisecond, time.Hour, nil)
	defer controller.Stop()
	outcome := awaitOutcome(t, pathRequest(controller, context.Background()), "lock held without a run")
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "active run missing") {
		t.Fatalf("got %v, want the missing active run error", outcome.err)
	}
	if flights := registeredFlights(controller); flights != 0 {
		t.Fatalf("%d flights stay registered", flights)
	}
}

func TestSegmentStoredWhileTheLockWasAcquiredIsServedAndFreesTheBatch(t *testing.T) {
	coordinator := newFaultCoordinator()
	store := raceStore(t)
	var controller *Engine
	coordinator.acquire = func(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
		token, active, err := coordinator.clusterCoordinator.AcquireJob(ctx, jobID, runID, ttl)
		store.setSegment(0, []byte("stored meanwhile"))
		return token, active, err
	}
	controller = pathController(t, coordinator, store, time.Minute, time.Hour, nil)
	defer controller.Stop()
	outcome := awaitOutcome(t, pathRequest(controller, context.Background()), "segment stored during acquire")
	if outcome.err != nil || string(outcome.data) != "stored meanwhile" {
		t.Fatalf("got %q, %v", outcome.data, outcome.err)
	}
	requireFlightGone(t, controller, coordinator, "segment stored during acquire")
}

func TestSetupFailuresFreeTheBatch(t *testing.T) {
	down := errors.New("coordinator down")
	cases := map[string]struct {
		arm  func(*faultCoordinator)
		want string
	}{
		"AcquireJob": {func(c *faultCoordinator) {
			c.acquire = func(context.Context, string, string, time.Duration) (string, string, error) { return "", "", down }
		}, "acquire segment lock"},
		"WaitJob": {func(c *faultCoordinator) {
			c.waitJob = func(context.Context, string) (<-chan domain.JobStatus, error) { return nil, down }
		}, "wait job"},
		"WaitSegment": {func(c *faultCoordinator) {
			c.waitSegment = func(context.Context, domain.SegmentData) (<-chan domain.SegmentStatus, error) {
				return nil, down
			}
		}, "wait segment"},
	}
	for name, test := range cases {
		coordinator := newFaultCoordinator()
		test.arm(coordinator)
		controller := pathController(t, coordinator, raceStore(t), time.Minute, time.Hour, nil)
		outcome := awaitOutcome(t, pathRequest(controller, context.Background()), name)
		if !errors.Is(outcome.err, down) || !strings.Contains(outcome.err.Error(), test.want) {
			t.Fatalf("%s failing: got %v, want %q wrapping the coordinator error", name, outcome.err, test.want)
		}
		requireFlightGone(t, controller, coordinator, name+" failing")
		controller.Stop()
	}
}

type latchedContext struct {
	context.Context
	cancelled atomic.Bool
}

func (c *latchedContext) Done() <-chan struct{} { return nil }

func (c *latchedContext) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

func TestAbandonedJoinerWhoseClientLeftDoesNotReenter(t *testing.T) {
	coordinator := newFaultCoordinator()
	creatorWaits := make(chan struct{}, 1)
	coordinator.waitJob = func(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
		creatorWaits <- struct{}{}
		return coordinator.clusterCoordinator.WaitJob(ctx, runID)
	}
	joined := make(chan struct{}, 1)
	observer := domain.ObserverFunc(func(_ context.Context, event domain.Event) {
		if event.Type == domain.EventJobJoined {
			joined <- struct{}{}
		}
	})
	controller := pathController(t, coordinator, raceStore(t), time.Minute, time.Hour, observer)
	defer controller.Stop()
	remoteHoldsJob(t, controller, coordinator.clusterCoordinator, 0, 3)

	creatorCtx, creatorLeaves := context.WithCancel(context.Background())
	creator := pathRequest(controller, creatorCtx)
	<-creatorWaits
	joinerCtx := &latchedContext{Context: context.Background()}
	joiner := pathRequest(controller, joinerCtx)
	<-joined
	claims := coordinator.claims.Load()

	joinerCtx.cancelled.Store(true)
	creatorLeaves()
	awaitOutcome(t, creator, "creator leaving")
	outcome := awaitOutcome(t, joiner, "abandoned joiner")
	if outcome.err != context.Canceled {
		t.Fatalf("an abandoned joiner whose client left must return its own cancellation, got %v", outcome.err)
	}
	if got := coordinator.claims.Load(); got != claims {
		t.Fatalf("an abandoned joiner whose client left must not claim the batch again, claims went from %d to %d", claims, got)
	}
}

func lockHeldWaiter(t *testing.T, coordinator *faultCoordinator, store domain.Storage) (*Engine, chan segmentOutcome) {
	t.Helper()
	parked := make(chan struct{}, 1)
	coordinator.waitJob = func(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
		parked <- struct{}{}
		return coordinator.clusterCoordinator.WaitJob(ctx, runID)
	}
	controller := pathController(t, coordinator, store, time.Minute, time.Hour, nil)
	remoteHoldsJob(t, controller, coordinator.clusterCoordinator, 0, 3)
	result := pathRequest(controller, context.Background())
	<-parked
	return controller, result
}

func TestLockHeldWaiterReturnsBeforeStopFinishesWaitingForOtherWork(t *testing.T) {
	coordinator := newFaultCoordinator()
	controller, result := lockHeldWaiter(t, coordinator, raceStore(t))
	if !controller.lifecycle.Enter() {
		t.Fatal("lifecycle must accept work before Stop")
	}
	stopped := make(chan struct{})
	go func() {
		controller.Stop()
		close(stopped)
	}()
	select {
	case outcome := <-result:
		if !errors.Is(outcome.err, ErrControllerStopped) {
			t.Fatalf("got %v, want ErrControllerStopped", outcome.err)
		}
	case <-stopped:
		t.Fatal("Stop returned while other work was still active")
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter behind a remote lock must not wait for Stop to finish draining other work")
	}
	controller.lifecycle.Exit()
	<-stopped
}

type gatedStorage struct {
	*raceStorage
	armed   atomic.Bool
	entered chan struct{}
	proceed chan struct{}
}

func (s *gatedStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.proceed
	}
	return s.raceStorage.SegmentExists(ctx, info)
}

func TestWaiterSeeingStopAndFlightEndTogetherReportsStopped(t *testing.T) {
	for round := 0; round < 16; round++ {
		coordinator := newFaultCoordinator()
		store := &gatedStorage{raceStorage: raceStore(t), entered: make(chan struct{}), proceed: make(chan struct{})}
		controller, result := lockHeldWaiter(t, coordinator, store)
		store.armed.Store(true)
		<-store.entered
		controller.Stop()
		close(store.proceed)
		if outcome := awaitOutcome(t, result, "stop and flight end together"); !errors.Is(outcome.err, ErrControllerStopped) {
			t.Fatalf("round %d: got %v, want ErrControllerStopped", round, outcome.err)
		}
	}
}

func TestCancelledJobIsReportedAsCancelled(t *testing.T) {
	coordinator := newFaultCoordinator()
	controller := pathController(t, coordinator, raceStore(t), time.Minute, time.Hour, nil)
	defer controller.Stop()
	jobs, err := coordinator.clusterCoordinator.Subscribe(context.Background(), domain.StreamVideo, controller.pipelineVersion)
	if err != nil {
		t.Fatal(err)
	}
	result := pathRequest(controller, context.Background())
	delivery := <-jobs
	status := domain.JobStatus{JobID: delivery.Job.ID, RunID: delivery.Job.RunID, State: domain.JobStateCancelled, Error: "operator stop"}
	if err := coordinator.SettleJob(context.Background(), delivery.Receipt, status); err != nil {
		t.Fatal(err)
	}
	outcome := awaitOutcome(t, result, "cancelled job")
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "segment job cancelled: operator stop") {
		t.Fatalf("got %v, want the cancellation and its reason", outcome.err)
	}
}

func TestLockHeldWaiterIgnoresStatusOfAnotherRun(t *testing.T) {
	coordinator := newFaultCoordinator()
	store := raceStore(t)
	statuses := make(chan domain.JobStatus)
	parked := make(chan struct{}, 1)
	coordinator.waitJob = func(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
		parked <- struct{}{}
		return statuses, nil
	}
	controller := pathController(t, coordinator, store, time.Minute, time.Hour, nil)
	defer controller.Stop()
	remoteHoldsJob(t, controller, coordinator.clusterCoordinator, 0, 3)
	result := pathRequest(controller, context.Background())
	<-parked
	foreign := domain.JobStatus{RunID: "another-run", State: domain.JobStateFailed, Error: "not ours"}
	for i := 0; i < 2; i++ {
		select {
		case statuses <- foreign:
		case outcome := <-result:
			t.Fatalf("the waiter acted on another run's status: %v", outcome.err)
		}
	}
	deliverSegment(t, controller, coordinator.clusterCoordinator, store, "remote")
	if outcome := awaitOutcome(t, result, "foreign status"); outcome.err != nil || string(outcome.data) != "remote" {
		t.Fatalf("got %q, %v", outcome.data, outcome.err)
	}
}

func TestFlightIgnoresStatusOfAnotherRun(t *testing.T) {
	coordinator := newFaultCoordinator()
	store := raceStore(t)
	statuses := make(chan domain.JobStatus)
	coordinator.waitJob = func(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
		return statuses, nil
	}
	controller := pathController(t, coordinator, store, time.Minute, time.Hour, nil)
	defer controller.Stop()
	result := pathRequest(controller, context.Background())
	foreign := domain.JobStatus{RunID: "another-run", State: domain.JobStateFailed, Error: "not ours"}
	for i := 0; i < 2; i++ {
		select {
		case statuses <- foreign:
		case outcome := <-result:
			t.Fatalf("the flight ended on another run's status: %v", outcome.err)
		}
	}
	deliverSegment(t, controller, coordinator.clusterCoordinator, store, "ours")
	if outcome := awaitOutcome(t, result, "foreign status on the flight"); outcome.err != nil || string(outcome.data) != "ours" {
		t.Fatalf("got %q, %v", outcome.data, outcome.err)
	}
}

type lateStorage struct {
	*raceStorage
	visibleAt time.Time
}

func (s *lateStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	if time.Now().Before(s.visibleAt) {
		return false, nil
	}
	return s.raceStorage.SegmentExists(ctx, info)
}

func TestSegmentStoredJustBeforeTheTimeoutIsServed(t *testing.T) {
	coordinator := newFaultCoordinator()
	store := &lateStorage{raceStorage: raceStore(t)}
	store.setSegment(0, []byte("just in time"))
	controller := pathController(t, coordinator, store, 150*time.Millisecond, time.Hour, nil)
	defer controller.Stop()
	store.visibleAt = time.Now().Add(140 * time.Millisecond)
	outcome := awaitOutcome(t, pathRequest(controller, context.Background()), "segment stored before the timeout")
	if outcome.err != nil || string(outcome.data) != "just in time" {
		t.Fatalf("a segment in storage when the wait times out must be served, got %q, %v", outcome.data, outcome.err)
	}
}

func TestLockProbeFailureIsNotReadAsAFreeLock(t *testing.T) {
	coordinator := newFaultCoordinator()
	coordinator.probes = make(chan struct{}, 1)
	coordinator.acquire = func(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
		if ttl < time.Minute {
			return "", "", errors.New("coordinator unreachable")
		}
		return coordinator.clusterCoordinator.AcquireJob(ctx, jobID, runID, ttl)
	}
	store := raceStore(t)
	controller, result := lockHeldWaiter(t, coordinator, store)
	defer controller.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-coordinator.probes:
		case outcome := <-result:
			t.Fatalf("the waiter gave up after a failed lock probe: %v", outcome.err)
		}
	}
	deliverSegment(t, controller, coordinator.clusterCoordinator, store, "remote")
	if outcome := awaitOutcome(t, result, "failed probes"); outcome.err != nil {
		t.Fatalf("got %v", outcome.err)
	}
	if claims := coordinator.claims.Load(); claims != 1 {
		t.Fatalf("a failed lock probe must not make the waiter claim the batch again, claims=%d", claims)
	}
}

type releaseEvents struct {
	mu     sync.Mutex
	events []domain.Event
	seen   chan domain.Event
}

func (r *releaseEvents) Observe(_ context.Context, event domain.Event) {
	if event.Type != domain.EventLockReleaseFailed {
		return
	}
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	select {
	case r.seen <- event:
	default:
	}
}

func TestFailedLockReleasesAreReported(t *testing.T) {
	refused := errors.New("release refused")
	releaseThenFail := func(coordinator *faultCoordinator) {
		coordinator.release = func(ctx context.Context, jobID, runID, token string) error {
			if err := coordinator.clusterCoordinator.ReleaseJob(ctx, jobID, runID, token); err != nil {
				return err
			}
			return refused
		}
	}

	coordinator := newFaultCoordinator()
	coordinator.enqueueRejects = true
	releaseThenFail(coordinator)
	recorder := &releaseEvents{seen: make(chan domain.Event, 4)}
	controller := pathController(t, coordinator, raceStore(t), time.Minute, time.Hour, recorder)
	outcome := awaitOutcome(t, pathRequest(controller, context.Background()), "rejected enqueue")
	if !errors.Is(outcome.err, ErrEnqueueRejected) {
		t.Fatalf("got %v, want the enqueue rejection", outcome.err)
	}
	select {
	case event := <-recorder.seen:
		if event.MediaKey == "" || event.JobID == "" || !strings.Contains(event.Error, "release refused") {
			t.Fatalf("flight release failure event is incomplete: %+v", event)
		}
	default:
		t.Fatal("a failed release of a flight's lock must emit domain.EventLockReleaseFailed")
	}
	controller.Stop()

	coordinator = newFaultCoordinator()
	recorder = &releaseEvents{seen: make(chan domain.Event, 4)}
	store := raceStore(t)
	parked := make(chan struct{}, 1)
	coordinator.waitJob = func(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
		select {
		case parked <- struct{}{}:
		default:
		}
		return coordinator.clusterCoordinator.WaitJob(ctx, runID)
	}
	controller = pathController(t, coordinator, store, time.Minute, time.Hour, recorder)
	defer controller.Stop()
	remoteHoldsJob(t, controller, coordinator.clusterCoordinator, 0, 3)
	result := pathRequest(controller, context.Background())
	<-parked
	var once sync.Once
	coordinator.clusterCoordinator.mu.Lock()
	coordinator.release = func(ctx context.Context, jobID, runID, token string) error {
		err := coordinator.clusterCoordinator.ReleaseJob(ctx, jobID, runID, token)
		once.Do(func() { err = refused })
		return err
	}
	for key := range coordinator.clusterCoordinator.locks {
		delete(coordinator.clusterCoordinator.locks, key)
	}
	for jobID := range coordinator.clusterCoordinator.activeJobs {
		delete(coordinator.clusterCoordinator.activeJobs, jobID)
	}
	coordinator.clusterCoordinator.mu.Unlock()
	select {
	case event := <-recorder.seen:
		if event.JobID == "" || !strings.Contains(event.Error, "release refused") {
			t.Fatalf("probe release failure event is incomplete: %+v", event)
		}
	case outcome := <-result:
		t.Fatalf("the request ended with %v before the probe's failed release was reported", outcome.err)
	case <-time.After(5 * time.Second):
		t.Fatal("a failed release of a lock probe must emit domain.EventLockReleaseFailed")
	}
	deliverSegment(t, controller, coordinator.clusterCoordinator, store, "taken over")
	if outcome := awaitOutcome(t, result, "takeover after the probe"); outcome.err != nil {
		t.Fatalf("got %v", outcome.err)
	}
}

func TestJoinFlightReleasesAndCancelsExpiredFlights(t *testing.T) {
	for _, joinExisting := range []bool{true, false} {
		coordinator := newFaultCoordinator()
		controller := pathController(t, coordinator, raceStore(t), time.Minute, time.Hour, nil)
		token, _, err := coordinator.clusterCoordinator.AcquireJob(context.Background(), "expired-job", "expired-run", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		cancelled := false
		expired := &flight{jobID: "expired-job", runID: "expired-run", mediaKey: "other-media", streamType: domain.StreamVideo, rendition: "1080p", start: 8, end: 11, deadline: time.Now().Add(-time.Second), token: token, watchCancel: func() { cancelled = true }, done: make(chan struct{})}
		live := &flight{jobID: "live-job", runID: "live-run", mediaKey: "media", streamType: domain.StreamVideo, rendition: "1080p", start: 0, end: 3, deadline: time.Now().Add(time.Hour), done: make(chan struct{})}
		controller.flights.mu.Lock()
		controller.flights.flights[expired.jobID] = expired
		if joinExisting {
			controller.flights.flights[live.jobID] = live
		}
		controller.flights.mu.Unlock()

		existing, created := controller.joinFlight("media", domain.StreamVideo, "1080p", 0, 3)
		what := fmt.Sprintf("joinExisting=%t", joinExisting)
		if joinExisting && existing != live || !joinExisting && created == nil {
			t.Fatalf("%s: got existing %v, created %v", what, existing, created)
		}
		select {
		case <-expired.done:
		default:
			t.Fatalf("%s: the expired flight was not closed", what)
		}
		if _, expiredFlag := controller.flightEnded(expired); !expiredFlag {
			t.Fatalf("%s: the expired flight is not marked expired", what)
		}
		if !cancelled {
			t.Fatalf("%s: the expired flight's job watch was not cancelled", what)
		}
		if locks := coordinator.heldLocks(); locks != 0 {
			t.Fatalf("%s: the expired flight's lock was not released, held=%d", what, locks)
		}
		controller.Stop()
	}
}

func TestFlightPastItsDeadlineTimesOutItsWaitersAndFreesTheLock(t *testing.T) {
	coordinator := newFaultCoordinator()
	released := make(chan struct{}, 1)
	coordinator.release = func(ctx context.Context, jobID, runID, token string) error {
		err := coordinator.clusterCoordinator.ReleaseJob(ctx, jobID, runID, token)
		released <- struct{}{}
		return err
	}
	controller := pathController(t, coordinator, raceStore(t), time.Minute, 100*time.Millisecond, nil)
	defer controller.Stop()
	outcome := awaitOutcome(t, pathRequest(controller, context.Background()), "flight deadline")
	if !errors.Is(outcome.err, ErrSegmentTimeout) {
		t.Fatalf("got %v, want ErrSegmentTimeout at the flight deadline", outcome.err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock of a flight past its deadline was not released")
	}
	if flights := registeredFlights(controller); flights != 0 {
		t.Fatalf("%d flights stay registered past the deadline", flights)
	}
}

func TestJobWatchIsCancelledWhenItsFlightExpiredDuringSetup(t *testing.T) {
	coordinator := newFaultCoordinator()
	jobTimeout := 20 * time.Millisecond
	var controller *Engine
	var watch context.Context
	coordinator.waitJob = func(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
		watch = ctx
		time.Sleep(2*jobTimeout + 20*time.Millisecond)
		info := raceSegmentInfo(t, controller, 0)
		controller.joinFlight(info.MediaKey, domain.StreamVideo, "1080p", 0, 3)
		return coordinator.clusterCoordinator.WaitJob(ctx, runID)
	}
	controller = pathController(t, coordinator, raceStore(t), time.Minute, jobTimeout, nil)
	defer controller.Stop()
	outcome := awaitOutcome(t, pathRequest(controller, context.Background()), "flight expired during setup")
	if !errors.Is(outcome.err, ErrSegmentTimeout) {
		t.Fatalf("got %v, want ErrSegmentTimeout", outcome.err)
	}
	if watch == nil || watch.Err() == nil {
		t.Fatal("the job watch of a flight that expired during setup must be cancelled")
	}
	if got := coordinator.enqueues(); got != 0 {
		t.Fatalf("a flight that expired during setup must not enqueue, got %d", got)
	}
}

func (c *faultCoordinator) enqueues() int {
	c.clusterCoordinator.mu.Lock()
	defer c.clusterCoordinator.mu.Unlock()
	return len(c.clusterCoordinator.deliveries)
}

func TestStartAfterStopDoesNotSubscribe(t *testing.T) {
	coordinator := newFaultCoordinator()
	controller := pathController(t, coordinator, raceStore(t), time.Minute, time.Hour, nil)
	controller.Stop()
	if err := controller.Start(context.Background()); !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("got %v, want ErrControllerStopped", err)
	}
	if got := coordinator.subscribes.Load(); got != 0 {
		t.Fatalf("Start on a stopped controller must not subscribe, got %d subscriptions", got)
	}
}

type lifecycleProbeCoordinator struct {
	*clusterCoordinator
	enqueues     atomic.Int32
	releaseGate  chan struct{}
	releaseEnter chan struct{}
	onAcquireJob func()
	waitJobDelay time.Duration
	onWaitJob    func()
	mu           sync.Mutex
	releaseDone  int
	waitJobErrs  []error
}

func newLifecycleProbeCoordinator() *lifecycleProbeCoordinator {
	return &lifecycleProbeCoordinator{clusterCoordinator: newClusterCoordinator()}
}

func (c *lifecycleProbeCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.enqueues.Add(1)
	return c.clusterCoordinator.Enqueue(ctx, job)
}

func (c *lifecycleProbeCoordinator) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	token, active, err := c.clusterCoordinator.AcquireJob(ctx, jobID, runID, ttl)
	if c.onAcquireJob != nil {
		c.onAcquireJob()
	}
	return token, active, err
}

func (c *lifecycleProbeCoordinator) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	if c.releaseEnter != nil {
		select {
		case c.releaseEnter <- struct{}{}:
		default:
		}
	}
	if c.releaseGate != nil {
		<-c.releaseGate
	}
	err := c.clusterCoordinator.ReleaseJob(ctx, jobID, runID, token)
	c.mu.Lock()
	c.releaseDone++
	c.mu.Unlock()
	return err
}

func (c *lifecycleProbeCoordinator) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	if c.onWaitJob != nil {
		c.onWaitJob()
	}
	time.Sleep(c.waitJobDelay)
	c.mu.Lock()
	c.waitJobErrs = append(c.waitJobErrs, ctx.Err())
	c.mu.Unlock()
	return c.clusterCoordinator.WaitJob(ctx, runID)
}

func (c *lifecycleProbeCoordinator) releasesCompleted() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releaseDone
}

func (c *lifecycleProbeCoordinator) holdsJob(jobID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, held := c.locks["job:"+jobID]
	return held
}

func parkFlight(t *testing.T, controller *Engine, coordinator *lifecycleProbeCoordinator, rendition string, enqueued, watched bool) *flight {
	t.Helper()
	_, created := controller.joinFlight("parked", domain.StreamVideo, rendition, 0, 0)
	if created == nil {
		t.Fatal("expected a new flight")
	}
	token, _, err := coordinator.AcquireJob(context.Background(), created.jobID, created.runID, time.Hour)
	if err != nil {
		t.Fatalf("acquire job: %v", err)
	}
	if !controller.setFlightLock(created, token) {
		t.Fatal("flight lock not recorded")
	}
	if enqueued {
		controller.markEnqueued(created)
	}
	if !watched {
		return created
	}
	watchCtx, watchCancel := context.WithCancel(controller.lifecycle.Context())
	statuses, err := coordinator.clusterCoordinator.WaitJob(watchCtx, created.runID)
	if err != nil {
		t.Fatalf("wait job: %v", err)
	}
	if !controller.setFlightWatch(created, watchCancel) {
		t.Fatal("flight watch not recorded")
	}
	if !controller.startFlightWatch(created, statuses) {
		t.Fatal("flight watch not started")
	}
	return created
}

func flightIsDone(f *flight) bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

func TestStopWaitsForFlightWatchersAndReleasesOnlyNeverEnqueuedLocks(t *testing.T) {
	coordinator := newLifecycleProbeCoordinator()
	coordinator.releaseGate = make(chan struct{})
	coordinator.releaseEnter = make(chan struct{}, 1)
	controller := lifecycleController(t, coordinator, time.Hour)

	if _, err := controller.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0); err == nil {
		t.Fatal("expected a timeout while no worker is running")
	}
	controller.flights.mu.Lock()
	var enqueuedFlight *flight
	for _, f := range controller.flights.flights {
		enqueuedFlight = f
	}
	controller.flights.mu.Unlock()
	if enqueuedFlight == nil {
		t.Fatal("expected one live flight after the timed-out request")
	}
	if coordinator.enqueues.Load() != 1 {
		t.Fatalf("expected the request to enqueue once, got %d", coordinator.enqueues.Load())
	}
	idle := parkFlight(t, controller, coordinator, "720p", false, true)
	if controller.lifecycle.Wait(10 * time.Millisecond) {
		t.Fatal("flight watchers must be tracked by the lifecycle")
	}
	if !coordinator.holdsJob(enqueuedFlight.jobID) || !coordinator.holdsJob(idle.jobID) {
		t.Fatal("expected both flights to hold their job locks")
	}

	controller.lifecycle.Close()
	select {
	case <-coordinator.releaseEnter:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher of the never-enqueued flight did not start releasing its lock")
	}
	stopped := make(chan struct{})
	go func() {
		controller.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop must wait for the watcher that is still releasing a lock")
	case <-time.After(300 * time.Millisecond):
	}
	close(coordinator.releaseGate)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return once the watchers finished")
	}

	if !controller.lifecycle.Wait(10 * time.Millisecond) {
		t.Fatal("flight watchers survived Stop")
	}
	if !flightIsDone(enqueuedFlight) || !flightIsDone(idle) {
		t.Fatal("Stop must finish every flight")
	}
	if got := coordinator.releasesCompleted(); got != 1 {
		t.Fatalf("Stop must wait for the in-progress release of the never-enqueued lock, completed releases: %d", got)
	}
	if coordinator.holdsJob(idle.jobID) {
		t.Fatal("the lock of a flight that was never enqueued must be released")
	}
	if !coordinator.holdsJob(enqueuedFlight.jobID) {
		t.Fatal("the lock of an enqueued job must be left to expire by TTL")
	}
	if coordinator.releaseCount() != 1 {
		t.Fatalf("expected exactly one job release, got %d", coordinator.releaseCount())
	}
}

func TestStopFinishesFlightsWithoutWatchers(t *testing.T) {
	coordinator := newLifecycleProbeCoordinator()
	controller := lifecycleController(t, coordinator, time.Hour)
	idle := parkFlight(t, controller, coordinator, "720p", false, false)
	queued := parkFlight(t, controller, coordinator, "480p", true, false)

	controller.Stop()

	if !flightIsDone(idle) || !flightIsDone(queued) {
		t.Fatal("Stop must finish flights even when no watcher is running")
	}
	if coordinator.holdsJob(idle.jobID) {
		t.Fatal("the lock of a never-enqueued flight must be released")
	}
	if !coordinator.holdsJob(queued.jobID) {
		t.Fatal("the lock of an enqueued flight must be left to expire by TTL")
	}
	controller.flights.mu.Lock()
	live := len(controller.flights.flights)
	controller.flights.mu.Unlock()
	if live != 0 {
		t.Fatalf("%d flights survived Stop", live)
	}
}

func TestSegmentNeverEnqueuesWhenWatcherCannotStart(t *testing.T) {
	coordinator := newLifecycleProbeCoordinator()
	controller := lifecycleController(t, coordinator, time.Hour)
	coordinator.onWaitJob = func() {
		if coordinator.enqueues.Load() == 0 {
			controller.lifecycle.Close()
		}
	}

	_, err := controller.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)

	if !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("got %v, want ErrControllerStopped", err)
	}
	if got := coordinator.enqueues.Load(); got != 0 {
		t.Fatalf("a stopped controller must not enqueue, got %d enqueues", got)
	}
	if coordinator.releaseCount() != 1 {
		t.Fatalf("the never-enqueued job lock must be released, got %d releases", coordinator.releaseCount())
	}
	controller.flights.mu.Lock()
	live := len(controller.flights.flights)
	controller.flights.mu.Unlock()
	if live != 0 {
		t.Fatalf("%d flights left behind", live)
	}
}

func TestSegmentRacingStopReturnsStoppedAndCancelsJobWatchWithLifecycle(t *testing.T) {
	coordinator := newLifecycleProbeCoordinator()
	coordinator.waitJobDelay = 200 * time.Millisecond
	entered := make(chan struct{})
	var once sync.Once
	coordinator.onWaitJob = func() { once.Do(func() { close(entered) }) }
	controller := lifecycleController(t, coordinator, time.Hour)

	result := make(chan error, 1)
	go func() {
		_, err := controller.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		result <- err
	}()
	<-entered
	controller.Stop()

	select {
	case err := <-result:
		if !errors.Is(err, ErrControllerStopped) {
			t.Fatalf("got %v, want ErrControllerStopped when Stop removed the flight", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Segment did not return after Stop")
	}
	if got := coordinator.enqueues.Load(); got != 0 {
		t.Fatalf("a request racing Stop must not enqueue, got %d", got)
	}
	coordinator.mu.Lock()
	errs := append([]error(nil), coordinator.waitJobErrs...)
	coordinator.mu.Unlock()
	if len(errs) != 1 || errs[0] == nil {
		t.Fatalf("the job watch context must be derived from the lifecycle and cancelled by Stop, got %v", errs)
	}
}

func TestStopReturnsErrControllerStoppedToWaitingSegment(t *testing.T) {
	coordinator := newLifecycleProbeCoordinator()
	controller := lifecycleControllerWithSegmentTimeout(t, coordinator, time.Hour, time.Minute)

	result := make(chan error, 1)
	go func() {
		_, err := controller.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		result <- err
	}()
	waitUntil(t, 5*time.Second, func() bool { return coordinator.enqueues.Load() == 1 })
	controller.Stop()

	select {
	case err := <-result:
		if !errors.Is(err, ErrControllerStopped) {
			t.Fatalf("got %v, want ErrControllerStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting segment did not return after Stop")
	}
}

func TestJoinFlightFlagsExpiredFlights(t *testing.T) {
	controller := lifecycleController(t, newLifecycleProbeCoordinator(), time.Hour)
	_, stale := controller.joinFlight("media", domain.StreamVideo, "1080p", 0, 3)
	if stale == nil {
		t.Fatal("expected a new flight")
	}
	controller.flights.mu.Lock()
	stale.deadline = time.Now().Add(-time.Second)
	controller.flights.mu.Unlock()

	_, fresh := controller.joinFlight("media", domain.StreamVideo, "1080p", 0, 3)

	if fresh == nil || fresh == stale {
		t.Fatal("an expired flight must be replaced by a new one")
	}
	if !flightIsDone(stale) {
		t.Fatal("the expired flight must be finished")
	}
	if abandoned, expired := controller.flightEnded(stale); abandoned || !expired {
		t.Fatalf("expired flight flags: abandoned=%v expired=%v, want expired only", abandoned, expired)
	}
}

func TestSegmentReturnsStoppedWhenStopRemovesFlightBeforeLockIsRecorded(t *testing.T) {
	coordinator := newLifecycleProbeCoordinator()
	controller := lifecycleController(t, coordinator, time.Hour)
	coordinator.onAcquireJob = controller.Stop

	_, err := controller.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)

	if !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("got %v, want ErrControllerStopped", err)
	}
	if got := coordinator.enqueues.Load(); got != 0 {
		t.Fatalf("a stopped controller must not enqueue, got %d enqueues", got)
	}
	if coordinator.releaseCount() != 1 {
		t.Fatalf("the acquired job lock must be released, got %d releases", coordinator.releaseCount())
	}
}

var flightEndings = []struct {
	name string
	end  func(*Engine, *flight)
}{
	{"finish", (*Engine).finishFlight},
	{"stop", (*Engine).stopFlight},
	{"abandon", (*Engine).abandonFlight},
	{"expire", (*Engine).expireFlight},
}

func TestEndingFlightAgainDoesNotReleaseItsJobLockTwice(t *testing.T) {
	for _, first := range flightEndings {
		for _, second := range flightEndings {
			t.Run(first.name+" then "+second.name, func(t *testing.T) {
				coordinator := newLifecycleProbeCoordinator()
				var releaseFailures atomic.Int32
				controller := lifecycleController(t, coordinator, time.Hour)
				controller.opts.Observer = domain.ObserverFunc(func(_ context.Context, event domain.Event) {
					if event.Type == domain.EventLockReleaseFailed {
						releaseFailures.Add(1)
					}
				})
				t.Cleanup(controller.Stop)
				f := parkFlight(t, controller, coordinator, "720p", false, false)

				first.end(controller, f)
				if got := coordinator.releasesCompleted(); got != 1 {
					t.Fatalf("ending a never-enqueued flight must release its job lock once, got %d releases", got)
				}
				second.end(controller, f)

				if got := coordinator.releasesCompleted(); got != 1 {
					t.Fatalf("a flight that already ended must not release its job lock again, got %d release calls", got)
				}
				if got := releaseFailures.Load(); got != 0 {
					t.Fatalf("ending a flight twice reported %d lock release failures", got)
				}
				if coordinator.holdsJob(f.jobID) {
					t.Fatal("the job lock must be released")
				}
			})
		}
	}
}

func TestEndingFlightCancelsItsJobWatch(t *testing.T) {
	for _, ending := range flightEndings {
		t.Run(ending.name, func(t *testing.T) {
			coordinator := newLifecycleProbeCoordinator()
			controller := lifecycleController(t, coordinator, time.Hour)
			t.Cleanup(controller.Stop)
			_, f := controller.joinFlight("watched", domain.StreamVideo, "720p", 0, 0)
			if f == nil {
				t.Fatal("expected a new flight")
			}
			watchCtx, watchCancel := context.WithCancel(controller.lifecycle.Context())
			defer watchCancel()
			if !controller.setFlightWatch(f, watchCancel) {
				t.Fatal("flight watch not recorded")
			}

			ending.end(controller, f)

			if watchCtx.Err() == nil {
				t.Fatal("ending a flight must cancel its job watch instead of leaving it open until Stop")
			}
			if controller.lifecycle.Stopped() {
				t.Fatal("the controller must still be running")
			}
		})
	}
}

func (c *Engine) finishFlightRun(jobID, runID string) {
	c.flights.mu.Lock()
	f := c.flights.flights[jobID]
	c.flights.mu.Unlock()
	if f != nil && f.runID == runID {
		c.finishFlight(f)
	}
}
