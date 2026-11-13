package engine

import (
	"context"
	"fmt"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

func (e *Engine) MasterPlaylist(ctx context.Context, sourceURL string) (string, error) {
	if err := e.ensureRunning(); err != nil {
		return "", err
	}
	meta, mediaKey, err := e.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	videos, dropped, err := rendition.GenerateVideo(meta, e.opts.TargetDuration)
	for _, rung := range dropped {
		e.observe(ctx, domain.Event{Type: domain.EventRenditionDropped, MediaKey: mediaKey, StreamType: domain.StreamVideo, Rendition: rung.Name, Error: rung.Reason})
	}
	if err != nil {
		return "", fmt.Errorf("master playlist: %w", err)
	}
	for _, video := range videos {
		if video.Colour.Tonemap {
			if err := e.tonemap.check(ctx); err != nil {
				return "", fmt.Errorf("master playlist: %w", err)
			}
			break
		}
	}
	audios := rendition.GenerateAudioTracks(meta.Audios)

	subtitles := convertibleSubtitles(meta.Subtitles)
	if e.opts.OmitSubtitleRenditions {
		subtitles = nil
	}

	out, err := e.playlist.Master(sourceURL, videos, audios, subtitles)
	if err != nil {
		return "", fmt.Errorf("master playlist: %w", err)
	}
	return out, nil
}

func (e *Engine) SubtitlePlaylist(ctx context.Context, sourceURL, trackID string) (string, error) {
	if err := e.ensureRunning(); err != nil {
		return "", err
	}
	meta, err := e.getMetadata(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}
	if _, err := convertibleSubtitle(meta, trackID); err != nil {
		return "", err
	}
	out, err := e.playlist.Subtitle(sourceURL, trackID, meta.Duration)
	if err != nil {
		return "", fmt.Errorf("subtitle playlist: %w", err)
	}
	return out, nil
}

func (e *Engine) VariantPlaylist(ctx context.Context, sourceURL string, streamType domain.StreamType, renditionName string) (string, error) {
	if err := e.ensureRunning(); err != nil {
		return "", err
	}
	meta, _, err := e.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	segments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, e.opts.TargetDuration)
	video, _, err := e.resolveRendition(meta, streamType, renditionName)
	if err != nil {
		return "", err
	}
	if video != nil && video.Colour.Tonemap {
		if err := e.tonemap.check(ctx); err != nil {
			return "", fmt.Errorf("variant playlist: %w", err)
		}
	}

	out, err := e.playlist.Variant(sourceURL, renditionName, streamType, segments)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrSegmentOutOfRange, err)
	}
	return out, nil
}
