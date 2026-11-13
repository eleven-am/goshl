package goshl

import (
	"context"
	"fmt"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

func (c *Controller) MasterPlaylist(ctx context.Context, sourceURL string) (string, error) {
	if err := c.ensureRunning(); err != nil {
		return "", err
	}
	meta, mediaKey, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	videos, dropped, err := rendition.GenerateVideo(meta, c.opts.TargetDuration)
	for _, rung := range dropped {
		c.observe(ctx, domain.Event{Type: domain.EventRenditionDropped, MediaKey: mediaKey, StreamType: domain.StreamVideo, Rendition: rung.Name, Error: rung.Reason})
	}
	if err != nil {
		return "", fmt.Errorf("master playlist: %w", err)
	}
	for _, video := range videos {
		if video.Colour.Tonemap {
			if err := c.tonemap.check(ctx); err != nil {
				return "", fmt.Errorf("master playlist: %w", err)
			}
			break
		}
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
	video, _, err := c.resolveRendition(meta, streamType, renditionName)
	if err != nil {
		return "", err
	}
	if video != nil && video.Colour.Tonemap {
		if err := c.tonemap.check(ctx); err != nil {
			return "", fmt.Errorf("variant playlist: %w", err)
		}
	}

	out, err := c.playlist.Variant(sourceURL, renditionName, streamType, segments)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrSegmentOutOfRange, err)
	}
	return out, nil
}
