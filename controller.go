package goshl

import (
	"context"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/engine"
	"github.com/eleven-am/goshl/internal/hwaccel"
)

type Controller struct {
	engine *engine.Engine
}

func NewController(opts Options) *Controller {
	controller, err := NewControllerWithContext(context.Background(), opts)
	if err != nil {
		panic(err)
	}
	return controller
}

func NewControllerWithContext(ctx context.Context, opts Options) (*Controller, error) {
	built, err := engine.New(ctx, opts.config())
	if err != nil {
		return nil, err
	}
	return &Controller{engine: built}, nil
}

func (c *Controller) Start(ctx context.Context) error {
	return c.engine.Start(ctx)
}

func (c *Controller) Stop() {
	c.engine.Stop()
}

func (c *Controller) HWAccelConfig() domain.HWAccelConfig {
	if c.engine == nil {
		return *hwaccel.NewConfig(domain.AccelNone)
	}
	return c.engine.HWAccelConfig()
}

func (c *Controller) MasterPlaylist(ctx context.Context, sourceURL string) (string, error) {
	return c.engine.MasterPlaylist(ctx, sourceURL)
}

func (c *Controller) VariantPlaylist(ctx context.Context, sourceURL string, streamType StreamType, renditionName string) (string, error) {
	return c.engine.VariantPlaylist(ctx, sourceURL, streamType, renditionName)
}

func (c *Controller) SubtitlePlaylist(ctx context.Context, sourceURL, trackID string) (string, error) {
	return c.engine.SubtitlePlaylist(ctx, sourceURL, trackID)
}

func (c *Controller) Segment(ctx context.Context, sourceURL string, streamType StreamType, renditionName string, index int) ([]byte, error) {
	return c.engine.Segment(ctx, sourceURL, streamType, renditionName, index)
}

func (c *Controller) Metadata(ctx context.Context, sourceURL string) (*Metadata, error) {
	return c.engine.Metadata(ctx, sourceURL)
}

func (c *Controller) ProbeMetadata(ctx context.Context, sourceURL string) (*Metadata, error) {
	return c.engine.ProbeMetadata(ctx, sourceURL)
}

func (c *Controller) MediaKey(ctx context.Context, sourceURL string) (string, error) {
	return c.engine.MediaKey(ctx, sourceURL)
}

func (c *Controller) Invalidate(ctx context.Context, sourceURL string) error {
	return c.engine.Invalidate(ctx, sourceURL)
}

func (c *Controller) SpriteVTT(ctx context.Context, sourceURL string) ([]byte, error) {
	return c.engine.SpriteVTT(ctx, sourceURL)
}

func (c *Controller) Sprite(ctx context.Context, sourceURL string, index int) ([]byte, error) {
	return c.engine.Sprite(ctx, sourceURL, index)
}

func (c *Controller) SubtitleVTT(ctx context.Context, sourceURL string, trackID string) ([]byte, error) {
	return c.engine.SubtitleVTT(ctx, sourceURL, trackID)
}

func (c *Controller) SubtitlesVTT(ctx context.Context, sourceURL string, trackIDs []string) (map[string]SubtitleResult, error) {
	results, err := c.engine.SubtitlesVTT(ctx, sourceURL, trackIDs)
	if err != nil {
		return nil, err
	}
	converted := make(map[string]SubtitleResult, len(results))
	for trackID, result := range results {
		converted[trackID] = SubtitleResult(result)
	}
	return converted, nil
}

func (c *Controller) Preview(ctx context.Context, sourceURL string, config *PreviewConfig) ([]byte, error) {
	return c.engine.Preview(ctx, sourceURL, config)
}

func (c *Controller) Thumbnail(ctx context.Context, sourceURL string, config *ThumbnailConfig) ([]byte, error) {
	return c.engine.Thumbnail(ctx, sourceURL, config)
}
