package transcode

import (
	"context"
	"errors"
	"testing"
	"time"

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
	close(ch)
	return ch, nil
}
func (s *stubCoordinator) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	s.renews = append(s.renews, receipt)
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

func TestExtractSegmentsRespectsRangeAndDuration(t *testing.T) {
	p := &Pool{}
	keyframes := []float64{0, 2, 4, 9, 15}
	segments := p.extractSegments(keyframes, 16, 1, 2)

	if len(segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segments))
	}
	if segments[0].Index != 1 || segments[0].Start != 9 || segments[0].End != 15 {
		t.Fatalf("unexpected first segment: %#v", segments[0])
	}
	if segments[1].Index != 2 || segments[1].End != 16 {
		t.Fatalf("unexpected second segment: %#v", segments[1])
	}
}

func TestFindVideoAndAudioRenditions(t *testing.T) {
	p := &Pool{}
	meta := &domain.Metadata{Video: domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000}, Audios: []domain.AudioStream{{Codec: "aac", Channels: 2, Bitrate: 128_000}, {Codec: "ac3", Channels: 6, Bitrate: 640_000}}}

	if p.findVideoRendition(meta, "720p") == nil {
		t.Fatalf("expected to find 720p rendition")
	}
	if p.findVideoRendition(meta, "unknown") != nil {
		t.Fatalf("should return nil for missing video rendition")
	}

	audio := p.findAudioRendition(meta, "audio1_aac_surround")
	if audio == nil {
		t.Fatalf("expected surround audio rendition")
	}
	if audio.StreamIndex != 1 {
		t.Fatalf("expected stream index 1, got %#v", audio)
	}
	if p.findAudioRendition(&domain.Metadata{}, "aac_stereo") != nil {
		t.Fatalf("should return nil when no audio present")
	}
}

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
		Deadline:        time.Now().Add(time.Minute),
		Segments:        []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}},
		ExpectedIndices: []int{0},
		VideoRendition:  &domain.VideoRendition{Name: "720p", Method: domain.Transcode},
		Metadata:        &domain.Metadata{},
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

func TestExtractSegmentsEmptyKeyframes(t *testing.T) {
	p := &Pool{}
	segments := p.extractSegments([]float64{}, 10, 0, 5)
	if len(segments) != 2 {
		t.Fatalf("expected 2 fallback segments, got %d", len(segments))
	}
	if segments[0].Start != 0 || segments[0].End != 6 || segments[1].Start != 6 || segments[1].End != 10 {
		t.Fatalf("unexpected fallback segments: %#v", segments)
	}
}

func TestFindAudioRenditionNotFound(t *testing.T) {
	p := &Pool{}
	meta := &domain.Metadata{Audios: []domain.AudioStream{{Codec: "ac3", Channels: 6, Bitrate: 640_000}}}
	if p.findAudioRendition(meta, "nonexistent") != nil {
		t.Fatalf("should return nil for missing audio rendition")
	}
}

func TestExtractSegmentsSingleKeyframe(t *testing.T) {
	p := &Pool{}
	keyframes := []float64{0}
	segments := p.extractSegments(keyframes, 10, 0, 0)

	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if segments[0].End != 10 {
		t.Fatalf("expected end at duration, got %v", segments[0].End)
	}
}

func TestExtractSegmentsUsesConfiguredTargetDuration(t *testing.T) {
	p := &Pool{targetDuration: 4.0}
	segments := p.extractSegments([]float64{0, 4, 8, 12}, 12, 1, 1)

	if len(segments) != 1 {
		t.Fatalf("expected one segment, got %#v", segments)
	}
	if segments[0].Start != 4 || segments[0].End != 8 {
		t.Fatalf("target duration was not respected: %#v", segments[0])
	}
}

func TestExtractSegmentsOutOfRange(t *testing.T) {
	p := &Pool{}
	keyframes := []float64{0, 6, 12}
	segments := p.extractSegments(keyframes, 15, 10, 15)

	if len(segments) != 0 {
		t.Fatalf("expected 0 segments for out-of-range indices, got %d", len(segments))
	}
}

func TestExtractSegmentsMultipleKeyframeGroups(t *testing.T) {
	p := &Pool{}
	keyframes := []float64{0, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20}
	segments := p.extractSegments(keyframes, 25, 0, 2)

	if len(segments) < 2 {
		t.Fatalf("expected at least 2 segments, got %d", len(segments))
	}
}

func TestWorkerSetError(t *testing.T) {
	w := &Worker{}
	w.setError(assertErr("test error"))
	if w.Err() == nil || w.Err().Error() != "test error" {
		t.Fatalf("expected error to be set, got %v", w.Err())
	}
	if w.State() != WorkerStateError {
		t.Fatalf("expected error state, got %v", w.State())
	}
}

func TestWaitForWorkerReturnsWorkerError(t *testing.T) {
	p := &Pool{}
	w := &Worker{}
	w.setError(assertErr("ffmpeg failed"))

	err := p.waitForWorker(context.Background(), w)
	if err == nil || err.Error() != "ffmpeg failed" {
		t.Fatalf("expected worker error, got %v", err)
	}
}

func TestPoolProcessJobSkipsJobAlreadyRunning(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil)
	if started := pool.beginJob("run", func(error) {}); !started {
		t.Fatal("expected first begin to start")
	}

	pool.processJob(context.Background(), domain.JobDelivery{Job: domain.Job{ID: "job", RunID: "run"}, Receipt: "receipt"})

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
			Deadline:        time.Now().Add(time.Minute),
			Segments:        []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}},
			ExpectedIndices: []int{0},
			VideoRendition:  &domain.VideoRendition{Name: "720p", Method: domain.Transcode},
			Metadata:        &domain.Metadata{},
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
			Deadline:        time.Now().Add(-time.Second),
			Segments:        []domain.Segment{{Index: 0, Start: 0, End: 6, Duration: 6}},
			ExpectedIndices: []int{0},
			VideoRendition:  &domain.VideoRendition{Name: "720p", Method: domain.Transcode},
			Metadata:        &domain.Metadata{},
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
