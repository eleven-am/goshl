package misc

import (
	"context"
	"fmt"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/mediaexec"
	"github.com/eleven-am/goshl/internal/source"
)

func (g *Generator) GetThumbnail(ctx context.Context, sourceURL, thumbnailKey string, duration float64, config domain.ThumbnailConfig) ([]byte, error) {
	exists, err := g.storage.ThumbnailExists(ctx, thumbnailKey)
	if err != nil {
		return nil, fmt.Errorf("check thumbnail: %w", err)
	}

	if exists {
		return g.storage.ReadThumbnail(ctx, thumbnailKey)
	}

	if err := g.runner().Once(ctx, &g.thumbnails, thumbnailKey, func(runCtx context.Context) (bool, error) {
		return g.storage.ThumbnailExists(runCtx, thumbnailKey)
	}, func(runCtx context.Context) error {
		args, err := g.thumbnailArgs(sourceURL, duration, config)
		if err != nil {
			return err
		}
		thumbnail, err := mediaexec.Output(runCtx, "ffmpeg", args...)
		if err != nil {
			return err
		}
		if err := g.storage.WriteThumbnail(runCtx, thumbnailKey, thumbnail); err != nil {
			return fmt.Errorf("write thumbnail: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return g.storage.ReadThumbnail(ctx, thumbnailKey)
}

func (g *Generator) thumbnailArgs(sourceURL string, duration float64, config domain.ThumbnailConfig) ([]string, error) {
	input, err := source.InputArgs(sourceURL)
	if err != nil {
		return nil, err
	}
	timestamp := duration * config.Position
	if timestamp < 0 {
		timestamp = 0
	}
	if timestamp >= duration {
		timestamp = duration - g.interval
		if timestamp < 0 {
			timestamp = 0
		}
	}

	args := []string{
		"-nostats", "-hide_banner", "-loglevel", "warning",
		"-ss", fmt.Sprintf("%.6f", timestamp),
	}
	args = append(args, input...)
	return append(args,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", config.Width),
		"-q:v", "2",
		"-f", "image2",
		"pipe:1",
	), nil
}
