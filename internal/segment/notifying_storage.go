package segment

import (
	"context"
	"fmt"

	"github.com/eleven-am/goshl/internal/domain"
)

type NotifyingStorage struct {
	storage     domain.Storage
	coordinator domain.Coordinator
}

func NewNotifyingStorage(storage domain.Storage, coordinator domain.Coordinator) *NotifyingStorage {
	return &NotifyingStorage{
		storage:     storage,
		coordinator: coordinator,
	}
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
	if err := s.coordinator.NotifySegment(ctx, info, status); err != nil {
		return fmt.Errorf("notify segment: %w", err)
	}

	return nil
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
