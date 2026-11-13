package goshl

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

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

func parkFlight(t *testing.T, controller *Controller, coordinator *lifecycleProbeCoordinator, rendition string, enqueued, watched bool) *flight {
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
	end  func(*Controller, *flight)
}{
	{"finish", (*Controller).finishFlight},
	{"stop", (*Controller).stopFlight},
	{"abandon", (*Controller).abandonFlight},
	{"expire", (*Controller).expireFlight},
}

func TestEndingFlightAgainDoesNotReleaseItsJobLockTwice(t *testing.T) {
	for _, first := range flightEndings {
		for _, second := range flightEndings {
			t.Run(first.name+" then "+second.name, func(t *testing.T) {
				coordinator := newLifecycleProbeCoordinator()
				var releaseFailures atomic.Int32
				controller := lifecycleController(t, coordinator, time.Hour)
				controller.opts.Observer = ObserverFunc(func(_ context.Context, event Event) {
					if event.Type == EventLockReleaseFailed {
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

func (c *Controller) finishFlightRun(jobID, runID string) {
	c.flights.mu.Lock()
	f := c.flights.flights[jobID]
	c.flights.mu.Unlock()
	if f != nil && f.runID == runID {
		c.finishFlight(f)
	}
}
