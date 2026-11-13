package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestSegmentReturnsCachedData(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 10, Keyframes: []float64{0, 6, 10}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{3: []byte("ok")}}
	svc := mustNew(Config{
		Storage:     store,
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)
	if err != nil {
		t.Fatalf("segment err: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("unexpected data %q", data)
	}
}

func TestSegmentEnqueuesWhenMissingAndUsesPubSubReady(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	meta.Audios = []domain.AudioStream{{Index: 1, Codec: "aac", Channels: 2, Bitrate: 128_000, Default: true}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{0: []byte("seg0")}}
	coord := &stubCoordinator{}
	svc := mustNew(Config{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamAudio, "aac_stereo", 1)
	if err != nil {
		t.Fatalf("segment err: %v", err)
	}
	if data != nil {
		t.Fatalf("stub storage has no segment 1, got %q", data)
	}
	if len(coord.enqueued) != 1 {
		t.Fatalf("expected enqueue, got %d", len(coord.enqueued))
	}
	if coord.enqueued[0].StartIndex != 1 || coord.enqueued[0].EndIndex != 1 {
		t.Fatalf("unexpected job indices: %#v", coord.enqueued[0])
	}
	if coord.enqueued[0].VideoStreamIndex != meta.Video.Index {
		t.Fatalf("enqueued job must carry the video stream index %d, got %d", meta.Video.Index, coord.enqueued[0].VideoStreamIndex)
	}
	if overlap := coord.enqueued[0].Overlap; overlap == nil || overlap.Index != 0 || coord.enqueued[0].Trailing != nil {
		t.Fatalf("enqueued audio job must carry segment 0 as overlap and no trailing segment: %#v / %#v", overlap, coord.enqueued[0].Trailing)
	}
}

func TestSegmentAcquiresAndReleasesLock(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{}
	svc := mustNew(Config{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)

	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected 1 lock call, got %d", len(coord.lockCalls))
	}
	if !strings.HasPrefix(coord.lockCalls[0], "job:") {
		t.Fatalf("expected canonical job lock key, got %q", coord.lockCalls[0])
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("lock must be held while the job runs, got %d releases", len(coord.releaseCalls))
	}
	svc.finishFlightRun(coord.enqueued[0].ID, coord.enqueued[0].RunID)
	if len(coord.releaseCalls) != 1 {
		t.Fatalf("expected 1 release call after job finished, got %d", len(coord.releaseCalls))
	}
}

func TestSegmentWaitsWhenLockHeld(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld}
	svc := mustNew(Config{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)

	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected 1 lock attempt, got %d", len(coord.lockCalls))
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("expected 0 release calls when lock not acquired, got %d", len(coord.releaseCalls))
	}
	if len(coord.enqueued) != 0 {
		t.Fatalf("expected no enqueue when lock held, got %d", len(coord.enqueued))
	}
}

func TestSegmentReturnsCoordinatorLockFailure(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: errors.New("coordinator down")}
	svc := mustNew(Config{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	_, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err == nil || !strings.Contains(err.Error(), "acquire segment lock") {
		t.Fatalf("expected lock failure, got %v", err)
	}
}

func TestSegmentDoubleCheckReturnsExistingAfterLock(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}

	lockAcquired := false
	coord := &stubCoordinator{
		onAcquireLock: func(key string) {
			lockAcquired = true
			store.setSegment(1, []byte("appeared"))
		},
	}

	svc := mustNew(Config{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !lockAcquired {
		t.Fatalf("lock should have been acquired")
	}
	if string(data) != "appeared" {
		t.Fatalf("expected segment data from double-check, got %q", data)
	}
	if len(coord.enqueued) != 0 {
		t.Fatalf("should not enqueue when segment exists after double-check, got %d", len(coord.enqueued))
	}
	if store.segmentExistsCalls != 2 {
		t.Fatalf("expected 2 segment exists calls (initial + double-check), got %d", store.segmentExistsCalls)
	}
}

func TestSegmentSkipsLockWhenAlreadyExists(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{3: []byte("cached")}}
	coord := &stubCoordinator{}
	svc := mustNew(Config{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "cached" {
		t.Fatalf("expected cached data, got %q", data)
	}

	if len(coord.lockCalls) != 0 {
		t.Fatalf("expected no lock calls when segment exists, got %d", len(coord.lockCalls))
	}
}

func TestSegmentRechecksStorageAfterWaitRegistration(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}, Video: domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, PixelFormat: "yuv420p", Profile: "High", Level: 40}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld}
	coord.onWaitSegment = func(info domain.SegmentData) {
		store.setSegment(info.Index, []byte("ready-before-subscription"))
	}
	svc := mustNew(Config{Storage: store, Coordinator: coord, PathGen: stubPathGen{}, SegmentTimeout: 20 * time.Millisecond})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("expected storage recheck to recover missed notification: %v", err)
	}
	if string(data) != "ready-before-subscription" {
		t.Fatalf("unexpected segment %q", data)
	}
}

func TestSegmentPollsDurableStorageWhenNotificationIsLost(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld}
	svc := mustNew(Config{Storage: store, Coordinator: coord, PathGen: stubPathGen{}, SegmentTimeout: time.Second})

	go func() {
		time.Sleep(150 * time.Millisecond)
		store.setSegment(1, []byte("durable-without-notification"))
	}()

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("expected durable polling to recover notification loss: %v", err)
	}
	if string(data) != "durable-without-notification" {
		t.Fatalf("unexpected segment %q", data)
	}
}

func TestSegmentContinuesPollingAfterNotificationChannelCloses(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	waitCh := make(chan domain.SegmentStatus)
	close(waitCh)
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld, waitCh: waitCh}
	svc := mustNew(Config{Storage: store, Coordinator: coord, PathGen: stubPathGen{}, SegmentTimeout: time.Second})

	go func() {
		time.Sleep(150 * time.Millisecond)
		store.setSegment(1, []byte("ready-after-close"))
	}()

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("expected polling after notification close: %v", err)
	}
	if string(data) != "ready-after-close" {
		t.Fatalf("unexpected segment %q", data)
	}
}

func seekTestController(t *testing.T, store *stubStorage, coord *stubCoordinator, opts Config) *Engine {
	t.Helper()
	meta := &domain.Metadata{Duration: 60, Keyframes: []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54, 60}, Video: domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, PixelFormat: "yuv420p", Profile: "High", Level: 40}}
	metaBytes, _ := json.Marshal(meta)
	store.metaData = metaBytes
	store.metaExists = true
	if store.segments == nil {
		store.segments = map[int][]byte{}
	}
	opts.Storage = store
	opts.Coordinator = coord
	opts.PathGen = stubPathGen{}
	return mustNew(opts)
}

func TestSegmentSequentialRequestsInsideOneJobEnqueueOnce(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Config{})

	for index := 0; index < 4; index++ {
		coord.waitCh = make(chan domain.SegmentStatus, 1)
		coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}
		_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", index)
	}

	if len(coord.enqueued) != 1 {
		t.Fatalf("expected exactly 1 enqueue for 4 requests in one job range, got %d", len(coord.enqueued))
	}
	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected exactly 1 lock acquisition, got %d", len(coord.lockCalls))
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("lock must be held until the job finishes, got %d releases", len(coord.releaseCalls))
	}
	svc.finishFlightRun(coord.enqueued[0].ID, coord.enqueued[0].RunID)
	if len(coord.releaseCalls) != 1 {
		t.Fatalf("expected the lock released when the job finished, got %d", len(coord.releaseCalls))
	}
}

func TestSegmentEnqueuesAgainAfterJobFinished(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Config{})

	for round := 0; round < 2; round++ {
		coord.waitCh = make(chan domain.SegmentStatus, 1)
		coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}
		_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
		svc.finishFlightRun(coord.enqueued[round].ID, coord.enqueued[round].RunID)
	}

	if len(coord.enqueued) != 2 {
		t.Fatalf("expected a fresh enqueue after the first job finished, got %d", len(coord.enqueued))
	}
}

func TestSegmentAnchorsJobAtRequestedIndex(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Config{})
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)

	if len(coord.enqueued) != 1 {
		t.Fatalf("expected 1 enqueue, got %d", len(coord.enqueued))
	}
	job := coord.enqueued[0]
	if job.StartIndex != 3 || job.EndIndex != 6 || job.RequestedIndex != 3 {
		t.Fatalf("job must start at the requested segment, got %d-%d requested %d", job.StartIndex, job.EndIndex, job.RequestedIndex)
	}
	if job.Segments[0].Index != 3 || len(job.ExpectedIndices) != 4 {
		t.Fatalf("unexpected job segments: %#v", job.ExpectedIndices)
	}
}

func TestSegmentJobStopsBeforeExistingSegments(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	store := &stubStorage{segments: map[int][]byte{5: []byte("cached")}}
	svc := seekTestController(t, store, coord, Config{})
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)

	job := coord.enqueued[0]
	if job.StartIndex != 3 || job.EndIndex != 4 {
		t.Fatalf("job must not overlap existing segment 5, got %d-%d", job.StartIndex, job.EndIndex)
	}
}

func TestSegmentJobStopsBeforeNeighbouringFlight(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Config{})

	for _, index := range []int{5, 3} {
		coord.waitCh = make(chan domain.SegmentStatus, 1)
		coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}
		_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", index)
	}

	if len(coord.enqueued) != 2 {
		t.Fatalf("expected 2 enqueues, got %d", len(coord.enqueued))
	}
	if coord.enqueued[0].StartIndex != 5 || coord.enqueued[0].EndIndex != 8 {
		t.Fatalf("unexpected first job: %d-%d", coord.enqueued[0].StartIndex, coord.enqueued[0].EndIndex)
	}
	if coord.enqueued[1].StartIndex != 3 || coord.enqueued[1].EndIndex != 4 {
		t.Fatalf("second job must stop before the in-flight job, got %d-%d", coord.enqueued[1].StartIndex, coord.enqueued[1].EndIndex)
	}
}

func waitForEnqueued(t *testing.T, coord *stubCoordinator, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		coord.mu.Lock()
		got := len(coord.enqueued)
		coord.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d enqueued jobs, got %d", want, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type joinRecorder struct {
	joined chan struct{}
	ready  chan domain.Event
}

func newJoinRecorder() *joinRecorder {
	return &joinRecorder{joined: make(chan struct{}, 8), ready: make(chan domain.Event, 8)}
}

func (r *joinRecorder) observer() domain.Observer {
	return domain.ObserverFunc(func(_ context.Context, event domain.Event) {
		switch event.Type {
		case domain.EventJobJoined:
			r.joined <- struct{}{}
		case domain.EventSegmentReady:
			r.ready <- event
		}
	})
}

type segmentResult struct {
	data []byte
	err  error
}

func startSegment(ctx context.Context, svc *Engine, index int) <-chan segmentResult {
	done := make(chan segmentResult, 1)
	go func() {
		data, err := svc.Segment(ctx, "file:///media", domain.StreamVideo, "1080p", index)
		done <- segmentResult{data, err}
	}()
	return done
}

func cancelFirstWaiter(t *testing.T, svc *Engine, coord *stubCoordinator) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := startSegment(ctx, svc, 2)
	waitForEnqueued(t, coord, 1)
	cancel()
	if res := <-done; !errors.Is(res.err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", res.err)
	}
}

func awaitJoined(t *testing.T, rec *joinRecorder, done <-chan segmentResult) {
	t.Helper()
	select {
	case <-rec.joined:
	case res := <-done:
		t.Fatalf("request returned without joining the cluster job: data=%q err=%v", res.data, res.err)
	case <-time.After(3 * time.Second):
		t.Fatal("request never joined the cluster job")
	}
}

func TestSegmentKeepsClusterJobAfterLocalWaiterLeaves(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	store := &stubStorage{}
	coord := &stubCoordinator{}
	rec := newJoinRecorder()
	svc := seekTestController(t, store, coord, Config{Observer: rec.observer()})

	cancelFirstWaiter(t, svc, coord)

	coord.mu.Lock()
	enqueued := len(coord.enqueued)
	released := len(coord.releaseCalls)
	coord.mu.Unlock()
	if enqueued != 1 {
		t.Fatalf("local waiter departure must leave the single cluster job in place, got %d", enqueued)
	}
	if released != 0 {
		t.Fatalf("local waiter departure must not release the cluster job lock, got %d releases", released)
	}

	followUp := startSegment(context.Background(), svc, 2)
	awaitJoined(t, rec, followUp)
	store.setSegment(2, []byte("segment-2"))
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	select {
	case res := <-followUp:
		if res.err != nil || string(res.data) != "segment-2" {
			t.Fatalf("follow-up request: data=%q err=%v", res.data, res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow-up request was never served")
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if len(coord.enqueued) != 1 || len(coord.releaseCalls) != 0 {
		t.Fatalf("follow-up must reuse the job: enqueued=%d releases=%v", len(coord.enqueued), coord.releaseCalls)
	}
}

func TestSegmentRejoinKeepsExistingClusterJob(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	store := &stubStorage{}
	coord := &stubCoordinator{}
	rec := newJoinRecorder()
	svc := seekTestController(t, store, coord, Config{Observer: rec.observer()})

	cancelFirstWaiter(t, svc, coord)

	second := startSegment(context.Background(), svc, 3)
	awaitJoined(t, rec, second)
	store.setSegment(3, []byte("segment-3"))
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	select {
	case res := <-second:
		if res.err != nil {
			t.Fatalf("rejoined request failed: %v", res.err)
		}
		if string(res.data) != "segment-3" {
			t.Fatalf("expected rejoined request to be served segment 3, got %q", res.data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rejoined request was never served")
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if len(coord.enqueued) != 1 {
		t.Fatalf("rejoin must not enqueue again, got %d", len(coord.enqueued))
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("rejoin must not release the cluster job lock, got %v", coord.releaseCalls)
	}
}

func TestSegmentParkedWaiterIsServedByReadyNotification(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	polled := make(chan struct{}, 64)
	store := &stubStorage{hideSegments: true, onSegmentExists: func() {
		select {
		case polled <- struct{}{}:
		default:
		}
	}}
	coord := &stubCoordinator{}
	rec := newJoinRecorder()
	svc := seekTestController(t, store, coord, Config{Observer: rec.observer(), SegmentTimeout: 10 * time.Second})

	done := startSegment(context.Background(), svc, 1)
	waitForEnqueued(t, coord, 1)
	for len(polled) > 0 {
		<-polled
	}
	select {
	case <-polled:
	case res := <-done:
		t.Fatalf("waiter returned before any notification: data=%q err=%v", res.data, res.err)
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never polled storage, so it is not parked")
	}

	store.setSegment(1, []byte("late-segment"))
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	select {
	case res := <-done:
		if res.err != nil || string(res.data) != "late-segment" {
			t.Fatalf("parked waiter: data=%q err=%v", res.data, res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parked waiter was not served by the ready notification")
	}
	select {
	case event := <-rec.ready:
		if event.Index != 1 || event.Bytes != int64(len("late-segment")) {
			t.Fatalf("unexpected ready event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected domain.EventSegmentReady for the notified segment")
	}
}

func TestSegmentRejectsInvalidRequestBeforeQueueing(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}, Video: domain.VideoStream{Width: 1920, Height: 1080}}
	metaBytes, _ := json.Marshal(meta)
	coord := &stubCoordinator{}
	svc := mustNew(Config{Storage: &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}, Coordinator: coord, PathGen: stubPathGen{}})

	if _, err := svc.Segment(context.Background(), "file:///media", domain.StreamType("bogus"), "1080p", 0); !errors.Is(err, ErrInvalidStreamType) {
		t.Fatalf("expected invalid stream type, got %v", err)
	}
	if _, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", -1); !errors.Is(err, ErrSegmentOutOfRange) {
		t.Fatalf("expected segment range error, got %v", err)
	}
	if len(coord.enqueued) != 0 {
		t.Fatalf("invalid requests must not enqueue work: %#v", coord.enqueued)
	}
}

func TestBuildJobOverlapAndTrailingRules(t *testing.T) {
	controller := &Engine{opts: Config{JobTimeout: time.Minute}, pipelineVersion: "test"}
	segments := []domain.Segment{
		{Index: 0, Start: 0, End: 6},
		{Index: 1, Start: 6, End: 12},
		{Index: 2, Start: 12, End: 18},
		{Index: 3, Start: 18, End: 24},
	}
	direct := domain.VideoRendition{Name: "source", Method: domain.DirectStream}
	transcoded := domain.VideoRendition{Name: "720p", Method: domain.Transcode}
	stereo := domain.AudioRendition{Name: "aac_stereo", Method: domain.Transcode}
	passthrough := domain.AudioRendition{Name: "ac3_passthrough", Method: domain.DirectStream}
	deadline := time.Now().Add(time.Minute)
	meta := testVideoMetadata(24, []float64{0, 6, 12, 18})
	build := func(stream domain.StreamType, video *domain.VideoRendition, audio *domain.AudioRendition, start, end int) domain.Job {
		name := ""
		if video != nil {
			name = video.Name
		} else {
			name = audio.Name
		}
		return controller.buildJob("run", deadline, "source", "key", stream, name, start, start, end, segments, video, audio, meta)
	}

	middle := build(domain.StreamVideo, &direct, nil, 1, 2)
	if middle.Overlap == nil || *middle.Overlap != segments[0] {
		t.Fatalf("copied video must carry the preceding segment as overlap, got %+v", middle.Overlap)
	}
	if middle.Trailing == nil || *middle.Trailing != segments[3] {
		t.Fatalf("copied video must carry the following segment as trailing, got %+v", middle.Trailing)
	}
	first := build(domain.StreamVideo, &direct, nil, 0, 1)
	if first.Overlap != nil || first.Trailing == nil || *first.Trailing != segments[2] {
		t.Fatalf("copied video from segment 0 has no overlap but a trailing segment, got %+v / %+v", first.Overlap, first.Trailing)
	}
	last := build(domain.StreamVideo, &direct, nil, 2, 3)
	if last.Overlap == nil || last.Trailing != nil {
		t.Fatalf("copied video ending at the last segment has no trailing segment, got %+v / %+v", last.Overlap, last.Trailing)
	}

	encoded := build(domain.StreamVideo, &transcoded, nil, 1, 2)
	if encoded.Overlap != nil || encoded.Trailing != nil {
		t.Fatalf("transcoded video must not process discarded neighbours, got %+v / %+v", encoded.Overlap, encoded.Trailing)
	}

	for _, audio := range []domain.AudioRendition{stereo, passthrough} {
		audio := audio
		job := build(domain.StreamAudio, nil, &audio, 1, 2)
		if job.Overlap == nil || *job.Overlap != segments[0] {
			t.Fatalf("%s must carry the preceding segment as overlap, got %+v", audio.Name, job.Overlap)
		}
		if job.Trailing == nil || *job.Trailing != segments[3] {
			t.Fatalf("%s must carry the following segment as trailing, got %+v", audio.Name, job.Trailing)
		}
		if start := build(domain.StreamAudio, nil, &audio, 0, 1); start.Overlap != nil || start.Trailing == nil || *start.Trailing != segments[2] {
			t.Fatalf("%s from segment 0 has no overlap but a trailing segment, got %+v / %+v", audio.Name, start.Overlap, start.Trailing)
		}
		if end := build(domain.StreamAudio, nil, &audio, 2, 3); end.Overlap == nil || end.Trailing != nil {
			t.Fatalf("%s ending at the last segment has no trailing segment, got %+v / %+v", audio.Name, end.Overlap, end.Trailing)
		}
	}
}

func TestBuildJobCarriesOnlyVideoStreamIndex(t *testing.T) {
	controller := &Engine{opts: Config{JobTimeout: time.Minute}, pipelineVersion: "test"}
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6}}
	video := domain.VideoRendition{Name: "720p", Method: domain.Transcode}
	meta := testVideoMetadata(6, []float64{0, 1.5, 3, 4.5})
	meta.Video.Index = 3
	job := controller.buildJob("run", time.Now().Add(time.Minute), "source", "key", domain.StreamVideo, video.Name, 0, 0, 0, segments, &video, nil, meta)
	if job.VideoStreamIndex != 3 {
		t.Fatalf("job must carry the video stream index 3, got %d", job.VideoStreamIndex)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	for _, field := range []string{"Keyframes", "Metadata", "Subtitles"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("job message must not carry probe metadata (%s): %s", field, encoded)
		}
	}
}

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
	requester := mustNew(Config{
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

func TestTranscodeDeadlineIgnoresProbeTimeout(t *testing.T) {
	controller := &Engine{opts: Config{JobTimeout: time.Minute, ProbeTimeout: 10 * time.Minute}, pipelineVersion: "test"}
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6}}
	video := domain.VideoRendition{Name: "720p", Method: domain.Transcode}
	job := controller.buildJob("run", time.Now().Add(time.Minute), "source", "key", domain.StreamVideo, video.Name, 0, 0, 0, segments, &video, nil, testVideoMetadata(6, []float64{0}))
	if remaining := time.Until(job.StartBy); remaining > time.Minute || remaining < 50*time.Second {
		t.Fatalf("expected deadline near JobTimeout, got %v", remaining)
	}
}

type segTestStorage struct {
	domain.Storage
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
	domain.Coordinator
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

func newSegTestController(t *testing.T, store *segTestStorage, coord *segTestCoordinator, mutate func(*Config)) *Engine {
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
	opts := Config{
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
	ctl, err := New(ctx, opts)
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

	_, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0)
	if err == nil || !strings.Contains(err.Error(), "queue down") {
		t.Fatalf("first call error = %v, want enqueue failure", err)
	}
	select {
	case <-coord.released:
	case <-time.After(2 * time.Second):
		t.Fatal("job lock was not released after enqueue failure")
	}

	_, err = ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0)
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

	if _, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("error = %v, want enqueue error", err)
	}
	select {
	case id := <-coord.released:
		t.Fatalf("lock %s released after ambiguous enqueue error", id)
	case <-time.After(100 * time.Millisecond):
	}
	_, _ = ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0)
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

	if _, err := ctl.Segment(ctx, "/x.mkv", domain.StreamVideo, "720p", 0); err == nil {
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
		return "", "other-run", domain.ErrLockHeld
	}
	joined := make(chan struct{}, 4)
	ctl := newSegTestController(t, store, coord, func(o *Config) {
		o.SegmentTimeout = 5 * time.Second
		o.Observer = domain.ObserverFunc(func(_ context.Context, event domain.Event) {
			if event.Type == domain.EventJobJoined {
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
		data, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0)
		creator <- result{data, err}
	}()
	<-entered
	go func() {
		data, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 1)
		joiner <- result{data, err}
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("joiner did not join the creator flight")
	}
	close(gate)

	select {
	case runID := <-coord.waitJobs:
		if runID != "other-run" {
			t.Fatalf("waited on run %q, want other-run", runID)
		}
	case r := <-joiner:
		t.Fatalf("joiner returned early: data=%q err=%v", r.data, r.err)
	case <-time.After(3 * time.Second):
		t.Fatal("creator did not follow the active run")
	}
	select {
	case r := <-joiner:
		t.Fatalf("joiner returned before the remote run delivered: data=%q err=%v", r.data, r.err)
	case r := <-creator:
		t.Fatalf("creator returned before the remote run delivered: data=%q err=%v", r.data, r.err)
	default:
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
	ctl := newSegTestController(t, store, coord, func(o *Config) {
		o.SegmentTimeout = 10 * time.Second
		o.JobTimeout = 150 * time.Millisecond
	})

	_, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0)
	if !errors.Is(err, ErrSegmentTimeout) {
		t.Fatalf("error = %v, want ErrSegmentTimeout", err)
	}
}

func TestSegmentRejectsUnsafeRenditionNameBeforeStorage(t *testing.T) {
	for _, name := range []string{"../../../etc/passwd", "a/b", "", ".hidden", "a..b"} {
		store := &segTestStorage{}
		ctl := newSegTestController(t, store, newSegTestCoordinator(), nil)
		_, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, name, 0)
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
	ctl := newSegTestController(t, store, coord, func(o *Config) {
		o.SegmentTimeout = 6 * time.Second
	})

	_, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0)
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
	newProbe := func(t *testing.T) (*Engine, *jobWatchProbeCoordinator) {
		coord := &jobWatchProbeCoordinator{segTestCoordinator: newSegTestCoordinator(), watches: make(chan jobWatchProbe, 4)}
		ctl := newSegTestController(t, &segTestStorage{}, coord.segTestCoordinator, func(o *Config) {
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
			_, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0)
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

		if _, err := ctl.Segment(context.Background(), "/x.mkv", domain.StreamVideo, "720p", 0); err == nil || !strings.Contains(err.Error(), "queue down") {
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
