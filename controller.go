package goshl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/coordination"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/ffmpegversion"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/misc"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/segment"
	"github.com/eleven-am/goshl/internal/transcode"
)

type Controller struct {
	opts            Options
	playlist        *playlist.Generator
	videoPool       *transcode.Pool
	audioPool       *transcode.Pool
	prober          *probe.Prober
	metadata        metadataState
	miscGen         *misc.Generator
	hwConfig        *domain.HWAccelConfig
	pipelineVersion string
	flights         *flightRegistry
	lifecycle       *coordination.Lifecycle
	tonemap         tonemapCapability
}

type tonemapCapability struct {
	mu     sync.Mutex
	known  bool
	err    error
	detect func(context.Context) (string, error)
}

func (t *tonemapCapability) check(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.known {
		return t.err
	}
	detect := t.detect
	if detect == nil {
		detect = hwaccel.DetectTonemap
	}
	_, err := detect(ctx)
	if err == nil || errors.Is(err, hwaccel.ErrTonemapUnavailable) {
		t.known = true
		t.err = err
	}
	return err
}

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
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if err := ffmpegversion.Require(ctx, binary, ffmpegversion.Minimum()); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
		}
	}

	var hwConfig *domain.HWAccelConfig
	if opts.HWAccelMode != "" {
		var err error
		hwConfig, err = hwaccel.RequireConfigWithMode(opts.HWAccelMode, opts.HWAccelDecodeMode)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
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

	videoPool := transcode.NewPool(
		opts.Coordinator,
		opts.VideoPoolSize,
		domain.StreamVideo,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
		transcode.PoolConfig{JobTimeout: opts.JobTimeout, Observer: opts.Observer, Profile: pipeline},
	)

	audioPool := transcode.NewPool(
		opts.Coordinator,
		opts.AudioPoolSize,
		domain.StreamAudio,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
		transcode.PoolConfig{JobTimeout: opts.JobTimeout, Observer: opts.Observer, Profile: pipeline},
	)

	miscGen := misc.NewGeneratorWithCoordinator(opts.Storage, opts.Coordinator, hwConfig, opts.Observer)
	miscGen.SetSpriteStorage(opts.SpriteStorage)
	miscGen.SetGenerationTimeout(opts.GenerationTimeout)
	lifecycle := coordination.NewLifecycle()
	miscGen.SetLifecycle(lifecycle)

	controller := &Controller{
		opts:            opts,
		playlist:        playlist.NewGenerator(opts.PathGen),
		videoPool:       videoPool,
		audioPool:       audioPool,
		prober:          probe.NewProber(),
		miscGen:         miscGen,
		hwConfig:        hwConfig,
		pipelineVersion: pipeline,
		flights:         newFlightRegistry(),
		lifecycle:       lifecycle,
	}
	controller.observe(ctx, domain.Event{Type: domain.EventHardwareSelected, Accelerator: hwConfig.Accelerator})
	if hwConfig.Error != "" {
		controller.observe(ctx, domain.Event{Type: domain.EventHardwareFallback, Accelerator: hwConfig.Accelerator, Error: hwConfig.Error})
	}
	return controller, nil
}

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

func (c *Controller) Start(ctx context.Context) error {
	if c.lifecycle.Stopped() {
		return ErrControllerStopped
	}
	if err := c.videoPool.Start(ctx); err != nil {
		return fmt.Errorf("start video pool: %w", err)
	}
	if err := c.audioPool.Start(ctx); err != nil {
		c.videoPool.Stop()
		return fmt.Errorf("start audio pool: %w", err)
	}
	if c.lifecycle.Stopped() {
		c.videoPool.Stop()
		c.audioPool.Stop()
		return ErrControllerStopped
	}
	return nil
}

const stopWaitBound = 10 * time.Second

func (c *Controller) Stop() {
	c.lifecycle.Close()
	c.videoPool.Stop()
	c.audioPool.Stop()
	c.lifecycle.Wait(stopWaitBound)
	c.finishAllFlights()
}

func (c *Controller) ensureRunning() error {
	if c.lifecycle.Stopped() {
		return ErrControllerStopped
	}
	return nil
}
