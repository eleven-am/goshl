package segment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/google/uuid"
)

type stubStorage struct {
	err    error
	writes []domain.SegmentData
}

func (s *stubStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}
func (s *stubStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SetMetadata(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (s *stubStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	s.writes = append(s.writes, info)
	return s.err
}
func (s *stubStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteSprite(ctx context.Context, mediaID string, index int, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSprite(ctx context.Context, mediaID string, index int) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SpriteExists(ctx context.Context, mediaID string, index int) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteSpriteVTT(ctx context.Context, mediaID string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSpriteVTT(ctx context.Context, mediaID string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SpriteVTTExists(ctx context.Context, mediaID string) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSubtitleVTT(ctx context.Context, mediaID string, lang string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	return false, nil
}
func (s *stubStorage) WritePreview(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadPreview(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) PreviewExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}
func (s *stubStorage) WriteThumbnail(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (s *stubStorage) ReadThumbnail(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) ThumbnailExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}

type stubPubSub struct {
	publishes []struct {
		info   domain.SegmentData
		status domain.SegmentStatus
	}
	err error
}

func (p *stubPubSub) Enqueue(ctx context.Context, job domain.Job) error {
	return nil
}
func (p *stubPubSub) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	return nil, nil
}
func (p *stubPubSub) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	return nil
}
func (p *stubPubSub) Ack(ctx context.Context, receipt string) error {
	return nil
}
func (p *stubPubSub) SettleJob(ctx context.Context, receipt string, status domain.JobStatus) error {
	return nil
}
func (p *stubPubSub) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	return make(chan domain.JobStatus), nil
}
func (p *stubPubSub) NotifySegment(ctx context.Context, info domain.SegmentData, status domain.SegmentStatus) error {
	p.publishes = append(p.publishes, struct {
		info   domain.SegmentData
		status domain.SegmentStatus
	}{info, status})
	return p.err
}
func (p *stubPubSub) WaitSegment(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error) {
	return nil, nil
}
func (p *stubPubSub) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	return "token", runID, nil
}
func (p *stubPubSub) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	return nil
}
func (p *stubPubSub) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return uuid.New().String(), nil
}
func (p *stubPubSub) ReleaseLock(ctx context.Context, key string, token string) error {
	return nil
}
func (p *stubPubSub) Close() {}

func TestNotifyingStoragePublishesReadyOnSuccess(t *testing.T) {
	storage := &stubStorage{}
	pubsub := &stubPubSub{}
	n := NewNotifyingStorage(storage, pubsub)

	info := domain.SegmentData{Index: 1, Rendition: "1080p", IsVideo: true}
	if err := n.WriteSegment(context.Background(), info, []byte("abc")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pubsub.publishes) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(pubsub.publishes))
	}
	publish := pubsub.publishes[0]
	if publish.info != info {
		t.Fatalf("wrong segment info: %v", publish.info)
	}
	if publish.status.State != domain.SegmentStateReady {
		t.Fatalf("expected ready state, got %v", publish.status.State)
	}
}

func TestNotifyingStorageLeavesFailureToRunScopedJobStatus(t *testing.T) {
	storage := &stubStorage{err: errors.New("boom")}
	pubsub := &stubPubSub{}
	n := NewNotifyingStorage(storage, pubsub)

	info := domain.SegmentData{Index: 2, Rendition: "aac", IsVideo: false}
	err := n.WriteSegment(context.Background(), info, nil)
	if err == nil {
		t.Fatalf("expected error")
	}

	if len(pubsub.publishes) != 0 {
		t.Fatalf("write failures must not publish unscoped segment status: %#v", pubsub.publishes)
	}
}

func TestNotifyingStorageKeepsDurableSuccessWhenPublishFails(t *testing.T) {
	storage := &stubStorage{}
	pubsub := &stubPubSub{err: errors.New("pubsub err")}
	var events []domain.Event
	n := NewNotifyingStorage(storage, pubsub, domain.ObserverFunc(func(ctx context.Context, event domain.Event) {
		events = append(events, event)
	}))

	err := n.WriteSegment(context.Background(), domain.SegmentData{}, nil)
	if err != nil {
		t.Fatalf("durable write must remain successful: %v", err)
	}
	if len(pubsub.publishes) != 3 {
		t.Fatalf("expected three notification attempts, got %d", len(pubsub.publishes))
	}
	if len(events) != 1 || events[0].Type != domain.EventSegmentNotifyFailed {
		t.Fatalf("expected notification failure event, got %#v", events)
	}
}

func TestNotifyingStoragePassthroughMethods(t *testing.T) {
	storage := &stubStorage{}
	pubsub := &stubPubSub{}
	n := NewNotifyingStorage(storage, pubsub)
	ctx := context.Background()

	if _, err := n.MetadataExists(ctx, "src"); err != nil {
		t.Fatalf("MetadataExists: %v", err)
	}
	if _, err := n.GetMetadata(ctx, "src"); err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if err := n.SetMetadata(ctx, "src", []byte("data")); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if _, err := n.ReadSegment(ctx, domain.SegmentData{}); err != nil {
		t.Fatalf("ReadSegment: %v", err)
	}
	if _, err := n.SegmentExists(ctx, domain.SegmentData{}); err != nil {
		t.Fatalf("SegmentExists: %v", err)
	}
	if err := n.WriteSprite(ctx, "src", 0, []byte("data")); err != nil {
		t.Fatalf("WriteSprite: %v", err)
	}
	if _, err := n.ReadSprite(ctx, "src", 0); err != nil {
		t.Fatalf("ReadSprite: %v", err)
	}
	if _, err := n.SpriteExists(ctx, "src", 0); err != nil {
		t.Fatalf("SpriteExists: %v", err)
	}
	if err := n.WriteSpriteVTT(ctx, "src", []byte("data")); err != nil {
		t.Fatalf("WriteSpriteVTT: %v", err)
	}
	if _, err := n.ReadSpriteVTT(ctx, "src"); err != nil {
		t.Fatalf("ReadSpriteVTT: %v", err)
	}
	if _, err := n.SpriteVTTExists(ctx, "src"); err != nil {
		t.Fatalf("SpriteVTTExists: %v", err)
	}
	if err := n.WriteSubtitleVTT(ctx, "src", "en", []byte("data")); err != nil {
		t.Fatalf("WriteSubtitleVTT: %v", err)
	}
	if _, err := n.ReadSubtitleVTT(ctx, "src", "en"); err != nil {
		t.Fatalf("ReadSubtitleVTT: %v", err)
	}
	if _, err := n.SubtitleVTTExists(ctx, "src", "en"); err != nil {
		t.Fatalf("SubtitleVTTExists: %v", err)
	}
	if err := n.WritePreview(ctx, "src", []byte("data")); err != nil {
		t.Fatalf("WritePreview: %v", err)
	}
	if _, err := n.ReadPreview(ctx, "src"); err != nil {
		t.Fatalf("ReadPreview: %v", err)
	}
	if _, err := n.PreviewExists(ctx, "src"); err != nil {
		t.Fatalf("PreviewExists: %v", err)
	}
	if err := n.WriteThumbnail(ctx, "src", []byte("data")); err != nil {
		t.Fatalf("WriteThumbnail: %v", err)
	}
	if _, err := n.ReadThumbnail(ctx, "src"); err != nil {
		t.Fatalf("ReadThumbnail: %v", err)
	}
	if _, err := n.ThumbnailExists(ctx, "src"); err != nil {
		t.Fatalf("ThumbnailExists: %v", err)
	}
}
