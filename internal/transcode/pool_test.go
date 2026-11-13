package transcode

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/google/uuid"
)

type stubCoordinator struct {
	publishes    []domain.SegmentStatus
	jobPublishes []domain.JobStatus
	subErr       error
	acks         []string
	profiles     []string
	jobErr       error
	renewErr     error
	renews       []string
	onRenew      func(ctx context.Context)
}

func (s *stubCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	return nil
}
func (s *stubCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	s.profiles = append(s.profiles, profile)
	if s.subErr != nil {
		return nil, s.subErr
	}
	ch := make(chan domain.JobDelivery)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}
func (s *stubCoordinator) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	s.renews = append(s.renews, receipt)
	if s.onRenew != nil {
		s.onRenew(ctx)
	}
	return s.renewErr
}
func (s *stubCoordinator) SettleJob(ctx context.Context, receipt string, status domain.JobStatus) error {
	s.jobPublishes = append(s.jobPublishes, status)
	if s.jobErr != nil {
		return s.jobErr
	}
	s.acks = append(s.acks, receipt)
	return nil
}
func (s *stubCoordinator) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	return make(chan domain.JobStatus), nil
}
func (s *stubCoordinator) Ack(ctx context.Context, receipt string) error {
	s.acks = append(s.acks, receipt)
	return nil
}
func (s *stubCoordinator) NotifySegment(ctx context.Context, info domain.SegmentData, status domain.SegmentStatus) error {
	s.publishes = append(s.publishes, status)
	return nil
}
func (s *stubCoordinator) WaitSegment(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error) {
	ch := make(chan domain.SegmentStatus)
	close(ch)
	return ch, nil
}
func (s *stubCoordinator) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	return uuid.New().String(), runID, nil
}
func (s *stubCoordinator) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	return nil
}
func (s *stubCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return uuid.New().String(), nil
}
func (s *stubCoordinator) ReleaseLock(ctx context.Context, key string, token string) error {
	return nil
}
func (s *stubCoordinator) Close() {}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func TestNewPool(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 2, domain.StreamVideo, 6.0, nil, nil, nil)

	if pool.size != 2 {
		t.Fatalf("expected size 2, got %d", pool.size)
	}
	if pool.streamType != domain.StreamVideo {
		t.Fatalf("expected video stream type")
	}
}

func TestValidateJobRejectsMixedPipelineProfile(t *testing.T) {
	pool := &Pool{streamType: domain.StreamVideo, profile: "worker-profile"}
	job := domain.Job{
		ID:              "job",
		RunID:           "run",
		MediaKey:        "media",
		Profile:         "other-profile",
		SourceURL:       "source",
		Rendition:       "720p",
		StreamType:      domain.StreamVideo,
		StartIndex:      0,
		EndIndex:        0,
		RequestedIndex:  0,
		EnqueuedAt:      time.Now(),
		StartBy:         time.Now().Add(time.Minute),
		Segments:        []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}},
		ExpectedIndices: []int{0},
		VideoRendition:  &domain.VideoRendition{Name: "720p", Method: domain.Transcode},
	}
	if err := pool.validateJob(job); err == nil {
		t.Fatalf("expected mixed pipeline profile rejection")
	}
	job.Profile = "worker-profile"
	if err := pool.validateJob(job); err != nil {
		t.Fatalf("expected valid concrete job: %v", err)
	}
}

func TestPoolStartAndStop(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "pipeline-a"})

	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("start error: %v", err)
	}

	pool.Stop()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("restart after stop should work: %v", err)
	}
	pool.Stop()
	if len(coord.profiles) != 2 || coord.profiles[0] != "pipeline-a" || coord.profiles[1] != "pipeline-a" {
		t.Fatalf("subscriptions must be scoped to the worker profile: %v", coord.profiles)
	}
}

func TestPoolStartSubscribeFailureCanRetry(t *testing.T) {
	coord := &stubCoordinator{subErr: errors.New("subscribe failed")}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil)

	if err := pool.Start(context.Background()); err == nil {
		t.Fatalf("expected start error")
	}

	coord.subErr = nil
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("expected retry to start after subscribe failure, got %v", err)
	}
	pool.Stop()
}

func TestPoolStartTwiceReturnsError(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil)

	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("first start error: %v", err)
	}

	if err := pool.Start(ctx); err == nil {
		t.Fatalf("expected error on second start")
	}

	pool.Stop()
}

func TestPoolStopWithoutStart(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil)
	pool.Stop()
}

func TestWorkerFinishRecordsErrorAndClosesDone(t *testing.T) {
	w := NewWorker(WorkerSpec{})
	w.finish(WorkerStateError, assertErr("test error"))
	if w.Err() == nil || w.Err().Error() != "test error" {
		t.Fatalf("expected error to be set, got %v", w.Err())
	}
	if w.State() != WorkerStateError {
		t.Fatalf("expected error state, got %v", w.State())
	}
	select {
	case <-w.Done():
	default:
		t.Fatalf("a finished worker must close Done")
	}
}

func TestWaitForWorkerReturnsWorkerError(t *testing.T) {
	p := &Pool{}
	w := NewWorker(WorkerSpec{})
	w.finish(WorkerStateError, assertErr("ffmpeg failed"))

	err := p.waitForWorker(context.Background(), w)
	if err == nil || err.Error() != "ffmpeg failed" {
		t.Fatalf("expected worker error, got %v", err)
	}
}

func TestValidateJobNeighbourRules(t *testing.T) {
	segments := []domain.Segment{{Index: 1, Start: 6, End: 12, Duration: 6}}
	overlap := domain.Segment{Index: 0, Start: 0, End: 6, Duration: 6}
	trailing := domain.Segment{Index: 2, Start: 12, End: 18, Duration: 6}
	base := func(stream domain.StreamType) domain.Job {
		job := domain.Job{ID: "job", RunID: "run", MediaKey: "media", Profile: "profile", SourceURL: "source", Rendition: "r", StreamType: stream, StartIndex: 1, EndIndex: 1, RequestedIndex: 1, EnqueuedAt: time.Now(), StartBy: time.Now().Add(time.Minute), Segments: segments, ExpectedIndices: []int{1}}
		if stream == domain.StreamVideo {
			job.VideoRendition = &domain.VideoRendition{Name: "r", Method: domain.DirectStream}
		} else {
			job.AudioRendition = &domain.AudioRendition{Name: "r", Method: domain.Transcode}
		}
		return job
	}
	video := NewPool(&stubCoordinator{}, 1, domain.StreamVideo, 6, nil, nil, nil, PoolConfig{Profile: "profile"})
	audio := NewPool(&stubCoordinator{}, 1, domain.StreamAudio, 6, nil, nil, nil, PoolConfig{Profile: "profile"})

	copied := base(domain.StreamVideo)
	copied.Overlap, copied.Trailing = &overlap, &trailing
	if err := video.validateJob(copied); err != nil {
		t.Fatalf("copied video with adjacent neighbours must be valid: %v", err)
	}

	withOverlap := base(domain.StreamAudio)
	withOverlap.Overlap = &overlap
	if err := audio.validateJob(withOverlap); err != nil {
		t.Fatalf("audio with an adjacent overlap must be valid: %v", err)
	}

	rejected := map[string]struct {
		pool *Pool
		job  func() domain.Job
	}{
		"audio trailing": {audio, func() domain.Job { job := base(domain.StreamAudio); job.Trailing = &trailing; return job }},
		"transcoded video trailing": {video, func() domain.Job {
			job := base(domain.StreamVideo)
			job.VideoRendition = &domain.VideoRendition{Name: "r", Method: domain.Transcode}
			job.Trailing = &trailing
			return job
		}},
		"trailing wrong index": {video, func() domain.Job {
			job := base(domain.StreamVideo)
			job.Trailing = &domain.Segment{Index: 3, Start: 12, End: 18, Duration: 6}
			return job
		}},
		"trailing wrong start": {video, func() domain.Job {
			job := base(domain.StreamVideo)
			job.Trailing = &domain.Segment{Index: 2, Start: 13, End: 18, Duration: 5}
			return job
		}},
		"overlap wrong index": {audio, func() domain.Job {
			job := base(domain.StreamAudio)
			job.Overlap = &domain.Segment{Index: 5, Start: 0, End: 6, Duration: 6}
			return job
		}},
		"negative video stream index": {video, func() domain.Job { job := base(domain.StreamVideo); job.VideoStreamIndex = -1; return job }},
	}
	for name, test := range rejected {
		if err := test.pool.validateJob(test.job()); err == nil {
			t.Errorf("%s: job must be rejected", name)
		}
	}
}

func TestPoolProcessJobSkipsJobAlreadyRunning(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil)
	if started := pool.beginJob("run", func(error) {}); !started {
		t.Fatal("expected first begin to start")
	}

	pool.processJob(context.Background(), validPoolDelivery("receipt", "run"))

	if len(coord.acks) != 0 || len(coord.publishes) != 0 {
		t.Fatalf("duplicate must be skipped without side effects: acks=%d publishes=%d", len(coord.acks), len(coord.publishes))
	}
}

func TestFailJobLeavesDeliveryPendingDuringShutdown(t *testing.T) {
	coord := &stubCoordinator{}
	pool := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)

	pool.failJob(ctx, domain.JobDelivery{Job: domain.Job{Rendition: "1080p", StartIndex: 0, EndIndex: 3}, Receipt: "receipt"}, context.Canceled, time.Now())

	if len(coord.publishes) != 0 {
		t.Fatalf("shutdown must not poison segment status, got %d publishes", len(coord.publishes))
	}
	if len(coord.acks) != 0 || len(coord.jobPublishes) != 0 {
		t.Fatalf("shutdown must leave the delivery pending: acks=%d statuses=%v", len(coord.acks), coord.jobPublishes)
	}
}

func TestTerminalSettlementFailureLeavesJobPending(t *testing.T) {
	coord := &stubCoordinator{jobErr: errors.New("coordinator unavailable")}
	pool := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	delivery := domain.JobDelivery{Job: domain.Job{ID: "job", RunID: "run", Rendition: "1080p", StartIndex: 0, EndIndex: 0}, Receipt: "receipt"}

	pool.completeJob(delivery, time.Now())

	if len(coord.acks) != 0 {
		t.Fatalf("job must remain pending when terminal status cannot be persisted: %v", coord.acks)
	}
	if len(coord.jobPublishes) != 3 {
		t.Fatalf("expected three terminal status attempts, got %d", len(coord.jobPublishes))
	}
}

func TestCompletedJobSettlesDeliveryReceipt(t *testing.T) {
	coord := &stubCoordinator{}
	pool := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	delivery := domain.JobDelivery{Job: domain.Job{ID: "batch", RunID: "run"}, Receipt: "delivery-receipt"}
	pool.completeJob(delivery, time.Now())
	if len(coord.acks) != 1 || coord.acks[0] != delivery.Receipt {
		t.Fatalf("expected receipt-scoped acknowledgement, got %v", coord.acks)
	}
}

func TestInitialLeaseFailureLeavesDeliveryPending(t *testing.T) {
	coord := &stubCoordinator{renewErr: errors.New("lease unavailable")}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile"})
	delivery := domain.JobDelivery{
		Receipt: "receipt",
		Job: domain.Job{
			ID:              "batch",
			RunID:           "run",
			MediaKey:        "media",
			Profile:         "profile",
			SourceURL:       "source",
			Rendition:       "720p",
			StreamType:      domain.StreamVideo,
			StartIndex:      0,
			EndIndex:        0,
			RequestedIndex:  0,
			EnqueuedAt:      time.Now(),
			StartBy:         time.Now().Add(time.Minute),
			Segments:        []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}},
			ExpectedIndices: []int{0},
			VideoRendition:  &domain.VideoRendition{Name: "720p", Method: domain.Transcode},
		},
	}
	pool.processJob(context.Background(), delivery)
	if len(coord.acks) != 0 || len(coord.jobPublishes) != 0 {
		t.Fatalf("lease failure must leave delivery pending: acks=%v statuses=%v", coord.acks, coord.jobPublishes)
	}
}

func TestExpiredDeliveryPublishesFailureAndSettles(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile"})
	delivery := domain.JobDelivery{
		Receipt: "expired-receipt",
		Job: domain.Job{
			ID:              "batch",
			RunID:           "expired-run",
			MediaKey:        "media",
			Profile:         "profile",
			SourceURL:       "source",
			Rendition:       "720p",
			StreamType:      domain.StreamVideo,
			StartIndex:      0,
			EndIndex:        0,
			RequestedIndex:  0,
			EnqueuedAt:      time.Now().Add(-time.Minute),
			StartBy:         time.Now().Add(-time.Second),
			Segments:        []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}},
			ExpectedIndices: []int{0},
			VideoRendition:  &domain.VideoRendition{Name: "720p", Method: domain.Transcode},
		},
	}

	pool.processJob(context.Background(), delivery)

	if len(coord.jobPublishes) != 1 || coord.jobPublishes[0].State != domain.JobStateFailed || coord.jobPublishes[0].Error != ErrJobExpired.Error() {
		t.Fatalf("expired delivery did not publish one terminal failure: %#v", coord.jobPublishes)
	}
	if len(coord.acks) != 1 || coord.acks[0] != delivery.Receipt {
		t.Fatalf("expired delivery was not acknowledged: %v", coord.acks)
	}
}

type eventRecorder struct {
	mu     sync.Mutex
	events []domain.Event
	signal chan domain.Event
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{signal: make(chan domain.Event, 256)}
}

func (r *eventRecorder) Observe(_ context.Context, event domain.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	select {
	case r.signal <- event:
	default:
	}
}

func (r *eventRecorder) snapshot() []domain.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Event(nil), r.events...)
}

func (r *eventRecorder) count(eventType domain.EventType) int {
	total := 0
	for _, event := range r.snapshot() {
		if event.Type == eventType {
			total++
		}
	}
	return total
}

func (r *eventRecorder) await(t *testing.T, eventType domain.EventType) domain.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-r.signal:
			if event.Type == eventType {
				return event
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", eventType)
		}
	}
}

func validPoolDelivery(receipt, runID string) domain.JobDelivery {
	return domain.JobDelivery{
		Receipt: receipt,
		Job: domain.Job{
			ID:              "batch",
			RunID:           runID,
			MediaKey:        "media",
			Profile:         "profile",
			SourceURL:       "source",
			Rendition:       "720p",
			StreamType:      domain.StreamVideo,
			StartIndex:      0,
			EndIndex:        0,
			RequestedIndex:  0,
			EnqueuedAt:      time.Now(),
			StartBy:         time.Now().Add(time.Minute),
			Segments:        []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}},
			ExpectedIndices: []int{0},
			VideoRendition:  &domain.VideoRendition{Name: "720p", Method: domain.Transcode},
		},
	}
}

func fastBackoff() backoff.BackOff {
	return backoff.NewConstantBackOff(time.Millisecond)
}

type resubscribingCoordinator struct {
	*stubCoordinator
	mu         sync.Mutex
	subscribes int
	subscribed chan int
}

func (c *resubscribingCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	c.mu.Lock()
	c.subscribes++
	n := c.subscribes
	c.mu.Unlock()
	ch := make(chan domain.JobDelivery)
	if n == 1 {
		close(ch)
	} else {
		go func() {
			<-ctx.Done()
			close(ch)
		}()
	}
	c.subscribed <- n
	return ch, nil
}

func TestPoolResubscribesAfterSubscriptionCloses(t *testing.T) {
	coord := &resubscribingCoordinator{stubCoordinator: &stubCoordinator{}, subscribed: make(chan int, 16)}
	recorder := newEventRecorder()
	pool := NewPool(coord, 2, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	pool.newBackoff = fastBackoff

	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer pool.Stop()

	timeout := time.After(5 * time.Second)
	for second := false; !second; {
		select {
		case n := <-coord.subscribed:
			second = n >= 2
		case <-timeout:
			t.Fatal("pool never resubscribed after its subscription closed")
		}
	}
	event := recorder.await(t, domain.EventPoolSubscriptionLost)
	if event.StreamType != domain.StreamVideo {
		t.Fatalf("subscription loss must name the stream type, got %q", event.StreamType)
	}
}

type renewScriptCoordinator struct {
	*stubCoordinator
	calls   atomic.Int32
	fail    func(call int32) bool
	reached int32
	done    chan struct{}
}

func (c *renewScriptCoordinator) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	call := c.calls.Add(1)
	if call == c.reached {
		close(c.done)
	}
	if c.fail(call) {
		return errors.New("renew unavailable")
	}
	return nil
}

func TestPoolToleratesTransientRenewFailure(t *testing.T) {
	coord := &renewScriptCoordinator{
		stubCoordinator: &stubCoordinator{},
		fail:            func(call int32) bool { return call%2 == 1 },
		reached:         4,
		done:            make(chan struct{}),
	}
	pool := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	baseCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	leaseCtx, stopLease := context.WithCancel(baseCtx)
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		pool.renewLease(leaseCtx, validPoolDelivery("receipt", "run"), 600*time.Millisecond, cancel, time.Now(), &leaseSettlement{})
	}()

	select {
	case <-coord.done:
	case <-leaseDone:
	case <-time.After(10 * time.Second):
		t.Fatal("renewals never reached the expected count")
	}
	if err := baseCtx.Err(); err != nil {
		t.Fatalf("a transient renew failure must not cancel the job: %v", context.Cause(baseCtx))
	}
	stopLease()
	<-leaseDone
}

func TestPoolCancelsAfterRenewFailuresExhaustLeaseWindow(t *testing.T) {
	coord := &renewScriptCoordinator{
		stubCoordinator: &stubCoordinator{},
		fail:            func(int32) bool { return true },
		reached:         -1,
	}
	recorder := newEventRecorder()
	pool := &Pool{coordinator: coord, streamType: domain.StreamVideo, observer: recorder}
	baseCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go pool.renewLease(baseCtx, validPoolDelivery("receipt", "run"), 600*time.Millisecond, cancel, time.Now(), &leaseSettlement{})

	select {
	case <-baseCtx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("persistent renew failure never cancelled the job")
	}
	if !errors.Is(context.Cause(baseCtx), ErrDeliveryLeaseLost) {
		t.Fatalf("expected ErrDeliveryLeaseLost, got %v", context.Cause(baseCtx))
	}
	if recorder.count(domain.EventJobLeaseFailed) != 1 {
		t.Fatalf("expected one lease failure event, got %d", recorder.count(domain.EventJobLeaseFailed))
	}
}

func TestPoolRejectsReceiptlessDeliveryWithDistinctEvent(t *testing.T) {
	coord := &stubCoordinator{}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})

	pool.processJob(context.Background(), validPoolDelivery("", "run"))

	if recorder.count(domain.EventJobDeliveryRejected) != 1 {
		t.Fatalf("expected one delivery rejection, got events %v", recorder.snapshot())
	}
	if recorder.count(domain.EventJobFailed) != 0 {
		t.Fatalf("a rejected delivery is not a failed job: %v", recorder.snapshot())
	}
	for _, event := range recorder.snapshot() {
		if event.Type == domain.EventJobDeliveryRejected && event.Error == "" {
			t.Fatalf("rejection must carry its reason")
		}
	}
	if len(coord.acks) != 0 || len(coord.jobPublishes) != 0 {
		t.Fatalf("a receiptless delivery cannot be settled: acks=%v statuses=%v", coord.acks, coord.jobPublishes)
	}
}

func TestPoolRejectsInvalidDeliveryWithDistinctEvent(t *testing.T) {
	coord := &stubCoordinator{}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	delivery := validPoolDelivery("receipt", "run")
	delivery.Job.Profile = "other"

	pool.processJob(context.Background(), delivery)

	if recorder.count(domain.EventJobDeliveryRejected) != 1 || recorder.count(domain.EventJobFailed) != 0 {
		t.Fatalf("invalid delivery must be rejected, not failed: %v", recorder.snapshot())
	}
	if len(coord.jobPublishes) != 1 || coord.jobPublishes[0].State != domain.JobStateFailed {
		t.Fatalf("an identifiable invalid job must settle as failed for its waiters: %v", coord.jobPublishes)
	}

	anonymous := validPoolDelivery("anonymous", "")
	anonymous.Job.ID = ""
	pool.processJob(context.Background(), anonymous)
	if recorder.count(domain.EventJobDeliveryRejected) != 2 || recorder.count(domain.EventJobFailed) != 0 {
		t.Fatalf("anonymous invalid delivery must be rejected: %v", recorder.snapshot())
	}
	if len(coord.acks) != 2 || coord.acks[1] != "anonymous" {
		t.Fatalf("anonymous invalid delivery must be acknowledged so it is not redelivered: %v", coord.acks)
	}
}

func TestPoolIgnoresDuplicateRunSilently(t *testing.T) {
	coord := &stubCoordinator{}
	recorder := newEventRecorder()
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", Observer: recorder})
	if !pool.beginJob("run", func(error) {}) {
		t.Fatal("expected first begin to start")
	}

	pool.processJob(context.Background(), validPoolDelivery("redelivery", "run"))

	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("redelivery of an active run must emit nothing: %v", events)
	}
	if len(coord.acks) != 0 || len(coord.jobPublishes) != 0 || len(coord.renews) != 0 {
		t.Fatalf("redelivery of an active run must not touch the coordinator: acks=%v statuses=%v renews=%v", coord.acks, coord.jobPublishes, coord.renews)
	}
}

type blockedRenewCoordinator struct {
	*stubCoordinator
	mu         sync.Mutex
	subscribes int
	subscribed chan context.Context
	renewing   chan struct{}
	release    chan struct{}
	renewOnce  sync.Once
	delivery   domain.JobDelivery
}

func (c *blockedRenewCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	c.mu.Lock()
	c.subscribes++
	n := c.subscribes
	c.mu.Unlock()
	ch := make(chan domain.JobDelivery, 1)
	if n == 1 {
		ch <- c.delivery
		c.subscribed <- ctx
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (c *blockedRenewCoordinator) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	c.renewOnce.Do(func() {
		close(c.renewing)
		<-c.release
	})
	return errors.New("released")
}

func TestPoolStopDoesNotBlockStart(t *testing.T) {
	coord := &blockedRenewCoordinator{
		stubCoordinator: &stubCoordinator{},
		subscribed:      make(chan context.Context, 1),
		renewing:        make(chan struct{}),
		release:         make(chan struct{}),
		delivery:        validPoolDelivery("receipt", "run"),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(coord.release) }) }
	t.Cleanup(release)
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile"})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	<-coord.renewing

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		pool.Stop()
	}()
	firstCtx := <-coord.subscribed
	<-firstCtx.Done()

	started := make(chan error, 1)
	go func() { started <- pool.Start(context.Background()) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("start while the previous run drains: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked behind a Stop that is still draining")
	}

	release()
	<-stopped
	pool.Stop()
}

func TestQueuedJobGetsFullExecutionBudget(t *testing.T) {
	pool := &Pool{jobTimeout: time.Second}
	job := validPoolDelivery("receipt", "run").Job
	job.StartBy = time.Now().Add(100 * time.Millisecond)

	ctx, cancel := pool.jobContext(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("job context has no deadline")
	}
	if remaining := time.Until(deadline); remaining < 900*time.Millisecond {
		t.Fatalf("a job started just before its start-by limit must still get the full JobTimeout, got %v", remaining)
	}
}

func TestProcessJobWorkerContextOutlivesStartBy(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	coord := &stubCoordinator{renewErr: errors.New("stop after inspecting the job context")}
	coord.onRenew = func(ctx context.Context) {
		deadline, hasDeadline = ctx.Deadline()
	}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil, PoolConfig{Profile: "profile", JobTimeout: 2 * time.Second})
	delivery := validPoolDelivery("receipt", "run")
	delivery.Job.StartBy = time.Now().Add(100 * time.Millisecond)

	pool.processJob(context.Background(), delivery)

	if !hasDeadline {
		t.Fatal("job context has no deadline")
	}
	if !deadline.After(delivery.Job.StartBy.Add(time.Second)) {
		t.Fatalf("worker deadline %v must outlive StartBy %v by the execution budget", deadline, delivery.Job.StartBy)
	}
}
