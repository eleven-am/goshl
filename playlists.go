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

	videos := rendition.GenerateVideo(meta.Video)
	audios := rendition.GenerateAudioTracks(meta.Audios)

	subtitles := meta.Subtitles
	if c.opts.OmitSubtitleRenditions {
		subtitles = nil
	}

	return c.playlist.Master(sourceURL, videos, audios, subtitles), nil
}

func (c *Controller) SubtitlePlaylist(ctx context.Context, sourceURL, trackID string) (string, error) {
	if err := c.ensureRunning(); err != nil {
		return "", err
	}
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}
	for _, subtitle := range meta.Subtitles {
		if subtitle.ID == trackID {
			return c.playlist.Subtitle(sourceURL, trackID, meta.Duration), nil
		}
	}
	return "", fmt.Errorf("subtitle track %s not found", trackID)
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
	if _, _, err := resolveRendition(meta, streamType, renditionName); err != nil {
		return "", err
	}
	if len(segments) == 0 {
		return "", fmt.Errorf("%w: source has no playable segments", ErrSegmentOutOfRange)
	}

	return c.playlist.Variant(sourceURL, renditionName, streamType, segments), nil
}
