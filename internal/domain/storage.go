package domain

import "context"

type Storage interface {
	MetadataExists(ctx context.Context, sourceURL string) (bool, error)
	GetMetadata(ctx context.Context, sourceURL string) ([]byte, error)
	SetMetadata(ctx context.Context, sourceURL string, data []byte) error

	WriteSegment(ctx context.Context, info SegmentData, data []byte) error
	ReadSegment(ctx context.Context, info SegmentData) ([]byte, error)
	SegmentExists(ctx context.Context, info SegmentData) (bool, error)

	WriteSprite(ctx context.Context, sourceURL string, index int, data []byte) error
	ReadSprite(ctx context.Context, sourceURL string, index int) ([]byte, error)
	SpriteExists(ctx context.Context, sourceURL string, index int) (bool, error)

	WriteSpriteVTT(ctx context.Context, sourceURL string, data []byte) error
	ReadSpriteVTT(ctx context.Context, sourceURL string) ([]byte, error)
	SpriteVTTExists(ctx context.Context, sourceURL string) (bool, error)

	WriteSubtitleVTT(ctx context.Context, sourceURL string, lang string, data []byte) error
	ReadSubtitleVTT(ctx context.Context, sourceURL string, lang string) ([]byte, error)
	SubtitleVTTExists(ctx context.Context, sourceURL string, lang string) (bool, error)
}
