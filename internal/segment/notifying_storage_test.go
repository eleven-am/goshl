package segment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
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
func (p *stubPubSub) Subscribe(ctx context.Context, streamType domain.StreamType) (<-chan domain.Job, error) {
	return nil, nil
}
func (p *stubPubSub) Ack(ctx context.Context, jobID string) error {
	return nil
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

func TestNotifyingStoragePublishesErrorWhenStorageFails(t *testing.T) {
	storage := &stubStorage{err: errors.New("boom")}
	pubsub := &stubPubSub{}
	n := NewNotifyingStorage(storage, pubsub)

	info := domain.SegmentData{Index: 2, Rendition: "aac", IsVideo: false}
	err := n.WriteSegment(context.Background(), info, nil)
	if err == nil {
		t.Fatalf("expected error")
	}

	if len(pubsub.publishes) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(pubsub.publishes))
	}
	pub := pubsub.publishes[0]
	if pub.status.State != domain.SegmentStateError || pub.status.Error == "" {
		t.Fatalf("expected error status, got %#v", pub.status)
	}
}

func TestNotifyingStorageReturnsErrorWhenPublishFails(t *testing.T) {
	storage := &stubStorage{}
	pubsub := &stubPubSub{err: errors.New("pubsub err")}
	n := NewNotifyingStorage(storage, pubsub)

	err := n.WriteSegment(context.Background(), domain.SegmentData{}, nil)
	if err == nil || !strings.Contains(err.Error(), "pubsub") {
		t.Fatalf("expected publish error, got %v", err)
	}
}
