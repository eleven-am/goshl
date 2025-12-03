// Package goshl provides an HLS transcoding service for on-demand video streaming.
//
// goshl handles the complete HLS workflow including media probing, adaptive bitrate
// transcoding, playlist generation, and segment delivery. It supports hardware
// acceleration (NVIDIA NVENC, Apple VideoToolbox) and provides a distributed
// architecture through pluggable Storage and Coordinator interfaces.
//
// # Architecture
//
// The library is built around three core interfaces that must be implemented:
//
//   - Storage: Persists metadata, segments, sprites, and subtitles
//   - Coordinator: Manages job queues and segment readiness notifications
//   - PathGenerator: Generates URLs for playlists, segments, and assets
//
// # Basic Usage
//
//	controller := goshl.NewController(goshl.Options{
//	    Storage:     myStorage,
//	    Coordinator: myCoordinator,
//	    PathGen:     myPathGenerator,
//	    HWAccel:     true, // Enable hardware acceleration
//	})
//
//	// Start the transcoding worker pools
//	if err := controller.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer controller.Stop()
//
//	// Generate master playlist for a media file
//	playlist, err := controller.MasterPlaylist(ctx, "file:///path/to/video.mp4")
//
// # HLS Workflow
//
// When a client requests content:
//
//  1. MasterPlaylist probes the source and returns available renditions
//  2. Client selects a variant and requests VariantPlaylist
//  3. Client requests individual Segments, which triggers transcoding if needed
//  4. Transcoded segments are cached in Storage for subsequent requests
//
// # Segment-on-Demand
//
// Segments are transcoded lazily when first requested. The Segment method:
//   - Returns immediately if the segment exists in storage
//   - Otherwise, enqueues a transcoding job and waits for completion
//   - Jobs transcode multiple segments at once (configurable via SegmentsPerJob)
package goshl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/misc"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/segment"
	"github.com/eleven-am/goshl/internal/transcode"

	"github.com/google/uuid"
)

type (
	// Storage defines the interface for persisting transcoded content and metadata.
	// Implementations should handle concurrent access and may use any backing store
	// (filesystem, S3, Redis, etc.).
	Storage = domain.Storage

	// Coordinator manages the distributed transcoding workflow. It handles job
	// queuing across worker pools and notifies waiting clients when segments
	// become available. For single-instance deployments, an in-memory implementation
	// suffices. For distributed systems, consider Redis or a message queue.
	Coordinator = domain.Coordinator

	// PathGenerator creates URLs for HLS resources. These URLs are embedded in
	// playlists and must be routable back to the appropriate Controller methods.
	PathGenerator = domain.PathGenerator

	// StreamType identifies the type of media stream (video or audio).
	StreamType = domain.StreamType
)

const (
	// StreamVideo represents a video stream.
	StreamVideo = domain.StreamVideo

	// StreamAudio represents an audio stream.
	StreamAudio = domain.StreamAudio
)

// Options configures the Controller behavior and dependencies.
type Options struct {
	// Storage is required. Handles persistence of metadata, segments, and assets.
	Storage Storage

	// Coordinator is required. Manages job distribution and segment notifications.
	Coordinator Coordinator

	// PathGen is required. Generates URLs for playlists and segments.
	PathGen PathGenerator

	// HWAccel enables hardware-accelerated encoding when available.
	// Automatically detects NVENC (NVIDIA) or VideoToolbox (Apple).
	// Falls back to software encoding if no hardware support is found.
	HWAccel bool

	// SegmentTimeout is the maximum time to wait for a segment to be transcoded.
	// Default: 30 seconds.
	SegmentTimeout time.Duration

	// TargetDuration is the target HLS segment duration in seconds.
	// Actual duration varies based on keyframe positions.
	// Default: 6.0 seconds.
	TargetDuration float64

	// SegmentsPerJob is the number of segments transcoded per job.
	// Higher values improve throughput but increase latency for first segment.
	// Default: 10.
	SegmentsPerJob int

	// VideoPoolSize is the number of concurrent video transcoding workers.
	// Default: 2.
	VideoPoolSize int

	// AudioPoolSize is the number of concurrent audio transcoding workers.
	// Default: 4.
	AudioPoolSize int
}

func (o *Options) setDefaults() {
	if o.SegmentTimeout == 0 {
		o.SegmentTimeout = 30 * time.Second
	}
	if o.TargetDuration == 0 {
		o.TargetDuration = 6.0
	}
	if o.SegmentsPerJob == 0 {
		o.SegmentsPerJob = 10
	}
	if o.VideoPoolSize == 0 {
		o.VideoPoolSize = 2
	}
	if o.AudioPoolSize == 0 {
		o.AudioPoolSize = 4
	}
}

func (o *Options) validate() {
	if o.Storage == nil {
		panic("service: Storage is required")
	}
	if o.Coordinator == nil {
		panic("service: Coordinator is required")
	}
	if o.PathGen == nil {
		panic("service: PathGen is required")
	}
}

// Controller is the main entry point for HLS transcoding operations.
// It coordinates media probing, playlist generation, and on-demand transcoding.
//
// A Controller must be started with Start before processing requests,
// and stopped with Stop when shutting down to ensure clean worker termination.
type Controller struct {
	opts      Options
	playlist  *playlist.Generator
	videoPool *transcode.Pool
	audioPool *transcode.Pool
	prober    *probe.Prober
	miscGen   *misc.Generator
}

// NewController creates a new Controller with the given options.
// It panics if required options (Storage, Coordinator, PathGen) are nil.
//
// The controller is not started automatically; call Start to begin
// processing transcoding jobs.
func NewController(opts Options) *Controller {
	opts.validate()
	opts.setDefaults()

	var hwConfig *domain.HWAccelConfig
	if opts.HWAccel {
		hwConfig = hwaccel.DetectBest()
	} else {
		hwConfig = hwaccel.NewConfig(domain.AccelNone)
	}
	cmdBuilder := ffmpeg.NewCommandBuilder(hwConfig)

	notifyingStorage := segment.NewNotifyingStorage(opts.Storage, opts.Coordinator)

	videoPool := transcode.NewPool(
		opts.Coordinator,
		opts.VideoPoolSize,
		domain.StreamVideo,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
	)

	audioPool := transcode.NewPool(
		opts.Coordinator,
		opts.AudioPoolSize,
		domain.StreamAudio,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
	)

	return &Controller{
		opts:      opts,
		playlist:  playlist.NewGenerator(opts.PathGen),
		videoPool: videoPool,
		audioPool: audioPool,
		prober:    probe.NewProber(opts.Storage),
		miscGen:   misc.NewGenerator(opts.Storage),
	}
}

// Start initializes the video and audio transcoding worker pools.
// It subscribes to the Coordinator for incoming jobs and begins processing.
//
// Start must be called before any transcoding can occur. The provided context
// controls the lifetime of background workers; canceling it triggers shutdown.
//
// Returns an error if the worker pools fail to subscribe to the Coordinator.
func (c *Controller) Start(ctx context.Context) error {
	if err := c.videoPool.Start(ctx); err != nil {
		return fmt.Errorf("start video pool: %w", err)
	}
	if err := c.audioPool.Start(ctx); err != nil {
		return fmt.Errorf("start audio pool: %w", err)
	}
	return nil
}

// Stop gracefully shuts down the transcoding worker pools.
// It waits for any in-progress transcoding jobs to complete before returning.
// Always call Stop when shutting down to prevent resource leaks.
func (c *Controller) Stop() {
	c.videoPool.Stop()
	c.audioPool.Stop()
}

// MasterPlaylist returns the HLS master playlist for a media source.
//
// The playlist advertises all available video renditions (based on source
// resolution and bitrate) and audio tracks. On first call for a source,
// it probes the media file using ffprobe and caches the metadata.
//
// The returned string is a complete M3U8 playlist ready to serve to clients.
func (c *Controller) MasterPlaylist(ctx context.Context, sourceURL string) (string, error) {
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	videos := rendition.GenerateVideo(meta.Video)
	var audios []domain.AudioRendition
	if len(meta.Audios) > 0 {
		audios = rendition.GenerateAudio(meta.Audios[0])
	}

	return c.playlist.Master(sourceURL, videos, audios), nil
}

// VariantPlaylist returns the HLS media playlist for a specific rendition.
//
// Parameters:
//   - sourceURL: The media source URL
//   - streamType: Either StreamVideo or StreamAudio
//   - renditionName: The rendition identifier (e.g., "1080p", "720p", "aac_stereo")
//
// The playlist contains segment references with durations calculated from
// the source keyframe positions. Segment URLs are generated via PathGenerator.
func (c *Controller) VariantPlaylist(ctx context.Context, sourceURL string, streamType StreamType, renditionName string) (string, error) {
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	segments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, c.opts.TargetDuration)

	return c.playlist.Variant(sourceURL, renditionName, streamType, segments), nil
}

// Segment returns a transcoded media segment.
//
// If the segment exists in storage, it returns immediately. Otherwise, it:
//  1. Subscribes to segment readiness notifications via Coordinator
//  2. Enqueues a transcoding job covering this segment and nearby segments
//  3. Waits for the worker to complete transcoding (up to SegmentTimeout)
//  4. Returns the transcoded segment data
//
// Parameters:
//   - sourceURL: The media source URL
//   - streamType: Either StreamVideo or StreamAudio
//   - renditionName: The rendition identifier (e.g., "1080p", "aac_stereo")
//   - index: Zero-based segment index
//
// Returns the raw MPEG-TS segment data, or an error if transcoding fails
// or times out.
func (c *Controller) Segment(ctx context.Context, sourceURL string, streamType StreamType, renditionName string, index int) ([]byte, error) {
	info := domain.SegmentData{
		SourceURL: sourceURL,
		Index:     index,
		Rendition: renditionName,
		IsVideo:   streamType == domain.StreamVideo,
	}

	exists, err := c.opts.Storage.SegmentExists(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("check segment: %w", err)
	}

	if exists {
		return c.opts.Storage.ReadSegment(ctx, info)
	}

	statusCh, err := c.opts.Coordinator.WaitSegment(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("wait segment: %w", err)
	}

	if err := c.enqueueSegment(ctx, sourceURL, streamType, renditionName, index); err != nil {
		return nil, fmt.Errorf("enqueue: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.opts.SegmentTimeout):
		return nil, fmt.Errorf("timeout waiting for segment %d", index)
	case status := <-statusCh:
		if status.State == domain.SegmentStateError {
			return nil, fmt.Errorf("segment error: %s", status.Error)
		}
		return c.opts.Storage.ReadSegment(ctx, info)
	}
}

// SpriteVTT returns a WebVTT file mapping timestamps to thumbnail sprite images.
//
// The VTT file references sprite sheet images (containing multiple thumbnails)
// with spatial coordinates for each timestamp. This enables video preview
// thumbnails during seek operations.
//
// Sprite sheets and VTT data are generated on first request and cached.
func (c *Controller) SpriteVTT(ctx context.Context, sourceURL string) ([]byte, error) {
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	urlPattern := c.opts.PathGen.Sprite(sourceURL, 0)
	urlPattern = urlPattern[:len(urlPattern)-1] + "%d"

	return c.miscGen.GetSpriteVTT(ctx, sourceURL, meta.Duration, urlPattern)
}

// Sprite returns a sprite sheet image containing multiple video thumbnails.
//
// Each sprite sheet contains a grid of thumbnails extracted from the video.
// The index parameter selects which sprite sheet to return (videos are divided
// into multiple sheets based on duration).
//
// Returns JPEG image data.
func (c *Controller) Sprite(ctx context.Context, sourceURL string, index int) ([]byte, error) {
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	urlPattern := c.opts.PathGen.Sprite(sourceURL, 0)
	urlPattern = urlPattern[:len(urlPattern)-1] + "%d"

	return c.miscGen.GetSprite(ctx, sourceURL, meta.Duration, urlPattern, index)
}

// SubtitleVTT extracts and returns subtitles in WebVTT format.
//
// The lang parameter specifies the subtitle track language code (e.g., "en", "es").
// If the requested language is not found in the source media, an error is returned.
//
// Subtitles are extracted from the source on first request and cached.
func (c *Controller) SubtitleVTT(ctx context.Context, sourceURL string, lang string) ([]byte, error) {
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	streamIndex := -1
	for i, sub := range meta.Subtitles {
		if sub.Language == lang {
			streamIndex = i
			break
		}
	}
	if streamIndex == -1 {
		return nil, fmt.Errorf("subtitle language %s not found", lang)
	}

	return c.miscGen.GetSubtitles(ctx, sourceURL, streamIndex, lang)
}

func (c *Controller) enqueueSegment(ctx context.Context, sourceURL string, streamType StreamType, renditionName string, index int) error {
	startIdx := (index / c.opts.SegmentsPerJob) * c.opts.SegmentsPerJob
	endIdx := startIdx + c.opts.SegmentsPerJob - 1

	job := domain.Job{
		ID:         uuid.New().String(),
		SourceURL:  sourceURL,
		Rendition:  renditionName,
		StreamType: streamType,
		StartIndex: startIdx,
		EndIndex:   endIdx,
	}

	return c.opts.Coordinator.Enqueue(ctx, job)
}

func (c *Controller) getMetadata(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	exists, err := c.opts.Storage.MetadataExists(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	if exists {
		data, err := c.opts.Storage.GetMetadata(ctx, sourceURL)
		if err != nil {
			return nil, err
		}
		var meta domain.Metadata
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil, err
		}
		return &meta, nil
	}

	return c.prober.Probe(ctx, sourceURL)
}
