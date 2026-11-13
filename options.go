package goshl

import (
	"context"
	"time"

	"github.com/eleven-am/goshl/internal/engine"
)

type SourceRevisionResolver func(context.Context, string) (string, error)

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

func (o Options) config() engine.Config {
	return engine.Config{
		Storage:                o.Storage,
		Coordinator:            o.Coordinator,
		PathGen:                o.PathGen,
		HWAccel:                o.HWAccel,
		HWAccelMode:            o.HWAccelMode,
		SegmentTimeout:         o.SegmentTimeout,
		TargetDuration:         o.TargetDuration,
		SegmentsPerJob:         o.SegmentsPerJob,
		VideoPoolSize:          o.VideoPoolSize,
		AudioPoolSize:          o.AudioPoolSize,
		DefaultPreviewConfig:   o.DefaultPreviewConfig,
		DefaultThumbnailConfig: o.DefaultThumbnailConfig,
		JobTimeout:             o.JobTimeout,
		ProbeTimeout:           o.ProbeTimeout,
		GenerationTimeout:      o.GenerationTimeout,
		SpriteStorage:          o.SpriteStorage,
		CacheVersion:           o.CacheVersion,
		SourceRevision:         engine.SourceRevisionResolver(o.SourceRevision),
		Observer:               o.Observer,
		HWAccelDecodeMode:      o.HWAccelDecodeMode,
		AllowHardwareFallback:  o.AllowHardwareFallback,
		OmitSubtitleRenditions: o.OmitSubtitleRenditions,
	}
}
