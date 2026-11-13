package goshl

import (
	"context"
	"fmt"

	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

func (c *Controller) MasterPlaylist(ctx context.Context, sourceURL string) (string, error) {
	if err := c.ensureRunning(); err != nil {
		return "", err
	}
	meta, _, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	videos, err := rendition.GenerateVideo(meta, c.opts.TargetDuration)
	if err != nil {
		return "", fmt.Errorf("master playlist: %w", err)
	}
	audios := rendition.GenerateAudioTracks(meta.Audios)

	subtitles := convertibleSubtitles(meta.Subtitles)
	if c.opts.OmitSubtitleRenditions {
		subtitles = nil
	}

	out, err := c.playlist.Master(sourceURL, videos, audios, subtitles)
	if err != nil {
		return "", fmt.Errorf("master playlist: %w", err)
	}
	return out, nil
}

func (c *Controller) SubtitlePlaylist(ctx context.Context, sourceURL, trackID string) (string, error) {
	if err := c.ensureRunning(); err != nil {
		return "", err
	}
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}
	if _, err := convertibleSubtitle(meta, trackID); err != nil {
		return "", err
	}
	out, err := c.playlist.Subtitle(sourceURL, trackID, meta.Duration)
	if err != nil {
		return "", fmt.Errorf("subtitle playlist: %w", err)
	}
	return out, nil
}

func (c *Controller) VariantPlaylist(ctx context.Context, sourceURL string, streamType StreamType, renditionName string) (string, error) {
	if err := c.ensureRunning(); err != nil {
		return "", err
	}
	meta, _, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	segments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, c.opts.TargetDuration)
	if _, _, err := c.resolveRendition(meta, streamType, renditionName); err != nil {
		return "", err
	}

	out, err := c.playlist.Variant(sourceURL, renditionName, streamType, segments)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrSegmentOutOfRange, err)
	}
	return out, nil
}
