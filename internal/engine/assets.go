package engine

import (
	"context"
	"fmt"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/misc"
	"github.com/eleven-am/goshl/internal/subtitle"
)

func (e *Engine) SpriteVTT(ctx context.Context, sourceURL string) ([]byte, error) {
	if err := e.ensureRunning(); err != nil {
		return nil, err
	}
	meta, mediaKey, err := e.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	spriteKey := assetIdentity(mediaKey, "sprites", e.miscGen.SpriteIdentity())
	return e.miscGen.GetSpriteVTT(ctx, sourceURL, spriteKey, meta.Duration, e.spriteURL(sourceURL))
}

func (e *Engine) Sprite(ctx context.Context, sourceURL string, index int) ([]byte, error) {
	if err := e.ensureRunning(); err != nil {
		return nil, err
	}
	if index < 0 {
		return nil, fmt.Errorf("sprite index out of range: %d", index)
	}
	meta, mediaKey, err := e.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	spriteKey := assetIdentity(mediaKey, "sprites", e.miscGen.SpriteIdentity())
	return e.miscGen.GetSprite(ctx, sourceURL, spriteKey, meta.Duration, index)
}

type SubtitleResult struct {
	Data []byte
	Err  error
}

func (e *Engine) SubtitleVTT(ctx context.Context, sourceURL string, trackID string) ([]byte, error) {
	results, err := e.SubtitlesVTT(ctx, sourceURL, []string{trackID})
	if err != nil {
		return nil, err
	}
	result := results[trackID]
	return result.Data, result.Err
}

func (e *Engine) SubtitlesVTT(ctx context.Context, sourceURL string, trackIDs []string) (map[string]SubtitleResult, error) {
	if err := e.ensureRunning(); err != nil {
		return nil, err
	}
	meta, mediaKey, err := e.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	results := make(map[string]SubtitleResult, len(trackIDs))
	jobs := make([]misc.SubtitleJob, 0, len(trackIDs))
	for _, trackID := range trackIDs {
		found, err := convertibleSubtitle(meta, trackID)
		if err != nil {
			results[trackID] = SubtitleResult{Err: err}
			continue
		}
		jobs = append(jobs, misc.SubtitleJob{
			Key:         assetIdentity(mediaKey, "subtitle", trackID),
			TrackID:     trackID,
			StreamIndex: found.Index,
			Codec:       found.Codec,
			Origin:      meta.StartTime,
		})
	}

	request := misc.SubtitleRequest{SourceKey: assetIdentity(mediaKey, "subtitles", nil), Jobs: jobs}
	for _, sibling := range convertibleSubtitles(meta.Subtitles) {
		request.Siblings = append(request.Siblings, misc.SubtitleJob{
			Key:         assetIdentity(mediaKey, "subtitle", sibling.ID),
			TrackID:     sibling.ID,
			StreamIndex: sibling.Index,
			Codec:       sibling.Codec,
			Origin:      meta.StartTime,
		})
	}
	outcomes, err := e.miscGen.GetSubtitles(ctx, sourceURL, request)
	if err != nil {
		return nil, err
	}
	for trackID, outcome := range outcomes {
		results[trackID] = SubtitleResult{Data: outcome.Data, Err: outcome.Err}
	}
	return results, nil
}

func convertibleSubtitles(streams []domain.SubtitleStream) []domain.SubtitleStream {
	kept := make([]domain.SubtitleStream, 0, len(streams))
	for _, stream := range streams {
		if subtitle.Convertible(stream.Codec) {
			kept = append(kept, stream)
		}
	}
	return kept
}

func convertibleSubtitle(meta *domain.Metadata, trackID string) (*domain.SubtitleStream, error) {
	for i := range meta.Subtitles {
		if meta.Subtitles[i].ID != trackID {
			continue
		}
		if !subtitle.Convertible(meta.Subtitles[i].Codec) {
			return nil, fmt.Errorf("%w: subtitle track %s uses codec %q, which cannot be converted to webvtt", ErrRenditionNotFound, trackID, meta.Subtitles[i].Codec)
		}
		return &meta.Subtitles[i], nil
	}
	return nil, fmt.Errorf("%w: subtitle track %s", ErrRenditionNotFound, trackID)
}

func (e *Engine) Preview(ctx context.Context, sourceURL string, config *domain.PreviewConfig) ([]byte, error) {
	if err := e.ensureRunning(); err != nil {
		return nil, err
	}
	cfg := e.opts.DefaultPreviewConfig
	if config != nil {
		if err := validatePreviewConfig(*config, true); err != nil {
			return nil, err
		}
		if config.MaxFrames > 0 {
			cfg.MaxFrames = config.MaxFrames
		}
		if config.Frameduration > 0 {
			cfg.Frameduration = config.Frameduration
		}
		if config.Width > 0 {
			cfg.Width = config.Width
		}
	}
	meta, mediaKey, err := e.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	spriteKey := assetIdentity(mediaKey, "sprites", e.miscGen.SpriteIdentity())
	previewKey := assetIdentity(mediaKey, "preview", cfg)
	return e.miscGen.GetPreview(ctx, sourceURL, spriteKey, previewKey, meta.Duration, cfg)
}

func (e *Engine) Thumbnail(ctx context.Context, sourceURL string, config *domain.ThumbnailConfig) ([]byte, error) {
	if err := e.ensureRunning(); err != nil {
		return nil, err
	}
	cfg := e.opts.DefaultThumbnailConfig
	if config != nil {
		if err := validateThumbnailConfig(*config, true); err != nil {
			return nil, err
		}
		if config.Position > 0 {
			cfg.Position = config.Position
		}
		if config.Width > 0 {
			cfg.Width = config.Width
		}
	}
	meta, mediaKey, err := e.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	thumbnailKey := assetIdentity(mediaKey, "thumbnail", cfg)
	return e.miscGen.GetThumbnail(ctx, sourceURL, thumbnailKey, meta.Duration, cfg)
}

func (e *Engine) spriteURL(sourceURL string) func(int) string {
	return func(index int) string {
		return e.opts.PathGen.Sprite(sourceURL, index)
	}
}
