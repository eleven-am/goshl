package goshl

import (
	"fmt"
	"math"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

const (
	minTargetDuration = 1.0
	maxTargetDuration = 60.0
	maxSegmentsPerJob = 1000
	maxPoolSize       = 1024
)

type Options struct {
	Storage Storage

	Coordinator Coordinator

	PathGen PathGenerator

	HWAccel bool

	HWAccelMode Accelerator

	SegmentTimeout time.Duration

	TargetDuration float64

	SegmentsPerJob int

	VideoPoolSize int

	AudioPoolSize int

	DefaultPreviewConfig PreviewConfig

	DefaultThumbnailConfig ThumbnailConfig

	JobTimeout time.Duration

	ProbeTimeout time.Duration

	GenerationTimeout time.Duration

	SpriteStorage SpriteStorage

	CacheVersion string

	SourceRevision SourceRevisionResolver

	Observer Observer

	HWAccelDecodeMode DecodeMode

	AllowHardwareFallback bool

	OmitSubtitleRenditions bool
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
	if o.ProbeTimeout == 0 {
		o.ProbeTimeout = o.JobTimeout
	}
	if o.GenerationTimeout == 0 {
		o.GenerationTimeout = 10 * time.Minute
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
	if o.SegmentTimeout < 0 || o.JobTimeout < 0 || o.ProbeTimeout < 0 || o.GenerationTimeout < 0 || o.TargetDuration < 0 || math.IsNaN(o.TargetDuration) || math.IsInf(o.TargetDuration, 0) {
		return fmt.Errorf("%w: durations must be positive", ErrInvalidOptions)
	}
	if o.SegmentsPerJob < 0 || o.VideoPoolSize < 0 || o.AudioPoolSize < 0 {
		return fmt.Errorf("%w: pool and batch sizes must be positive", ErrInvalidOptions)
	}
	if err := validatePreviewConfig(o.DefaultPreviewConfig, true); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	if err := validateThumbnailConfig(o.DefaultThumbnailConfig, true); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	if o.HWAccelDecodeMode != "" && o.HWAccelDecodeMode != domain.DecodeSafe && o.HWAccelDecodeMode != domain.DecodeDevice {
		return fmt.Errorf("%w: unsupported hardware decode mode %q", ErrInvalidOptions, o.HWAccelDecodeMode)
	}
	return nil
}

func (o *Options) validateResolvedError() error {
	if o.SegmentTimeout <= 0 || o.JobTimeout <= 0 || o.ProbeTimeout <= 0 || o.TargetDuration <= 0 {
		return fmt.Errorf("%w: durations must be positive", ErrInvalidOptions)
	}
	if o.SegmentsPerJob <= 0 || o.VideoPoolSize <= 0 || o.AudioPoolSize <= 0 {
		return fmt.Errorf("%w: pool and batch sizes must be positive", ErrInvalidOptions)
	}
	if o.TargetDuration < minTargetDuration || o.TargetDuration > maxTargetDuration {
		return fmt.Errorf("%w: TargetDuration must be between %g and %g seconds", ErrInvalidOptions, minTargetDuration, maxTargetDuration)
	}
	if o.SegmentsPerJob > maxSegmentsPerJob || o.VideoPoolSize > maxPoolSize || o.AudioPoolSize > maxPoolSize {
		return fmt.Errorf("%w: SegmentsPerJob is limited to %d and pool sizes to %d", ErrInvalidOptions, maxSegmentsPerJob, maxPoolSize)
	}
	if err := validatePreviewConfig(o.DefaultPreviewConfig, false); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	if err := validateThumbnailConfig(o.DefaultThumbnailConfig, false); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	return nil
}

func validatePreviewConfig(config domain.PreviewConfig, allowUnset bool) error {
	if !validIntField(config.MaxFrames, 1000, allowUnset) || !validIntField(config.Frameduration, 60_000, allowUnset) || !validIntField(config.Width, 4096, allowUnset) {
		return ErrInvalidPreviewConfig
	}
	return nil
}

func validateThumbnailConfig(config domain.ThumbnailConfig, allowUnset bool) error {
	if config.Position < 0 || config.Position > 1 || math.IsNaN(config.Position) || math.IsInf(config.Position, 0) || !validIntField(config.Width, 8192, allowUnset) {
		return ErrInvalidThumbnailConfig
	}
	return nil
}

func validIntField(value, max int, allowUnset bool) bool {
	if value == 0 {
		return allowUnset
	}
	return value > 0 && value <= max
}
