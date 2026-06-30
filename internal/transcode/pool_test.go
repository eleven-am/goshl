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
	publishes []domain.SegmentStatus
	subErr    error
	acks      []string
}

func (s *stubCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	return nil
}
func (s *stubCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType) (<-chan domain.Job, error) {
	if s.subErr != nil {
		return nil, s.subErr
	}
	ch := make(chan domain.Job)
	close(ch)
	return ch, nil
}
func (s *stubCoordinator) Ack(ctx context.Context, jobID string) error {
	s.acks = append(s.acks, jobID)
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

func TestFindNearestKeyframePrefersPreviousWhenVeryClose(t *testing.T) {
	keyframes := []float64{5, 10, 15}
	got := findNearestKeyframe(keyframes, 10.005)
	if got != 5 {
		t.Fatalf("expected fallback to previous keyframe, got %v", got)
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

func TestPublishErrorSendsRange(t *testing.T) {
	coord := &stubCoordinator{}
	p := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	job := domain.Job{Rendition: "1080p", StartIndex: 2, EndIndex: 4}

	p.publishError(context.Background(), job, assertErr("boom"))

	if len(coord.publishes) != 3 {
		t.Fatalf("expected publish per segment, got %d", len(coord.publishes))
	}
	for _, status := range coord.publishes {
		if status.State != domain.SegmentStateError || status.Error == "" {
			t.Fatalf("status missing error: %#v", status)
		}
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

func TestPoolStartAndStop(t *testing.T) {
	coord := &stubCoordinator{}
	pool := NewPool(coord, 1, domain.StreamVideo, 6.0, nil, nil, nil)

	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("start error: %v", err)
	}

	pool.Stop()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("restart after stop should work: %v", err)
	}
	pool.Stop()
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

func TestFindNearestKeyframeEmptySlice(t *testing.T) {
	got := findNearestKeyframe([]float64{}, 5.0)
	if got != 0 {
		t.Fatalf("expected 0 for empty keyframes, got %v", got)
	}
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

func TestFindNearestKeyframeExactMatch(t *testing.T) {
	keyframes := []float64{0, 5, 10, 15}
	got := findNearestKeyframe(keyframes, 10.5)
	if got != 10 {
		t.Fatalf("expected 10 for target 10.5, got %v", got)
	}
}

func TestFindNearestKeyframeBetweenKeys(t *testing.T) {
	keyframes := []float64{0, 5, 10, 15}
	got := findNearestKeyframe(keyframes, 7)
	if got != 5 {
		t.Fatalf("expected 5 for target 7, got %v", got)
	}
}

func TestFindNearestKeyframeTargetPastAll(t *testing.T) {
	keyframes := []float64{0, 5, 10}
	got := findNearestKeyframe(keyframes, 20)
	if got != 10 {
		t.Fatalf("expected 10 for target past all, got %v", got)
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
