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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	// SegmentData encapsulates information about a specific media segment.
	SegmentData = domain.SegmentData

	// SegmentStatus represents the processing state of a segment.
	SegmentStatus = domain.SegmentStatus

	// Job represents a transcoding task for a range of segments.
	Job = domain.Job

	// PreviewConfig configures animated preview generation parameters.
	PreviewConfig = domain.PreviewConfig

	// ThumbnailConfig configures thumbnail generation parameters.
	ThumbnailConfig = domain.ThumbnailConfig

	// Metadata contains probed information about a media source.
	Metadata = domain.Metadata

	// VideoStream contains video stream information.
	VideoStream = domain.VideoStream

	// AudioStream contains audio stream information.
	AudioStream = domain.AudioStream

	// SubtitleStream contains subtitle stream information.
	SubtitleStream = domain.SubtitleStream

	// Accelerator identifies a hardware acceleration backend.
	Accelerator = domain.Accelerator
)

const (
	// StreamVideo represents a video stream.
	StreamVideo = domain.StreamVideo

	// StreamAudio represents an audio stream.
	StreamAudio = domain.StreamAudio

	// AccelNone disables hardware acceleration and uses software encoding.
	AccelNone = domain.AccelNone

	// AccelCUDA selects NVIDIA CUDA/NVENC acceleration.
	AccelCUDA = domain.AccelCUDA

	// AccelVideoToolbox selects Apple VideoToolbox acceleration.
	AccelVideoToolbox = domain.AccelVideoToolbox

	// AccelVAAPI selects VAAPI acceleration.
	AccelVAAPI = domain.AccelVAAPI

	// AccelQSV selects Intel Quick Sync Video acceleration.
	AccelQSV = domain.AccelQSV
)

var (
	// ErrLockHeld is returned by Coordinator.AcquireLock when the lock is held by another instance.
	ErrLockHeld = domain.ErrLockHeld
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

	// HWAccelMode explicitly selects a hardware accelerator.
	// When set, it takes precedence over HWAccel auto-detection.
	HWAccelMode Accelerator

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

	// DefaultPreviewConfig sets default parameters for animated preview generation.
	// Defaults: MaxFrames=40, Frameduration=250ms, Width=480.
	DefaultPreviewConfig PreviewConfig

	// DefaultThumbnailConfig sets default parameters for thumbnail generation.
	// Defaults: Position=0.1 (10% into video), Width=480.
	DefaultThumbnailConfig ThumbnailConfig
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
	if o.DefaultPreviewConfig.MaxFrames == 0 {
		o.DefaultPreviewConfig.MaxFrames = 40
	}
	if o.DefaultPreviewConfig.Frameduration == 0 {
		o.DefaultPreviewConfig.Frameduration = 250
	}
	if o.DefaultPreviewConfig.Width == 0 {
		o.DefaultPreviewConfig.Width = 480
	}
	if o.DefaultThumbnailConfig.Position == 0 {
		o.DefaultThumbnailConfig.Position = 0.1
	}
	if o.DefaultThumbnailConfig.Width == 0 {
		o.DefaultThumbnailConfig.Width = 480
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
	hwConfig  *domain.HWAccelConfig
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
	if opts.HWAccelMode != "" {
		var err error
		hwConfig, err = hwaccel.RequireConfig(opts.HWAccelMode)
		if err != nil {
			panic(err)
		}
	} else if opts.HWAccel {
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
		opts.TargetDuration,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
	)

	audioPool := transcode.NewPool(
		opts.Coordinator,
		opts.AudioPoolSize,
		domain.StreamAudio,
		opts.TargetDuration,
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
		miscGen:   misc.NewGenerator(opts.Storage, hwConfig),
		hwConfig:  hwConfig,
	}
}

// HWAccelConfig returns the hardware acceleration configuration selected by the controller.
func (c *Controller) HWAccelConfig() domain.HWAccelConfig {
	if c.hwConfig == nil {
		return *hwaccel.NewConfig(domain.AccelNone)
	}
	return *c.hwConfig
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
		c.videoPool.Stop()
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
	audios := rendition.GenerateAudioTracks(meta.Audios)

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
//  1. Acquires a distributed lock to prevent duplicate transcoding
//  2. Double-checks segment existence after acquiring lock
//  3. Subscribes to segment readiness notifications via Coordinator
//  4. Enqueues a transcoding job covering this segment and nearby segments
//  5. Waits for the worker to complete transcoding (up to SegmentTimeout)
//  6. Returns the transcoded segment data
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

	lockKey := fmt.Sprintf("segment:%s:%s:%d", sourceURL, renditionName, index)
	token, lockErr := c.opts.Coordinator.AcquireLock(ctx, lockKey, c.opts.SegmentTimeout+time.Minute)

	if lockErr != nil {
		if !errors.Is(lockErr, domain.ErrLockHeld) {
			return nil, fmt.Errorf("acquire segment lock: %w", lockErr)
		}
		statusCh, err := c.opts.Coordinator.WaitSegment(ctx, info)
		if err != nil {
			return nil, fmt.Errorf("wait segment: %w", err)
		}
		return c.waitForSegment(ctx, info, statusCh)
	}
	defer c.opts.Coordinator.ReleaseLock(ctx, lockKey, token)

	exists, err = c.opts.Storage.SegmentExists(ctx, info)
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

	return c.waitForSegment(ctx, info, statusCh)
}

func (c *Controller) waitForSegment(ctx context.Context, info domain.SegmentData, statusCh <-chan domain.SegmentStatus) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.opts.SegmentTimeout):
		return nil, fmt.Errorf("timeout waiting for segment %d", info.Index)
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

	return c.miscGen.GetSpriteVTT(ctx, sourceURL, meta.Duration, c.spriteURL(sourceURL))
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

	return c.miscGen.GetSprite(ctx, sourceURL, meta.Duration, c.spriteURL(sourceURL), index)
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

// Preview returns an animated GIF preview of the video.
//
// The preview is generated by sampling frames from sprite sheets and combining
// them into an animated GIF. This provides a quick visual overview of the video
// content suitable for hover previews.
//
// If config is nil, DefaultPreviewConfig is used. The generated preview is
// cached in storage for subsequent requests.
//
// Returns GIF image data.
func (c *Controller) Preview(ctx context.Context, sourceURL string, config *PreviewConfig) ([]byte, error) {
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	cfg := c.opts.DefaultPreviewConfig
	if config != nil {
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

	return c.miscGen.GetPreview(ctx, sourceURL, meta.Duration, c.spriteURL(sourceURL), cfg)
}

func (c *Controller) Thumbnail(ctx context.Context, sourceURL string, config *ThumbnailConfig) ([]byte, error) {
	meta, err := c.getMetadata(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	cfg := c.opts.DefaultThumbnailConfig
	if config != nil {
		cfg.Position = config.Position
		if config.Width > 0 {
			cfg.Width = config.Width
		}
	}

	return c.miscGen.GetThumbnail(ctx, sourceURL, meta.Duration, c.spriteURL(sourceURL), cfg)
}

func (c *Controller) spriteURL(sourceURL string) func(int) string {
	return func(index int) string {
		return c.opts.PathGen.Sprite(sourceURL, index)
	}
}

func (c *Controller) Metadata(ctx context.Context, sourceURL string) (*Metadata, error) {
	return c.getMetadata(ctx, sourceURL)
}

func (c *Controller) enqueueSegment(ctx context.Context, sourceURL string, streamType StreamType, renditionName string, index int) error {
	startIdx := (index / c.opts.SegmentsPerJob) * c.opts.SegmentsPerJob
	endIdx := startIdx + c.opts.SegmentsPerJob - 1

	jobID := deterministicJobID(sourceURL, renditionName, streamType, startIdx, endIdx)

	job := domain.Job{
		ID:         jobID,
		SourceURL:  sourceURL,
		Rendition:  renditionName,
		StreamType: streamType,
		StartIndex: startIdx,
		EndIndex:   endIdx,
	}

	return c.opts.Coordinator.Enqueue(ctx, job)
}

func deterministicJobID(sourceURL, rendition string, streamType StreamType, startIdx, endIdx int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s:%s:%d:%d", sourceURL, rendition, streamType, startIdx, endIdx)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (c *Controller) getMetadata(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	exists, err := c.opts.Storage.MetadataExists(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	if exists {
		return c.readCachedMetadata(ctx, sourceURL)
	}

	lockKey := fmt.Sprintf("metadata:%s", sourceURL)
	token, lockErr := c.opts.Coordinator.AcquireLock(ctx, lockKey, 2*time.Minute)

	if lockErr != nil {
		if !errors.Is(lockErr, domain.ErrLockHeld) {
			return nil, fmt.Errorf("acquire metadata lock: %w", lockErr)
		}
		return c.waitForMetadata(ctx, sourceURL, 2*time.Minute)
	}
	defer c.opts.Coordinator.ReleaseLock(ctx, lockKey, token)

	exists, err = c.opts.Storage.MetadataExists(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	if exists {
		return c.readCachedMetadata(ctx, sourceURL)
	}

	return c.prober.Probe(ctx, sourceURL)
}

func (c *Controller) readCachedMetadata(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
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

func (c *Controller) waitForMetadata(ctx context.Context, sourceURL string, timeout time.Duration) (*domain.Metadata, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("timeout waiting for metadata")
			}
			exists, err := c.opts.Storage.MetadataExists(ctx, sourceURL)
			if err != nil {
				return nil, err
			}
			if exists {
				return c.readCachedMetadata(ctx, sourceURL)
			}
		}
	}
}
