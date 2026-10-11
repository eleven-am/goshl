package domain

import "context"

type Storage interface {
	MetadataExists(ctx context.Context, mediaKey string) (bool, error)
	GetMetadata(ctx context.Context, mediaKey string) ([]byte, error)
	SetMetadata(ctx context.Context, mediaKey string, data []byte) error

	WriteSegment(ctx context.Context, info SegmentData, data []byte) error
	ReadSegment(ctx context.Context, info SegmentData) ([]byte, error)
	SegmentExists(ctx context.Context, info SegmentData) (bool, error)

	WriteSprite(ctx context.Context, assetKey string, index int, data []byte) error
	ReadSprite(ctx context.Context, assetKey string, index int) ([]byte, error)
	SpriteExists(ctx context.Context, assetKey string, index int) (bool, error)

	WriteSpriteVTT(ctx context.Context, assetKey string, data []byte) error
	ReadSpriteVTT(ctx context.Context, assetKey string) ([]byte, error)
	SpriteVTTExists(ctx context.Context, assetKey string) (bool, error)

	WriteSubtitleVTT(ctx context.Context, assetKey string, trackID string, data []byte) error
	ReadSubtitleVTT(ctx context.Context, assetKey string, trackID string) ([]byte, error)
	SubtitleVTTExists(ctx context.Context, assetKey string, trackID string) (bool, error)

	WritePreview(ctx context.Context, assetKey string, data []byte) error
	ReadPreview(ctx context.Context, assetKey string) ([]byte, error)
	PreviewExists(ctx context.Context, assetKey string) (bool, error)

	WriteThumbnail(ctx context.Context, assetKey string, data []byte) error
	ReadThumbnail(ctx context.Context, assetKey string) ([]byte, error)
	ThumbnailExists(ctx context.Context, assetKey string) (bool, error)
}

type SpriteStorage interface {
	WriteSprite(ctx context.Context, assetKey string, index int, data []byte) error
	ReadSprite(ctx context.Context, assetKey string, index int) ([]byte, error)
	SpriteExists(ctx context.Context, assetKey string, index int) (bool, error)

	WriteSpriteVTT(ctx context.Context, assetKey string, data []byte) error
	ReadSpriteVTT(ctx context.Context, assetKey string) ([]byte, error)
	SpriteVTTExists(ctx context.Context, assetKey string) (bool, error)
}
