package segment

import (
	"context"
	"fmt"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type NotifyingStorage struct {
	storage     domain.Storage
	coordinator domain.Coordinator
	observer    domain.Observer
}

func NewNotifyingStorage(storage domain.Storage, coordinator domain.Coordinator, observers ...domain.Observer) *NotifyingStorage {
	result := &NotifyingStorage{
		storage:     storage,
		coordinator: coordinator,
	}
	if len(observers) > 0 {
		result.observer = observers[0]
	}
	return result
}

func (s *NotifyingStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	return s.storage.MetadataExists(ctx, sourceURL)
}

func (s *NotifyingStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.storage.GetMetadata(ctx, sourceURL)
}

func (s *NotifyingStorage) SetMetadata(ctx context.Context, sourceURL string, data []byte) error {
	return s.storage.SetMetadata(ctx, sourceURL, data)
}

func (s *NotifyingStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	if err := s.storage.WriteSegment(ctx, info, data); err != nil {
		status := domain.SegmentStatus{
			State: domain.SegmentStateError,
			Error: err.Error(),
		}
		s.coordinator.NotifySegment(ctx, info, status)
		return fmt.Errorf("storage write: %w", err)
	}

	status := domain.SegmentStatus{State: domain.SegmentStateReady}
	if err := s.notify(info, status); err != nil {
		s.observe(domain.Event{
			Type:       domain.EventSegmentNotifyFailed,
			MediaKey:   info.MediaKey,
			Rendition:  info.Rendition,
			Index:      info.Index,
			StreamType: streamType(info),
			Error:      err.Error(),
		})
	}

	return nil
}

func (s *NotifyingStorage) notify(info domain.SegmentData, status domain.SegmentStatus) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = s.coordinator.NotifySegment(ctx, info, status)
		cancel()
		if err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
		}
	}
	return err
}

func (s *NotifyingStorage) observe(event domain.Event) {
	if s.observer == nil {
		return
	}
	event.Time = time.Now()
	s.observer.Observe(context.Background(), event)
}

func streamType(info domain.SegmentData) domain.StreamType {
	if info.IsVideo {
		return domain.StreamVideo
	}
	return domain.StreamAudio
}

func (s *NotifyingStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	return s.storage.ReadSegment(ctx, info)
}

func (s *NotifyingStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	return s.storage.SegmentExists(ctx, info)
}

func (s *NotifyingStorage) WriteSprite(ctx context.Context, sourceURL string, index int, data []byte) error {
	return s.storage.WriteSprite(ctx, sourceURL, index, data)
}

func (s *NotifyingStorage) ReadSprite(ctx context.Context, sourceURL string, index int) ([]byte, error) {
	return s.storage.ReadSprite(ctx, sourceURL, index)
}

func (s *NotifyingStorage) SpriteExists(ctx context.Context, sourceURL string, index int) (bool, error) {
	return s.storage.SpriteExists(ctx, sourceURL, index)
}

func (s *NotifyingStorage) WriteSpriteVTT(ctx context.Context, sourceURL string, data []byte) error {
	return s.storage.WriteSpriteVTT(ctx, sourceURL, data)
}

func (s *NotifyingStorage) ReadSpriteVTT(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.storage.ReadSpriteVTT(ctx, sourceURL)
}

func (s *NotifyingStorage) SpriteVTTExists(ctx context.Context, sourceURL string) (bool, error) {
	return s.storage.SpriteVTTExists(ctx, sourceURL)
}

func (s *NotifyingStorage) WriteSubtitleVTT(ctx context.Context, sourceURL string, lang string, data []byte) error {
	return s.storage.WriteSubtitleVTT(ctx, sourceURL, lang, data)
}

func (s *NotifyingStorage) ReadSubtitleVTT(ctx context.Context, sourceURL string, lang string) ([]byte, error) {
	return s.storage.ReadSubtitleVTT(ctx, sourceURL, lang)
}

func (s *NotifyingStorage) SubtitleVTTExists(ctx context.Context, sourceURL string, lang string) (bool, error) {
	return s.storage.SubtitleVTTExists(ctx, sourceURL, lang)
}

func (s *NotifyingStorage) WritePreview(ctx context.Context, sourceURL string, data []byte) error {
	return s.storage.WritePreview(ctx, sourceURL, data)
}

func (s *NotifyingStorage) ReadPreview(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.storage.ReadPreview(ctx, sourceURL)
}

func (s *NotifyingStorage) PreviewExists(ctx context.Context, sourceURL string) (bool, error) {
	return s.storage.PreviewExists(ctx, sourceURL)
}

func (s *NotifyingStorage) WriteThumbnail(ctx context.Context, sourceURL string, data []byte) error {
	return s.storage.WriteThumbnail(ctx, sourceURL, data)
}

func (s *NotifyingStorage) ReadThumbnail(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.storage.ReadThumbnail(ctx, sourceURL)
}

func (s *NotifyingStorage) ThumbnailExists(ctx context.Context, sourceURL string) (bool, error) {
	return s.storage.ThumbnailExists(ctx, sourceURL)
}
