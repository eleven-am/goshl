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

type raceStorage struct {
	*stubStorage
	metaMu sync.Mutex
}

func (s *raceStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.stubStorage.metaExists, nil
}

func (s *raceStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.stubStorage.metaData, nil
}

type raceCoordinator struct {
	*clusterCoordinator
	acquireDelay   time.Duration
	acquireGranted chan struct{}
	acquireProceed chan struct{}
	acquireHeld    atomic.Bool
	enqueueEntered chan struct{}
	enqueueProceed chan struct{}
	enqueueHeld    atomic.Bool
	enqueues       atomic.Int32
	enqueued       chan struct{}
	unlocked       atomic.Int32
	waitJobSeen    chan string
	waitJobGate    chan struct{}
	onWaitJob      func()
	lockProbed     chan struct{}
	released       chan struct{}
	onSubscribe    func(call int)
	subMu          sync.Mutex
	subContexts    []context.Context
}

func (c *raceCoordinator) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	if c.acquireDelay > 0 {
		time.Sleep(c.acquireDelay)
	}
	token, active, err := c.clusterCoordinator.AcquireJob(ctx, jobID, runID, ttl)
	if c.lockProbed != nil && ttl < time.Minute {
		select {
		case c.lockProbed <- struct{}{}:
		default:
		}
	}
	if c.acquireGranted != nil && err == nil && c.acquireHeld.CompareAndSwap(false, true) {
		close(c.acquireGranted)
		<-c.acquireProceed
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", ctxErr
		}
	}
	return token, active, err
}

func (c *raceCoordinator) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	err := c.clusterCoordinator.ReleaseJob(ctx, jobID, runID, token)
	if c.released != nil {
		select {
		case c.released <- struct{}{}:
		default:
		}
	}
	return err
}

func (c *raceCoordinator) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	if c.onWaitJob != nil {
		c.onWaitJob()
	}
	if c.waitJobSeen != nil {
		c.waitJobSeen <- runID
	}
	if c.waitJobGate != nil {
		<-c.waitJobGate
	}
	return c.clusterCoordinator.WaitJob(ctx, runID)
}

func (c *raceCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.enqueues.Add(1)
	c.clusterCoordinator.mu.Lock()
	_, held := c.clusterCoordinator.locks["job:"+job.ID]
	c.clusterCoordinator.mu.Unlock()
	if !held {
		c.unlocked.Add(1)
	}
	if c.enqueueEntered != nil && c.enqueueHeld.CompareAndSwap(false, true) {
		close(c.enqueueEntered)
		<-c.enqueueProceed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.clusterCoordinator.Enqueue(ctx, job)
	if err == nil && c.enqueued != nil {
		select {
		case c.enqueued <- struct{}{}:
		default:
		}
	}
	return err
}

func (c *raceCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	c.subMu.Lock()
	c.subContexts = append(c.subContexts, ctx)
	call := len(c.subContexts)
	c.subMu.Unlock()
	if c.onSubscribe != nil {
		c.onSubscribe(call)
	}
	return c.clusterCoordinator.Subscribe(ctx, streamType, profile)
}

func (c *raceCoordinator) queuedJobs() int {
	c.clusterCoordinator.mu.Lock()
	defer c.clusterCoordinator.mu.Unlock()
	queued := 0
	for _, queue := range c.clusterCoordinator.queues {
		queued += len(queue)
	}
	return queued
}

func (c *raceCoordinator) heldLocks() int {
	c.clusterCoordinator.mu.Lock()
	defer c.clusterCoordinator.mu.Unlock()
	return len(c.clusterCoordinator.locks)
}

func raceStore(t *testing.T) *raceStorage {
	t.Helper()
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	return &raceStorage{stubStorage: &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}}
}

func raceController(t *testing.T, coordinator Coordinator, store Storage, segmentTimeout time.Duration) *Controller {
	t.Helper()
	return NewController(Options{
		Storage:        store,
		Coordinator:    coordinator,
		PathGen:        stubPathGen{},
		SegmentTimeout: segmentTimeout,
		JobTimeout:     time.Hour,
	})
}

func raceSegmentInfo(t *testing.T, controller *Controller, index int) domain.SegmentData {
	t.Helper()
	mediaKey, err := controller.resolveMediaKey(context.Background(), "file:///media")
	if err != nil {
		t.Fatal(err)
	}
	return domain.SegmentData{MediaKey: mediaKey, Index: index, Rendition: "1080p", IsVideo: true}
}

func remoteHoldsJob(t *testing.T, controller *Controller, coordinator *clusterCoordinator, start, end int) {
	t.Helper()
	info := raceSegmentInfo(t, controller, start)
	jobID := deterministicJobID(info.MediaKey, "1080p", domain.StreamVideo, start, end)
	if _, _, err := coordinator.AcquireJob(context.Background(), jobID, "remote-run", time.Hour); err != nil {
		t.Fatal(err)
	}
}

func requestSegment(controller *Controller, ctx context.Context) chan error {
	result := make(chan error, 1)
	go func() {
		_, err := controller.Segment(ctx, "file:///media", domain.StreamVideo, "1080p", 0)
		result <- err
	}()
	return result
}

func TestClientLeavingDuringEnqueueStillQueuesTheSharedJob(t *testing.T) {
	coordinator := &raceCoordinator{clusterCoordinator: newClusterCoordinator(), enqueueEntered: make(chan struct{}), enqueueProceed: make(chan struct{})}
	controller := raceController(t, coordinator, raceStore(t), time.Minute)
	defer controller.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	first := requestSegment(controller, ctx)
	<-coordinator.enqueueEntered
	cancel()
	close(coordinator.enqueueProceed)

	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the departed client must get its own cancellation, got %v", err)
	}
	if queued := coordinator.queuedJobs(); queued != 1 {
		t.Fatalf("the job shared by the flight must be queued although its first requester left during Enqueue, queued=%d", queued)
	}
	if locks := coordinator.heldLocks(); locks != 1 {
		t.Fatalf("the queued job must keep its lock, held=%d", locks)
	}
}

func TestClientLeavingDuringAcquireJobDoesNotOrphanTheLock(t *testing.T) {
	coordinator := &raceCoordinator{clusterCoordinator: newClusterCoordinator(), acquireGranted: make(chan struct{}), acquireProceed: make(chan struct{})}
	controller := raceController(t, coordinator, raceStore(t), time.Minute)
	defer controller.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	first := requestSegment(controller, ctx)
	<-coordinator.acquireGranted
	cancel()
	close(coordinator.acquireProceed)

	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the departed client must get its own cancellation, got %v", err)
	}
	if locks := coordinator.heldLocks(); locks != 0 {
		t.Fatalf("a lock granted to a request that left before enqueue must be released, held=%d", locks)
	}
	if got := coordinator.enqueues.Load(); got != 0 {
		t.Fatalf("a request that left before enqueue must not enqueue, got %d", got)
	}
}

func TestRemoteWaiterTakesOverWhenLockHolderAbandonsBeforeEnqueue(t *testing.T) {
	cluster := newClusterCoordinator()
	store := raceStore(t)
	coordinatorA := &raceCoordinator{clusterCoordinator: cluster, waitJobGate: make(chan struct{}), waitJobSeen: make(chan string, 1)}
	coordinatorB := &raceCoordinator{clusterCoordinator: cluster, waitJobSeen: make(chan string, 4), enqueued: make(chan struct{}, 1)}
	replicaA := raceController(t, coordinatorA, store, 3*time.Second)
	replicaB := raceController(t, coordinatorB, store, 3*time.Second)
	defer replicaA.Stop()
	defer replicaB.Stop()

	ctxA, cancelA := context.WithCancel(context.Background())
	resultA := requestSegment(replicaA, ctxA)
	runA := <-coordinatorA.waitJobSeen
	resultB := requestSegment(replicaB, context.Background())
	if watched := <-coordinatorB.waitJobSeen; watched != runA {
		t.Fatalf("replica B must wait on replica A's run %s, got %s", runA, watched)
	}

	cancelA()
	close(coordinatorA.waitJobGate)
	if err := <-resultA; !errors.Is(err, context.Canceled) {
		t.Fatalf("replica A request: %v", err)
	}

	select {
	case <-coordinatorB.enqueued:
	case err := <-resultB:
		t.Fatalf("replica B gave up with %v instead of taking over the job replica A abandoned", err)
	}
	info := raceSegmentInfo(t, replicaB, 0)
	if err := store.WriteSegment(context.Background(), info, []byte("produced")); err != nil {
		t.Fatal(err)
	}
	if err := cluster.NotifySegment(context.Background(), info, domain.SegmentStatus{State: domain.SegmentStateReady}); err != nil {
		t.Fatal(err)
	}
	if err := <-resultB; err != nil {
		t.Fatalf("replica B request: %v", err)
	}
}

func TestConcurrentRequestsBehindRemoteLockAllWaitForTheRemoteJob(t *testing.T) {
	const requests = 16
	cluster := newClusterCoordinator()
	coordinator := &raceCoordinator{clusterCoordinator: cluster, acquireDelay: time.Millisecond, lockProbed: make(chan struct{}, 1)}
	store := raceStore(t)
	controller := raceController(t, coordinator, store, time.Minute)
	defer controller.Stop()
	remoteHoldsJob(t, controller, cluster, 0, 3)

	results := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func() {
			data, err := controller.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
			if err == nil && string(data) != "remote" {
				err = errors.New("wrong segment data")
			}
			results <- err
		}()
	}
	select {
	case <-coordinator.lockProbed:
	case err := <-results:
		t.Fatalf("a request returned %v while the remote job was still running", err)
	}
	info := raceSegmentInfo(t, controller, 0)
	if err := store.WriteSegment(context.Background(), info, []byte("remote")); err != nil {
		t.Fatal(err)
	}
	if err := cluster.NotifySegment(context.Background(), info, domain.SegmentStatus{State: domain.SegmentStateReady}); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for i := 0; i < requests; i++ {
		if err := <-results; err != nil {
			failed++
			t.Logf("request failed: %v", err)
		}
	}
	if failed != 0 {
		t.Fatalf("%d of %d requests failed although the remote job delivered the segment", failed, requests)
	}
	if got := coordinator.enqueues.Load(); got != 0 {
		t.Fatalf("no local job may be enqueued while the remote lock is held, got %d", got)
	}
}

func TestLockHeldWaiterReturnsStoppedOnStop(t *testing.T) {
	cluster := newClusterCoordinator()
	coordinator := &raceCoordinator{clusterCoordinator: cluster, waitJobSeen: make(chan string, 1)}
	controller := raceController(t, coordinator, raceStore(t), time.Minute)
	remoteHoldsJob(t, controller, cluster, 0, 3)

	result := requestSegment(controller, context.Background())
	<-coordinator.waitJobSeen
	controller.Stop()
	select {
	case err := <-result:
		if !errors.Is(err, ErrControllerStopped) {
			t.Fatalf("got %v, want ErrControllerStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request waiting on a remote job lock kept waiting after Stop returned")
	}
}

type stopOnErrContext struct {
	context.Context
	armed atomic.Bool
	fire  func()
}

func (c *stopOnErrContext) Done() <-chan struct{} { return nil }

func (c *stopOnErrContext) Err() error {
	if c.armed.CompareAndSwap(true, false) {
		c.fire()
	}
	return nil
}

func TestStopAfterWatcherStartNeverEnqueuesAnUnlockedJob(t *testing.T) {
	coordinator := &raceCoordinator{clusterCoordinator: newClusterCoordinator(), released: make(chan struct{}, 1)}
	controller := raceController(t, coordinator, raceStore(t), time.Minute)
	ctx := &stopOnErrContext{Context: context.Background()}
	ctx.fire = func() {
		controller.lifecycle.Close()
		<-coordinator.released
	}
	coordinator.onWaitJob = func() { ctx.armed.Store(true) }

	_, err := controller.Segment(ctx, "file:///media", domain.StreamVideo, "1080p", 0)
	controller.Stop()

	if !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("got %v, want ErrControllerStopped", err)
	}
	if got := coordinator.enqueues.Load(); got != 0 {
		t.Fatalf("the job was enqueued after Stop released its lock: enqueues=%d without lock=%d", got, coordinator.unlocked.Load())
	}
}

func TestStartRacingStopLeavesNoPoolRunning(t *testing.T) {
	coordinator := &raceCoordinator{clusterCoordinator: newClusterCoordinator()}
	controller := raceController(t, coordinator, raceStore(t), time.Minute)
	coordinator.onSubscribe = func(call int) {
		if call == 2 {
			controller.lifecycle.Close()
		}
	}

	err := controller.Start(context.Background())
	defer controller.Stop()

	if !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("Start overtaken by Stop returned %v, want ErrControllerStopped", err)
	}
	coordinator.subMu.Lock()
	defer coordinator.subMu.Unlock()
	if len(coordinator.subContexts) != 2 {
		t.Fatalf("expected both pools to subscribe, got %d", len(coordinator.subContexts))
	}
	for index, subscription := range coordinator.subContexts {
		if subscription.Err() == nil {
			t.Fatalf("pool subscription %d is still live after Start lost the race with Stop", index)
		}
	}
}
