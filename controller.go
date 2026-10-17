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
	"math"
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

	SpriteStorage = domain.SpriteStorage

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

	Segment = domain.Segment

	VideoRendition = domain.VideoRendition

	AudioRendition = domain.AudioRendition

	PlaybackMethod = domain.PlaybackMethod

	// Accelerator identifies a hardware acceleration backend.
	Accelerator = domain.Accelerator

	HWAccelConfig = domain.HWAccelConfig

	SegmentState = domain.SegmentState

	Observer = domain.Observer

	ObserverFunc = domain.ObserverFunc

	Event = domain.Event

	EventType = domain.EventType

	DecodeMode = domain.DecodeMode
)

const (
	// StreamVideo represents a video stream.
	StreamVideo = domain.StreamVideo

	// StreamAudio represents an audio stream.
	StreamAudio = domain.StreamAudio

	StreamSubtitle = domain.StreamSubtitle

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

	SegmentStateReady = domain.SegmentStateReady

	SegmentStateError = domain.SegmentStateError

	DecodeSafe = domain.DecodeSafe

	DecodeDevice = domain.DecodeDevice

	DirectPlay = domain.DirectPlay

	DirectStream = domain.DirectStream

	Transcode = domain.Transcode

	EventMetadataCacheHit = domain.EventMetadataCacheHit

	EventMetadataCacheMiss = domain.EventMetadataCacheMiss

	EventMetadataProbed = domain.EventMetadataProbed

	EventSegmentCacheHit = domain.EventSegmentCacheHit

	EventSegmentCacheMiss = domain.EventSegmentCacheMiss

	EventLockWait = domain.EventLockWait

	EventJobEnqueued  = domain.EventJobEnqueued
	EventJobJoined    = domain.EventJobJoined
	EventJobAbandoned = domain.EventJobAbandoned

	EventJobStarted = domain.EventJobStarted

	EventJobCompleted = domain.EventJobCompleted

	EventJobFailed = domain.EventJobFailed

	EventJobAckFailed = domain.EventJobAckFailed

	EventSegmentReady = domain.EventSegmentReady

	EventSegmentFailed = domain.EventSegmentFailed

	EventSegmentNotifyFailed = domain.EventSegmentNotifyFailed

	EventHardwareSelected = domain.EventHardwareSelected

	EventHardwareFallback = domain.EventHardwareFallback

	EventLockReleaseFailed = domain.EventLockReleaseFailed
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
	// Default: 4.
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

	JobTimeout time.Duration

	// AbandonGrace is how long a running job survives after its last waiter
	// leaves before it is cancelled. A waiter that joins within the grace
	// period keeps the job alive.
	// Default: 3 seconds.
	AbandonGrace time.Duration

	ProbeTimeout time.Duration

	SpriteStorage SpriteStorage

	CacheVersion string

	SourceRevision SourceRevisionResolver

	Observer Observer

	HWAccelDecodeMode DecodeMode

	AllowHardwareFallback bool
}

func (o *Options) setDefaults() {
	if o.SegmentTimeout == 0 {
		o.SegmentTimeout = 30 * time.Second
	}
	if o.TargetDuration == 0 {
		o.TargetDuration = 6.0
	}
	if o.SegmentsPerJob == 0 {
		o.SegmentsPerJob = 4
	}
	if o.VideoPoolSize == 0 {
		o.VideoPoolSize = 2
	}
	if o.AudioPoolSize == 0 {
		o.AudioPoolSize = 4
	}
	if o.JobTimeout == 0 {
		o.JobTimeout = 2 * time.Minute
	}
	if o.AbandonGrace == 0 {
		o.AbandonGrace = 3 * time.Second
	}
	if o.ProbeTimeout == 0 {
		o.ProbeTimeout = o.JobTimeout
	}
	if o.HWAccelDecodeMode == "" {
		o.HWAccelDecodeMode = domain.DecodeSafe
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
	if err := o.validateError(); err != nil {
		panic(err)
	}
}

func (o *Options) validateError() error {
	if o.Storage == nil {
		return fmt.Errorf("%w: Storage is required", ErrInvalidOptions)
	}
	if o.Coordinator == nil {
		return fmt.Errorf("%w: Coordinator is required", ErrInvalidOptions)
	}
	if o.PathGen == nil {
		return fmt.Errorf("%w: PathGen is required", ErrInvalidOptions)
	}
	if o.SegmentTimeout < 0 || o.JobTimeout < 0 || o.AbandonGrace < 0 || o.ProbeTimeout < 0 || o.TargetDuration < 0 || math.IsNaN(o.TargetDuration) || math.IsInf(o.TargetDuration, 0) {
		return fmt.Errorf("%w: durations must be positive", ErrInvalidOptions)
	}
	if o.SegmentsPerJob < 0 || o.VideoPoolSize < 0 || o.AudioPoolSize < 0 {
		return fmt.Errorf("%w: pool and batch sizes must be positive", ErrInvalidOptions)
	}
	if o.DefaultPreviewConfig.MaxFrames < 0 || o.DefaultPreviewConfig.Frameduration < 0 || o.DefaultPreviewConfig.Width < 0 || o.DefaultThumbnailConfig.Position < 0 || o.DefaultThumbnailConfig.Position > 1 || math.IsNaN(o.DefaultThumbnailConfig.Position) || math.IsInf(o.DefaultThumbnailConfig.Position, 0) || o.DefaultThumbnailConfig.Width < 0 {
		return fmt.Errorf("%w: generated asset configuration is invalid", ErrInvalidOptions)
	}
	if o.HWAccelDecodeMode != "" && o.HWAccelDecodeMode != domain.DecodeSafe && o.HWAccelDecodeMode != domain.DecodeDevice {
		return fmt.Errorf("%w: unsupported hardware decode mode %q", ErrInvalidOptions, o.HWAccelDecodeMode)
	}
	return nil
}

func (o *Options) validateResolvedError() error {
	if o.SegmentTimeout <= 0 || o.JobTimeout <= 0 || o.AbandonGrace <= 0 || o.ProbeTimeout <= 0 || o.TargetDuration <= 0 {
		return fmt.Errorf("%w: durations must be positive", ErrInvalidOptions)
	}
	if o.SegmentsPerJob <= 0 || o.VideoPoolSize <= 0 || o.AudioPoolSize <= 0 {
		return fmt.Errorf("%w: pool and batch sizes must be positive", ErrInvalidOptions)
	}
	if err := validatePreviewConfig(o.DefaultPreviewConfig); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOptions, err)
	}
	if err := validateThumbnailConfig(o.DefaultThumbnailConfig); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOptions, err)
	}
	return nil
}

func validatePreviewConfig(config domain.PreviewConfig) error {
	if config.MaxFrames <= 0 || config.MaxFrames > 1000 || config.Frameduration <= 0 || config.Frameduration > 60_000 || config.Width <= 0 || config.Width > 4096 {
		return ErrInvalidPreviewConfig
	}
	return nil
}

func validateThumbnailConfig(config domain.ThumbnailConfig) error {
	if config.Position < 0 || config.Position > 1 || math.IsNaN(config.Position) || math.IsInf(config.Position, 0) || config.Width <= 0 || config.Width > 8192 {
		return ErrInvalidThumbnailConfig
	}
	return nil
}

// Controller is the main entry point for HLS transcoding operations.
// It coordinates media probing, playlist generation, and on-demand transcoding.
//
// A Controller must be started with Start before processing requests,
// and stopped with Stop when shutting down to ensure clean worker termination.
type Controller struct {
	opts            Options
	playlist        *playlist.Generator
	videoPool       *transcode.Pool
	audioPool       *transcode.Pool
	prober          *probe.Prober
	miscGen         *misc.Generator
	hwConfig        *domain.HWAccelConfig
	pipelineVersion string
	flights         *flightRegistry
}

// NewController creates a new Controller with the given options.
// It panics if required options (Storage, Coordinator, PathGen) are nil.
//
// The controller is not started automatically; call Start to begin
// processing transcoding jobs.
func NewController(opts Options) *Controller {
	controller, err := NewControllerWithContext(context.Background(), opts)
	if err != nil {
		panic(err)
	}
	return controller
}

func NewControllerWithContext(ctx context.Context, opts Options) (*Controller, error) {
	if err := opts.validateError(); err != nil {
		return nil, err
	}
	opts.setDefaults()
	if err := opts.validateResolvedError(); err != nil {
		return nil, err
	}

	var hwConfig *domain.HWAccelConfig
	if opts.HWAccelMode != "" {
		var err error
		hwConfig, err = hwaccel.RequireConfigWithMode(opts.HWAccelMode, opts.HWAccelDecodeMode)
		if err != nil {
			return nil, err
		}
		if opts.HWAccelMode != domain.AccelNone {
			validateCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			validationErr := hwaccel.Validate(validateCtx, hwConfig)
			cancel()
			if validationErr != nil {
				if !opts.AllowHardwareFallback {
					return nil, fmt.Errorf("hardware pipeline validation failed: %w", validationErr)
				}
				hwConfig = hwaccel.NewConfig(domain.AccelNone)
				hwConfig.Requested = opts.HWAccelMode
				hwConfig.Error = validationErr.Error()
			}
		}
	} else if opts.HWAccel {
		hwConfig = hwaccel.DetectBestContext(ctx)
	} else {
		hwConfig = hwaccel.NewConfig(domain.AccelNone)
	}
	cmdBuilder := ffmpeg.NewCommandBuilder(hwConfig)
	pipeline := pipelineVersion(opts, hwConfig)

	notifyingStorage := segment.NewNotifyingStorage(opts.Storage, opts.Coordinator, opts.Observer)

	var controller *Controller
	onJobDone := func(jobID string) { controller.finishFlight(jobID) }

	videoPool := transcode.NewPool(
		opts.Coordinator,
		opts.VideoPoolSize,
		domain.StreamVideo,
		opts.TargetDuration,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
		transcode.PoolConfig{JobTimeout: opts.JobTimeout, Observer: opts.Observer, Profile: pipeline, OnJobDone: onJobDone},
	)

	audioPool := transcode.NewPool(
		opts.Coordinator,
		opts.AudioPoolSize,
		domain.StreamAudio,
		opts.TargetDuration,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
		transcode.PoolConfig{JobTimeout: opts.JobTimeout, Observer: opts.Observer, Profile: pipeline, OnJobDone: onJobDone},
	)

	miscGen := misc.NewGeneratorWithCoordinator(opts.Storage, opts.Coordinator, hwConfig, opts.Observer)
	miscGen.SetSpriteStorage(opts.SpriteStorage)

	controller = &Controller{
		opts:            opts,
		playlist:        playlist.NewGenerator(opts.PathGen),
		videoPool:       videoPool,
		audioPool:       audioPool,
		prober:          probe.NewProber(opts.Storage),
		miscGen:         miscGen,
		hwConfig:        hwConfig,
		pipelineVersion: pipeline,
		flights:         newFlightRegistry(),
	}
	controller.observe(ctx, domain.Event{Type: domain.EventHardwareSelected, Accelerator: hwConfig.Accelerator})
	if hwConfig.Error != "" && hwConfig.Requested != hwConfig.Accelerator {
		controller.observe(ctx, domain.Event{Type: domain.EventHardwareFallback, Accelerator: hwConfig.Accelerator, Error: hwConfig.Error})
	}
	return controller, nil
}

// HWAccelConfig returns the hardware acceleration configuration selected by the controller.
func (c *Controller) HWAccelConfig() domain.HWAccelConfig {
	if c.hwConfig == nil {
		return *hwaccel.NewConfig(domain.AccelNone)
	}
	config := *c.hwConfig
	config.Available = append([]domain.Accelerator(nil), c.hwConfig.Available...)
	config.DecodeFlags = append([]string(nil), c.hwConfig.DecodeFlags...)
	config.EncodeFlags = append([]string(nil), c.hwConfig.EncodeFlags...)
	return config
}

func (c *Controller) observe(ctx context.Context, event domain.Event) {
	if c.opts.Observer == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	c.opts.Observer.Observe(ctx, event)
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
	meta, _, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("get metadata: %w", err)
	}

	videos := rendition.GenerateVideo(meta.Video)
	audios := rendition.GenerateAudioTracks(meta.Audios)

	return c.playlist.Master(sourceURL, videos, audios, meta.Subtitles), nil
}

func (c *Controller) SubtitlePlaylist(ctx context.Context, sourceURL, trackID string) (string, error) {
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
	if streamType != domain.StreamVideo && streamType != domain.StreamAudio {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStreamType, streamType)
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, index)
	}
	mediaKey, err := c.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	info := domain.SegmentData{
		MediaKey:  mediaKey,
		Index:     index,
		Rendition: renditionName,
		IsVideo:   streamType == domain.StreamVideo,
	}

	exists, err := c.opts.Storage.SegmentExists(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("check segment: %w", err)
	}

	if exists {
		c.observe(ctx, domain.Event{Type: domain.EventSegmentCacheHit, MediaKey: mediaKey, StreamType: streamType, Rendition: renditionName, Index: index})
		return c.opts.Storage.ReadSegment(ctx, info)
	}
	c.observe(ctx, domain.Event{Type: domain.EventSegmentCacheMiss, MediaKey: mediaKey, StreamType: streamType, Rendition: renditionName, Index: index})

	meta, err := c.getMetadataWithKey(ctx, sourceURL, mediaKey)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}
	videoRendition, audioRendition, err := resolveRendition(meta, streamType, renditionName)
	if err != nil {
		return nil, err
	}
	allSegments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, c.opts.TargetDuration)
	if index >= len(allSegments) {
		return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, index)
	}
	endIdx := index + c.opts.SegmentsPerJob - 1
	if endIdx >= len(allSegments) {
		endIdx = len(allSegments) - 1
	}
	for next := index + 1; next <= endIdx; next++ {
		nextInfo := info
		nextInfo.Index = next
		nextExists, err := c.opts.Storage.SegmentExists(ctx, nextInfo)
		if err != nil {
			return nil, fmt.Errorf("check segment: %w", err)
		}
		if nextExists {
			endIdx = next - 1
			break
		}
	}

	requested := time.Now()
	existing, created := c.joinFlight(mediaKey, streamType, renditionName, index, endIdx)
	if existing != nil {
		defer c.leaveFlight(existing)
		statusCh, err := c.opts.Coordinator.WaitSegment(ctx, info)
		if err != nil {
			return nil, fmt.Errorf("wait segment: %w", err)
		}
		c.observe(ctx, domain.Event{Type: domain.EventJobJoined, MediaKey: mediaKey, JobID: existing.jobID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: existing.start, EndIndex: existing.end})
		if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
			return data, err
		}
		return c.waitForSegment(ctx, info, statusCh, requested)
	}
	defer c.leaveFlight(created)
	startIdx, endIdx, jobID := created.start, created.end, created.jobID
	lockKey := "job:" + jobID
	token, lockErr := c.opts.Coordinator.AcquireLock(ctx, lockKey, c.opts.JobTimeout+time.Minute)

	if lockErr != nil {
		c.finishFlight(jobID)
		if !errors.Is(lockErr, domain.ErrLockHeld) {
			return nil, fmt.Errorf("acquire segment lock: %w", lockErr)
		}
		statusCh, err := c.opts.Coordinator.WaitSegment(ctx, info)
		if err != nil {
			return nil, fmt.Errorf("wait segment: %w", err)
		}
		c.observe(ctx, domain.Event{Type: domain.EventLockWait, MediaKey: mediaKey, JobID: jobID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: startIdx, EndIndex: endIdx})
		if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
			return data, err
		}
		return c.waitForSegment(ctx, info, statusCh, requested)
	}
	c.setFlightLock(created, lockKey, token)

	exists, err = c.opts.Storage.SegmentExists(ctx, info)
	if err != nil {
		c.finishFlight(jobID)
		return nil, fmt.Errorf("check segment: %w", err)
	}
	if exists {
		c.finishFlight(jobID)
		return c.opts.Storage.ReadSegment(ctx, info)
	}

	statusCh, err := c.opts.Coordinator.WaitSegment(ctx, info)
	if err != nil {
		c.finishFlight(jobID)
		return nil, fmt.Errorf("wait segment: %w", err)
	}

	job := c.buildJob(sourceURL, mediaKey, streamType, renditionName, index, startIdx, endIdx, allSegments, videoRendition, audioRendition)
	if err := c.opts.Coordinator.Enqueue(ctx, job); err != nil {
		c.finishFlight(jobID)
		return nil, fmt.Errorf("enqueue: %w", err)
	}
	c.observe(ctx, domain.Event{Type: domain.EventJobEnqueued, MediaKey: mediaKey, JobID: job.ID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: startIdx, EndIndex: endIdx})

	return c.waitForSegment(ctx, info, statusCh, requested)
}

func (c *Controller) waitForSegment(ctx context.Context, info domain.SegmentData, statusCh <-chan domain.SegmentStatus, requested time.Time) ([]byte, error) {
	timer := time.NewTimer(c.opts.SegmentTimeout)
	defer timer.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
			return nil, fmt.Errorf("%w: segment %d", ErrSegmentTimeout, info.Index)
		case <-poll.C:
			if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
		case status, ok := <-statusCh:
			if !ok {
				if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
					return data, err
				}
				statusCh = nil
				continue
			}
			if status.State == domain.SegmentStateError {
				c.observe(ctx, domain.Event{Type: domain.EventSegmentFailed, MediaKey: info.MediaKey, Rendition: info.Rendition, Index: info.Index, Error: status.Error})
				return nil, fmt.Errorf("segment error: %s", status.Error)
			}
			data, err := c.opts.Storage.ReadSegment(ctx, info)
			if err == nil {
				c.observe(ctx, domain.Event{Type: domain.EventSegmentReady, MediaKey: info.MediaKey, Rendition: info.Rendition, Index: info.Index, Bytes: int64(len(data)), Duration: time.Since(requested)})
			}
			return data, err
		}
	}
}

func (c *Controller) readStoredSegment(ctx context.Context, info domain.SegmentData) ([]byte, bool, error) {
	exists, err := c.opts.Storage.SegmentExists(ctx, info)
	if err != nil || !exists {
		return nil, false, err
	}
	data, err := c.opts.Storage.ReadSegment(ctx, info)
	return data, true, err
}

func resolveRendition(meta *domain.Metadata, streamType domain.StreamType, name string) (*domain.VideoRendition, *domain.AudioRendition, error) {
	switch streamType {
	case domain.StreamVideo:
		for _, candidate := range rendition.GenerateVideo(meta.Video) {
			if candidate.Name == name {
				value := candidate
				return &value, nil, nil
			}
		}
	case domain.StreamAudio:
		for _, candidate := range rendition.GenerateAudioTracks(meta.Audios) {
			if candidate.Name == name {
				value := candidate
				return nil, &value, nil
			}
		}
	default:
		return nil, nil, fmt.Errorf("%w: %q", ErrInvalidStreamType, streamType)
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrRenditionNotFound, name)
}

func (c *Controller) buildJob(sourceURL, mediaKey string, streamType domain.StreamType, renditionName string, index, startIdx, endIdx int, allSegments []domain.Segment, video *domain.VideoRendition, audio *domain.AudioRendition) domain.Job {
	segments := append([]domain.Segment(nil), allSegments[startIdx:endIdx+1]...)
	expected := make([]int, len(segments))
	for i, segment := range segments {
		expected[i] = segment.Index
	}
	var overlap *domain.Segment
	if streamType == domain.StreamVideo && video != nil && video.Method == domain.DirectStream && startIdx > 0 {
		value := allSegments[startIdx-1]
		overlap = &value
	}
	now := time.Now()
	return domain.Job{
		ID:              deterministicJobID(mediaKey, renditionName, streamType, startIdx, endIdx),
		MediaKey:        mediaKey,
		Profile:         c.pipelineVersion,
		SourceURL:       sourceURL,
		Rendition:       renditionName,
		StreamType:      streamType,
		StartIndex:      startIdx,
		EndIndex:        endIdx,
		RequestedIndex:  index,
		EnqueuedAt:      now,
		Deadline:        now.Add(c.opts.JobTimeout),
		Segments:        segments,
		Overlap:         overlap,
		ExpectedIndices: expected,
		VideoRendition:  video,
		AudioRendition:  audio,
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
	meta, mediaKey, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	spriteKey := assetIdentity(mediaKey, "sprites", "160x90:5:10x10")
	return c.miscGen.GetSpriteVTT(ctx, sourceURL, spriteKey, meta.Duration, c.spriteURL(sourceURL))
}

// Sprite returns a sprite sheet image containing multiple video thumbnails.
//
// Each sprite sheet contains a grid of thumbnails extracted from the video.
// The index parameter selects which sprite sheet to return (videos are divided
// into multiple sheets based on duration).
//
// Returns JPEG image data.
func (c *Controller) Sprite(ctx context.Context, sourceURL string, index int) ([]byte, error) {
	if index < 0 {
		return nil, fmt.Errorf("sprite index out of range: %d", index)
	}
	meta, mediaKey, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	spriteKey := assetIdentity(mediaKey, "sprites", "160x90:5:10x10")
	return c.miscGen.GetSprite(ctx, sourceURL, spriteKey, meta.Duration, c.spriteURL(sourceURL), index)
}

// SubtitleVTT extracts and returns subtitles in WebVTT format.
//
// The lang parameter specifies the subtitle track language code (e.g., "en", "es").
// If the requested language is not found in the source media, an error is returned.
//
// Subtitles are extracted from the source on first request and cached.
func (c *Controller) SubtitleVTT(ctx context.Context, sourceURL string, trackID string) ([]byte, error) {
	meta, mediaKey, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	streamIndex := -1
	for _, sub := range meta.Subtitles {
		if sub.ID == trackID {
			streamIndex = sub.Index
			break
		}
	}
	if streamIndex == -1 {
		return nil, fmt.Errorf("subtitle track %s not found", trackID)
	}

	subtitleKey := assetIdentity(mediaKey, "subtitle", trackID)
	return c.miscGen.GetSubtitles(ctx, sourceURL, subtitleKey, streamIndex, trackID)
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
	cfg := c.opts.DefaultPreviewConfig
	if config != nil {
		if config.MaxFrames < 0 || config.Frameduration < 0 || config.Width < 0 {
			return nil, ErrInvalidPreviewConfig
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
	if err := validatePreviewConfig(cfg); err != nil {
		return nil, err
	}
	meta, mediaKey, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	spriteKey := assetIdentity(mediaKey, "sprites", "160x90:5:10x10")
	previewKey := assetIdentity(mediaKey, "preview", cfg)
	return c.miscGen.GetPreview(ctx, sourceURL, spriteKey, previewKey, meta.Duration, c.spriteURL(sourceURL), cfg)
}

func (c *Controller) Thumbnail(ctx context.Context, sourceURL string, config *ThumbnailConfig) ([]byte, error) {
	cfg := c.opts.DefaultThumbnailConfig
	if config != nil {
		if math.IsNaN(config.Position) || math.IsInf(config.Position, 0) || config.Position < 0 || config.Position > 1 || config.Width < 0 {
			return nil, ErrInvalidThumbnailConfig
		}
		cfg.Position = config.Position
		if config.Width > 0 {
			cfg.Width = config.Width
		}
	}
	if err := validateThumbnailConfig(cfg); err != nil {
		return nil, err
	}
	meta, mediaKey, err := c.getMetadataResolved(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}

	thumbnailKey := assetIdentity(mediaKey, "thumbnail", cfg)
	return c.miscGen.GetThumbnail(ctx, sourceURL, thumbnailKey, meta.Duration, cfg)
}

func (c *Controller) spriteURL(sourceURL string) func(int) string {
	return func(index int) string {
		return c.opts.PathGen.Sprite(sourceURL, index)
	}
}

func (c *Controller) Metadata(ctx context.Context, sourceURL string) (*Metadata, error) {
	return c.getMetadata(ctx, sourceURL)
}

func deterministicJobID(sourceURL, rendition string, streamType StreamType, startIdx, endIdx int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s:%s:%d:%d", sourceURL, rendition, streamType, startIdx, endIdx)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (c *Controller) getMetadata(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	metadata, _, err := c.getMetadataResolved(ctx, sourceURL)
	return metadata, err
}

func (c *Controller) getMetadataResolved(ctx context.Context, sourceURL string) (*domain.Metadata, string, error) {
	mediaKey, err := c.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return nil, "", err
	}
	metadata, err := c.getMetadataWithKey(ctx, sourceURL, mediaKey)
	return metadata, mediaKey, err
}

func (c *Controller) getMetadataWithKey(ctx context.Context, sourceURL, mediaKey string) (*domain.Metadata, error) {
	exists, err := c.opts.Storage.MetadataExists(ctx, mediaKey)
	if err != nil {
		return nil, err
	}
	if exists {
		c.observe(ctx, domain.Event{Type: domain.EventMetadataCacheHit, MediaKey: mediaKey})
		return c.readCachedMetadata(ctx, mediaKey)
	}
	c.observe(ctx, domain.Event{Type: domain.EventMetadataCacheMiss, MediaKey: mediaKey})

	lockKey := "metadata:" + mediaKey
	token, lockErr := c.opts.Coordinator.AcquireLock(ctx, lockKey, c.opts.ProbeTimeout+time.Minute)

	if lockErr != nil {
		if !errors.Is(lockErr, domain.ErrLockHeld) {
			return nil, fmt.Errorf("acquire metadata lock: %w", lockErr)
		}
		c.observe(ctx, domain.Event{Type: domain.EventLockWait, MediaKey: mediaKey})
		return c.waitForMetadata(ctx, mediaKey, c.opts.ProbeTimeout)
	}
	defer func() {
		cleanup, cancel := cleanupContext()
		defer cancel()
		if err := c.opts.Coordinator.ReleaseLock(cleanup, lockKey, token); err != nil {
			c.observe(cleanup, domain.Event{Type: domain.EventLockReleaseFailed, MediaKey: mediaKey, Error: err.Error()})
		}
	}()

	exists, err = c.opts.Storage.MetadataExists(ctx, mediaKey)
	if err != nil {
		return nil, err
	}
	if exists {
		return c.readCachedMetadata(ctx, mediaKey)
	}

	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, c.opts.ProbeTimeout)
	metadata, probeErr := c.prober.ProbeWithKey(probeCtx, sourceURL, mediaKey)
	cancel()
	event := domain.Event{Type: domain.EventMetadataProbed, MediaKey: mediaKey, Duration: time.Since(started)}
	if probeErr != nil {
		event.Error = probeErr.Error()
	}
	c.observe(ctx, event)
	return metadata, probeErr
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
				return nil, ErrMetadataTimeout
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
