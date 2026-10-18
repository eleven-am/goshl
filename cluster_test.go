package goshl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/google/uuid"
)

type clusterRoute struct {
	stream  domain.StreamType
	profile string
}

type clusterLock struct {
	token   string
	expires time.Time
}

type clusterDelivery struct {
	job     domain.Job
	receipt string
	expires time.Time
}

type clusterCoordinator struct {
	mu             sync.Mutex
	queues         map[clusterRoute]chan domain.JobDelivery
	jobWaiters     map[string]map[chan domain.JobStatus]struct{}
	jobStatuses    map[string]domain.JobStatus
	segmentWaiters map[string]map[chan domain.SegmentStatus]struct{}
	locks          map[string]clusterLock
	activeJobs     map[string]string
	deliveries     map[string]clusterDelivery
	runReceipts    map[string]string
	settled        map[string]domain.JobStatus
	releases       []string
	acks           []string
	onEnqueue      func(domain.Job)
	enqueueErr     error
	onRenew        func(context.Context, domain.Job) error
	onSettle       func(context.Context, string, domain.JobStatus) error
}

func newClusterCoordinator() *clusterCoordinator {
	return &clusterCoordinator{
		queues:         make(map[clusterRoute]chan domain.JobDelivery),
		jobWaiters:     make(map[string]map[chan domain.JobStatus]struct{}),
		jobStatuses:    make(map[string]domain.JobStatus),
		segmentWaiters: make(map[string]map[chan domain.SegmentStatus]struct{}),
		locks:          make(map[string]clusterLock),
		activeJobs:     make(map[string]string),
		deliveries:     make(map[string]clusterDelivery),
		runReceipts:    make(map[string]string),
		settled:        make(map[string]domain.JobStatus),
	}
}

func (c *clusterCoordinator) queue(route clusterRoute) chan domain.JobDelivery {
	queue := c.queues[route]
	if queue == nil {
		queue = make(chan domain.JobDelivery, 8)
		c.queues[route] = queue
	}
	return queue
}

func (c *clusterCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	if c.onEnqueue != nil {
		c.onEnqueue(job)
	}
	receipt := uuid.NewString()
	c.mu.Lock()
	queue := c.queue(clusterRoute{stream: job.StreamType, profile: job.Profile})
	delivery := clusterDelivery{job: job, receipt: receipt, expires: time.Now().Add(5 * time.Second)}
	c.deliveries[receipt] = delivery
	c.runReceipts[job.RunID] = receipt
	c.mu.Unlock()
	select {
	case queue <- domain.JobDelivery{Job: job, Receipt: receipt}:
		return c.enqueueErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *clusterCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queue(clusterRoute{stream: streamType, profile: profile}), nil
}

func (c *clusterCoordinator) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	c.mu.Lock()
	delivery, found := c.deliveries[receipt]
	if !found {
		c.mu.Unlock()
		return errors.New("unknown receipt")
	}
	onRenew := c.onRenew
	c.mu.Unlock()
	if onRenew != nil {
		if err := onRenew(ctx, delivery.job); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, found := c.deliveries[receipt]; !found {
		return errors.New("unknown receipt")
	}
	delivery.expires = time.Now().Add(ttl)
	c.deliveries[receipt] = delivery
	return nil
}

func (c *clusterCoordinator) Ack(ctx context.Context, receipt string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delivery, found := c.deliveries[receipt]
	if !found {
		return errors.New("unknown receipt")
	}
	delete(c.deliveries, receipt)
	if c.runReceipts[delivery.job.RunID] == receipt {
		delete(c.runReceipts, delivery.job.RunID)
	}
	c.acks = append(c.acks, receipt)
	return nil
}

func (c *clusterCoordinator) redeliverExpired(runID string, now time.Time) (domain.JobDelivery, bool, error) {
	c.mu.Lock()
	receipt := c.runReceipts[runID]
	delivery, found := c.deliveries[receipt]
	if !found {
		c.mu.Unlock()
		return domain.JobDelivery{}, false, errors.New("active delivery not found")
	}
	if now.Before(delivery.expires) {
		c.mu.Unlock()
		return domain.JobDelivery{}, false, nil
	}
	delete(c.deliveries, receipt)
	newReceipt := uuid.NewString()
	delivery.receipt = newReceipt
	delivery.expires = time.Now().Add(5 * time.Second)
	c.deliveries[newReceipt] = delivery
	c.runReceipts[runID] = newReceipt
	queue := c.queue(clusterRoute{stream: delivery.job.StreamType, profile: delivery.job.Profile})
	c.mu.Unlock()

	redelivery := domain.JobDelivery{Job: delivery.job, Receipt: newReceipt}
	queue <- redelivery
	return redelivery, true, nil
}

func (c *clusterCoordinator) deliveryExpiry(receipt string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delivery, found := c.deliveries[receipt]
	return delivery.expires, found
}

func (c *clusterCoordinator) SettleJob(ctx context.Context, receipt string, status domain.JobStatus) error {
	if c.onSettle != nil {
		if err := c.onSettle(ctx, receipt, status); err != nil {
			return err
		}
	}
	c.mu.Lock()
	if previous, found := c.settled[receipt]; found {
		c.mu.Unlock()
		if previous == status {
			return nil
		}
		return errors.New("receipt already settled with another status")
	}
	delivery, found := c.deliveries[receipt]
	if !found {
		c.mu.Unlock()
		return errors.New("unknown receipt")
	}
	if delivery.job.ID != status.JobID || delivery.job.RunID != status.RunID || !terminalJobState(status.State) {
		c.mu.Unlock()
		return errors.New("invalid terminal status")
	}
	if previous, found := c.jobStatuses[status.RunID]; found && terminalJobState(previous.State) {
		c.mu.Unlock()
		return errors.New("run already settled")
	}
	c.jobStatuses[status.RunID] = status
	c.settled[receipt] = status
	delete(c.deliveries, receipt)
	if c.runReceipts[delivery.job.RunID] == receipt {
		delete(c.runReceipts, delivery.job.RunID)
	}
	c.acks = append(c.acks, receipt)
	waiters := make([]chan domain.JobStatus, 0, len(c.jobWaiters[status.RunID]))
	for waiter := range c.jobWaiters[status.RunID] {
		waiters = append(waiters, waiter)
	}
	c.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter <- status:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func terminalJobState(state domain.JobState) bool {
	return state == domain.JobStateCompleted || state == domain.JobStateFailed || state == domain.JobStateCancelled
}

func (c *clusterCoordinator) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	waiter := make(chan domain.JobStatus, 1)
	c.mu.Lock()
	if c.jobWaiters[runID] == nil {
		c.jobWaiters[runID] = make(map[chan domain.JobStatus]struct{})
	}
	c.jobWaiters[runID][waiter] = struct{}{}
	status, found := c.jobStatuses[runID]
	c.mu.Unlock()
	if found {
		waiter <- status
	}
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		delete(c.jobWaiters[runID], waiter)
		c.mu.Unlock()
	}()
	return waiter, nil
}

func clusterSegmentKey(info domain.SegmentData) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%t", info.MediaKey, info.Rendition, info.Index, info.IsVideo)
}

func (c *clusterCoordinator) NotifySegment(ctx context.Context, info domain.SegmentData, status domain.SegmentStatus) error {
	key := clusterSegmentKey(info)
	c.mu.Lock()
	waiters := make([]chan domain.SegmentStatus, 0, len(c.segmentWaiters[key]))
	for waiter := range c.segmentWaiters[key] {
		waiters = append(waiters, waiter)
	}
	c.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter <- status:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *clusterCoordinator) WaitSegment(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error) {
	waiter := make(chan domain.SegmentStatus, 1)
	key := clusterSegmentKey(info)
	c.mu.Lock()
	if c.segmentWaiters[key] == nil {
		c.segmentWaiters[key] = make(map[chan domain.SegmentStatus]struct{})
	}
	c.segmentWaiters[key][waiter] = struct{}{}
	c.mu.Unlock()
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		delete(c.segmentWaiters[key], waiter)
		c.mu.Unlock()
	}()
	return waiter, nil
}

func (c *clusterCoordinator) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := "job:" + jobID
	if lock, found := c.locks[key]; found && time.Now().Before(lock.expires) {
		return "", c.activeJobs[jobID], domain.ErrLockHeld
	}
	token := uuid.NewString()
	c.locks[key] = clusterLock{token: token, expires: time.Now().Add(ttl)}
	c.activeJobs[jobID] = runID
	return token, runID, nil
}

func (c *clusterCoordinator) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := "job:" + jobID
	lock, found := c.locks[key]
	if !found || lock.token != token || c.activeJobs[jobID] != runID {
		return errors.New("job lock ownership mismatch")
	}
	delete(c.locks, key)
	delete(c.activeJobs, jobID)
	c.releases = append(c.releases, key)
	return nil
}

func (c *clusterCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lock, found := c.locks[key]; found && time.Now().Before(lock.expires) {
		return "", domain.ErrLockHeld
	}
	token := uuid.NewString()
	c.locks[key] = clusterLock{token: token, expires: time.Now().Add(ttl)}
	return token, nil
}

func (c *clusterCoordinator) ReleaseLock(ctx context.Context, key, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock, found := c.locks[key]
	if !found || lock.token != token {
		return errors.New("lock ownership mismatch")
	}
	delete(c.locks, key)
	c.releases = append(c.releases, key)
	return nil
}

func (c *clusterCoordinator) releaseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.releases)
}

func newClusterController(store Storage, coordinator Coordinator) *Controller {
	return NewController(Options{
		Storage:        store,
		Coordinator:    coordinator,
		PathGen:        stubPathGen{},
		SegmentTimeout: time.Second,
		JobTimeout:     time.Second,
	})
}

func TestRemoteWorkerPoolCompletionFinishesOriginatingFlight(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	coordinator.onEnqueue = func(job domain.Job) {
		for _, index := range job.ExpectedIndices {
			info := domain.SegmentData{MediaKey: job.MediaKey, Index: index, Rendition: job.Rendition, IsVideo: true}
			if err := store.WriteSegment(context.Background(), info, []byte("remote")); err != nil {
				t.Errorf("write remote segment: %v", err)
			}
		}
	}
	requester := newClusterController(store, coordinator)
	worker := newClusterController(store, coordinator)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	if err := worker.Start(workerCtx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()

	data, err := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "remote" {
		t.Fatalf("unexpected segment %q", data)
	}
	waitUntil(t, time.Second, func() bool { return coordinator.releaseCount() == 1 })
}

func TestAmbiguousEnqueueKeepsRunRegisteredUntilTerminalStatus(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	coordinator.enqueueErr = errors.New("connection lost after enqueue")
	jobSent := make(chan domain.Job, 1)
	coordinator.onEnqueue = func(job domain.Job) { jobSent <- job }
	allowWorker := make(chan struct{})
	workerRenewed := make(chan struct{})
	var renewed sync.Once
	coordinator.onRenew = func(ctx context.Context, job domain.Job) error {
		renewed.Do(func() { close(workerRenewed) })
		select {
		case <-allowWorker:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, index := range job.ExpectedIndices {
			store.setSegment(index, []byte("remote"))
		}
		return nil
	}
	requester := newClusterController(store, coordinator)
	worker := newClusterController(store, coordinator)
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()

	_, requestErr := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
	if requestErr == nil || !errors.Is(requestErr, coordinator.enqueueErr) {
		t.Fatalf("expected ambiguous enqueue error, got %v", requestErr)
	}
	job := <-jobSent
	select {
	case <-workerRenewed:
	case <-time.After(time.Second):
		t.Fatal("worker did not receive persisted delivery")
	}
	if coordinator.releaseCount() != 0 {
		t.Fatal("ambiguous enqueue released the active run before a terminal status")
	}
	coordinator.mu.Lock()
	lock := coordinator.locks["job:"+job.ID]
	coordinator.mu.Unlock()
	if delta := lock.expires.Sub(job.Deadline); delta < time.Minute-20*time.Millisecond || delta > time.Minute+20*time.Millisecond {
		t.Fatalf("job and lock do not share one deadline: lock expires %s after job", delta)
	}
	close(allowWorker)
	waitUntil(t, time.Second, func() bool { return coordinator.releaseCount() == 1 })
}

func TestStoppedWorkerDeliveryCompletesAfterRedelivery(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	jobSent := make(chan domain.Job, 1)
	coordinator.onEnqueue = func(job domain.Job) { jobSent <- job }
	workerRenewing := make(chan struct{})
	var renewing sync.Once
	coordinator.onRenew = func(ctx context.Context, job domain.Job) error {
		renewing.Do(func() { close(workerRenewing) })
		<-ctx.Done()
		return ctx.Err()
	}
	requester := newClusterController(store, coordinator)
	workerA := newClusterController(store, coordinator)
	workerACtx, stopWorkerA := context.WithCancel(context.Background())
	if err := workerA.Start(workerACtx); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, requestErr := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		result <- requestErr
	}()
	job := <-jobSent
	select {
	case <-workerRenewing:
	case <-time.After(time.Second):
		t.Fatal("first worker did not begin renewing its delivery")
	}
	stopWorkerA()
	workerA.Stop()

	for _, index := range job.ExpectedIndices {
		store.setSegment(index, []byte("redelivered"))
	}
	coordinator.onRenew = nil
	workerB := newClusterController(store, coordinator)
	if err := workerB.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer workerB.Stop()

	coordinator.mu.Lock()
	receipt := coordinator.runReceipts[job.RunID]
	expires := coordinator.deliveries[receipt].expires
	coordinator.mu.Unlock()
	if _, redelivered, err := coordinator.redeliverExpired(job.RunID, expires.Add(time.Millisecond)); err != nil || !redelivered {
		t.Fatalf("delivery was not redelivered: redelivered=%t err=%v", redelivered, err)
	}
	select {
	case requestErr := <-result:
		if requestErr != nil {
			t.Fatalf("request did not complete after redelivery: %v", requestErr)
		}
	case <-time.After(time.Second):
		t.Fatal("redelivered worker did not complete the request")
	}
}

func TestWorkerRenewsLeaseThroughTerminalSettlement(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	jobSent := make(chan domain.Job, 1)
	coordinator.onEnqueue = func(job domain.Job) {
		jobSent <- job
		for _, index := range job.ExpectedIndices {
			store.setSegment(index, []byte("settled"))
		}
	}
	settling := make(chan string, 1)
	allowSettle := make(chan struct{})
	coordinator.onSettle = func(ctx context.Context, receipt string, status domain.JobStatus) error {
		select {
		case settling <- receipt:
		default:
		}
		select {
		case <-allowSettle:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	requester := newClusterController(store, coordinator)
	worker := newClusterController(store, coordinator)
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	requestDone := make(chan error, 1)
	go func() {
		_, requestErr := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		requestDone <- requestErr
	}()
	job := <-jobSent
	var receipt string
	select {
	case receipt = <-settling:
	case <-time.After(time.Second):
		t.Fatal("worker did not begin terminal settlement")
	}
	initialExpiry, found := coordinator.deliveryExpiry(receipt)
	if !found {
		t.Fatal("delivery disappeared before settlement")
	}
	waitUntil(t, 6*time.Second, func() bool {
		expires, active := coordinator.deliveryExpiry(receipt)
		return active && expires.After(initialExpiry)
	})
	if _, redelivered, err := coordinator.redeliverExpired(job.RunID, initialExpiry.Add(time.Millisecond)); err != nil || redelivered {
		t.Fatalf("delivery expired while terminal settlement was active: redelivered=%t err=%v", redelivered, err)
	}
	close(allowSettle)
	select {
	case requestErr := <-requestDone:
		if requestErr != nil {
			t.Fatalf("request failed: %v", requestErr)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not complete")
	}
}

func TestSuccessfulSettlementSuppressesInFlightRenewalFailure(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	coordinator.onEnqueue = func(job domain.Job) {
		for _, index := range job.ExpectedIndices {
			store.setSegment(index, []byte("settled"))
		}
	}
	renewInFlight := make(chan struct{})
	releaseRenew := make(chan struct{})
	var renewals int
	var renewMu sync.Mutex
	coordinator.onRenew = func(ctx context.Context, job domain.Job) error {
		renewMu.Lock()
		renewals++
		attempt := renewals
		renewMu.Unlock()
		if attempt == 2 {
			close(renewInFlight)
			select {
			case <-releaseRenew:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	coordinator.onSettle = func(ctx context.Context, receipt string, status domain.JobStatus) error {
		select {
		case <-renewInFlight:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var eventsMu sync.Mutex
	var events []domain.Event
	observer := domain.ObserverFunc(func(ctx context.Context, event domain.Event) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})
	requester := newClusterController(store, coordinator)
	worker := NewController(Options{
		Storage:        store,
		Coordinator:    coordinator,
		PathGen:        stubPathGen{},
		SegmentTimeout: time.Second,
		JobTimeout:     time.Second,
		Observer:       observer,
	})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, requestErr := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		requestDone <- requestErr
	}()
	go func() {
		for {
			coordinator.mu.Lock()
			settled := len(coordinator.settled) > 0
			coordinator.mu.Unlock()
			if settled {
				close(releaseRenew)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case requestErr := <-requestDone:
		if requestErr != nil {
			t.Fatalf("request failed: %v", requestErr)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("request did not complete")
	}
	worker.Stop()
	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, event := range events {
		if event.Type == domain.EventJobLeaseFailed {
			t.Fatalf("successful settlement emitted a lease failure: %s", event.Error)
		}
	}
}

func TestRemoteWaiterSurvivesOriginRequestCancellation(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	requesterA := newClusterController(store, coordinator)
	requesterB := newClusterController(store, coordinator)
	worker := newClusterController(store, coordinator)
	jobReachedQueue := make(chan domain.Job, 1)
	allowWorker := make(chan struct{})
	coordinator.onEnqueue = func(job domain.Job) {
		jobReachedQueue <- job
	}
	var writeSegments sync.Once
	coordinator.onRenew = func(ctx context.Context, job domain.Job) error {
		select {
		case <-allowWorker:
		case <-ctx.Done():
			return ctx.Err()
		}
		writeSegments.Do(func() {
			for _, index := range job.ExpectedIndices {
				info := domain.SegmentData{MediaKey: job.MediaKey, Index: index, Rendition: job.Rendition, IsVideo: true}
				if err := store.WriteSegment(context.Background(), info, []byte("remote")); err != nil {
					t.Errorf("write remote segment: %v", err)
				}
			}
		})
		return nil
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	if err := worker.Start(workerCtx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()

	ctxA, cancelA := context.WithCancel(context.Background())
	resultA := make(chan error, 1)
	go func() {
		_, requestErr := requesterA.Segment(ctxA, "file:///media", domain.StreamVideo, "1080p", 0)
		resultA <- requestErr
	}()
	var job domain.Job
	select {
	case job = <-jobReachedQueue:
	case <-time.After(time.Second):
		t.Fatal("origin did not enqueue")
	}
	resultB := make(chan error, 1)
	go func() {
		_, requestErr := requesterB.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		resultB <- requestErr
	}()
	waitUntil(t, time.Second, func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return len(coordinator.jobWaiters[job.RunID]) >= 2
	})
	cancelA()
	close(allowWorker)
	if requestErr := <-resultA; !errors.Is(requestErr, context.Canceled) {
		t.Fatalf("expected origin cancellation, got %v", requestErr)
	}
	select {
	case requestErr := <-resultB:
		if requestErr != nil {
			t.Fatalf("remote waiter failed: %v", requestErr)
		}
	case <-time.After(time.Second):
		t.Fatal("remote waiter did not complete")
	}
}

func TestStaleRunCannotFinishReplacementFlight(t *testing.T) {
	controller := &Controller{flights: newFlightRegistry()}
	old := &flight{jobID: "batch", runID: "old", done: make(chan struct{})}
	replacement := &flight{jobID: "batch", runID: "new", done: make(chan struct{})}
	controller.flights.flights[replacement.jobID] = replacement
	controller.finishFlight(old)
	if controller.flights.flights[replacement.jobID] != replacement {
		t.Fatal("stale run removed replacement flight")
	}
	select {
	case <-replacement.done:
		t.Fatal("stale run closed replacement flight")
	default:
	}
}

func TestDurableJobFailureWakesEveryReplica(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	requesterA := newClusterController(store, coordinator)
	requesterB := newClusterController(store, coordinator)
	deliveries, err := coordinator.Subscribe(context.Background(), domain.StreamVideo, requesterA.pipelineVersion)
	if err != nil {
		t.Fatal(err)
	}
	resultA := make(chan error, 1)
	go func() {
		_, requestErr := requesterA.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		resultA <- requestErr
	}()
	var delivery domain.JobDelivery
	select {
	case delivery = <-deliveries:
	case <-time.After(time.Second):
		t.Fatal("job was not delivered")
	}
	resultB := make(chan error, 1)
	go func() {
		_, requestErr := requesterB.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		resultB <- requestErr
	}()
	waitUntil(t, time.Second, func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return len(coordinator.jobWaiters[delivery.Job.RunID]) >= 2
	})
	if err := coordinator.SettleJob(context.Background(), delivery.Receipt, domain.JobStatus{JobID: delivery.Job.ID, RunID: delivery.Job.RunID, State: domain.JobStateFailed, Error: "remote failure"}); err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]<-chan error{"owner": resultA, "lock waiter": resultB} {
		select {
		case requestErr := <-result:
			if requestErr == nil || requestErr.Error() != "segment error: remote failure" {
				t.Fatalf("%s received unexpected request error: %v", name, requestErr)
			}
		case <-time.After(time.Second):
			t.Fatalf("durable job failure did not wake %s", name)
		}
	}
}

func TestStaleSegmentFailureDoesNotPoisonReplacementRun(t *testing.T) {
	metadata, err := jsonMetadata(testVideoMetadata(24, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	store := &stubStorage{metaData: metadata, metaExists: true, segments: make(map[int][]byte)}
	coordinator := newClusterCoordinator()
	requester := newClusterController(store, coordinator)
	deliveries, err := coordinator.Subscribe(context.Background(), domain.StreamVideo, requester.pipelineVersion)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, requestErr := requester.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 0)
		result <- struct {
			data []byte
			err  error
		}{data: data, err: requestErr}
	}()
	var delivery domain.JobDelivery
	select {
	case delivery = <-deliveries:
	case <-time.After(time.Second):
		t.Fatal("replacement job was not delivered")
	}
	info := domain.SegmentData{MediaKey: delivery.Job.MediaKey, Index: 0, Rendition: "1080p", IsVideo: true}
	waitUntil(t, time.Second, func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return len(coordinator.segmentWaiters[clusterSegmentKey(info)]) > 0
	})
	if err := coordinator.NotifySegment(context.Background(), info, domain.SegmentStatus{State: domain.SegmentStateError, Error: "old run failure"}); err != nil {
		t.Fatal(err)
	}
	select {
	case early := <-result:
		t.Fatalf("unscoped segment failure completed replacement run: data=%q err=%v", early.data, early.err)
	case <-time.After(20 * time.Millisecond):
	}
	store.setSegment(0, []byte("replacement"))
	if err := coordinator.SettleJob(context.Background(), delivery.Receipt, domain.JobStatus{JobID: delivery.Job.ID, RunID: delivery.Job.RunID, State: domain.JobStateCompleted}); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-result:
		if completed.err != nil || string(completed.data) != "replacement" {
			t.Fatalf("replacement run failed: data=%q err=%v", completed.data, completed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement run did not complete")
	}
}

func TestDeliveryLeaseRenewalAndRedelivery(t *testing.T) {
	coordinator := newClusterCoordinator()
	deliveries, err := coordinator.Subscribe(context.Background(), domain.StreamVideo, "profile")
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "batch", RunID: "run", StreamType: domain.StreamVideo, Profile: "profile"}
	if err := coordinator.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	first := <-deliveries
	if err := coordinator.Renew(context.Background(), first.Receipt, time.Minute); err != nil {
		t.Fatal(err)
	}
	expires, found := coordinator.deliveryExpiry(first.Receipt)
	if !found {
		t.Fatal("renewed delivery disappeared")
	}
	if _, redelivered, err := coordinator.redeliverExpired(job.RunID, expires.Add(-time.Millisecond)); err != nil || redelivered {
		t.Fatalf("renewed delivery was redelivered early: redelivered=%t err=%v", redelivered, err)
	}
	second, redelivered, err := coordinator.redeliverExpired(job.RunID, expires.Add(time.Millisecond))
	if err != nil || !redelivered {
		t.Fatalf("expired delivery was not redelivered: redelivered=%t err=%v", redelivered, err)
	}
	queued := <-deliveries
	if queued.Receipt != second.Receipt || queued.Receipt == first.Receipt {
		t.Fatalf("redelivery receipt was not replaced: first=%q second=%q queued=%q", first.Receipt, second.Receipt, queued.Receipt)
	}
	if err := coordinator.Renew(context.Background(), first.Receipt, time.Minute); err == nil {
		t.Fatal("stale receipt renewed after redelivery")
	}
	if err := coordinator.Ack(context.Background(), first.Receipt); err == nil {
		t.Fatal("stale receipt acknowledged after redelivery")
	}
	if err := coordinator.Ack(context.Background(), second.Receipt); err != nil {
		t.Fatalf("active receipt was not acknowledged: %v", err)
	}
}

func TestTerminalSettlementIsAtomicAndIdempotent(t *testing.T) {
	coordinator := newClusterCoordinator()
	deliveries, err := coordinator.Subscribe(context.Background(), domain.StreamVideo, "profile")
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "batch", RunID: "run", StreamType: domain.StreamVideo, Profile: "profile"}
	if err := coordinator.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	delivery := <-deliveries
	status := domain.JobStatus{JobID: job.ID, RunID: job.RunID, State: domain.JobStateCompleted}
	if err := coordinator.SettleJob(context.Background(), delivery.Receipt, status); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.SettleJob(context.Background(), delivery.Receipt, status); err != nil {
		t.Fatalf("identical settlement retry was not idempotent: %v", err)
	}
	if _, _, err := coordinator.redeliverExpired(job.RunID, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("settled delivery remained available for redelivery")
	}
	waiter, err := coordinator.WaitJob(context.Background(), job.RunID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case replay := <-waiter:
		if replay != status {
			t.Fatalf("unexpected terminal replay: %#v", replay)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal status was not replayed")
	}
}

func TestCoordinatorRoutesDeliveriesByPipelineProfile(t *testing.T) {
	coordinator := newClusterCoordinator()
	matched, err := coordinator.Subscribe(context.Background(), domain.StreamVideo, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := coordinator.Subscribe(context.Background(), domain.StreamVideo, "profile-b")
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "batch", RunID: "run", StreamType: domain.StreamVideo, Profile: "profile-a"}
	if err := coordinator.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	select {
	case delivery := <-matched:
		if delivery.Job.RunID != job.RunID {
			t.Fatalf("unexpected delivery: %#v", delivery)
		}
	case <-time.After(time.Second):
		t.Fatal("matching worker did not receive delivery")
	}
	select {
	case delivery := <-mismatched:
		t.Fatalf("mismatched worker received delivery: %#v", delivery)
	case <-time.After(20 * time.Millisecond):
	}
}

func jsonMetadata(metadata *domain.Metadata) ([]byte, error) {
	return json.Marshal(metadata)
}

func waitUntil(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met before timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
