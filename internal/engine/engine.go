package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/coordination"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/encoderslots"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/ffmpegversion"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/live"
	"github.com/eleven-am/goshl/internal/misc"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/probe"
	"github.com/eleven-am/goshl/internal/segment"
	"github.com/eleven-am/goshl/internal/transcode"
)

type Engine struct {
	opts            Config
	playlist        *playlist.Generator
	videoPool       *transcode.Pool
	audioPool       *transcode.Pool
	channelPool     *transcode.Pool
	channel         channelState
	routeProfile    string
	prober          *probe.Prober
	metadata        metadataState
	miscGen         *misc.Generator
	hwConfig        *domain.HWAccelConfig
	pipelineVersion string
	flights         *flightRegistry
	lifecycle       *coordination.Lifecycle
	tonemap         tonemapCapability
	live            *live.Manager
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

func New(ctx context.Context, opts Config) (*Engine, error) {
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
	route := routeProfile(pipeline)

	slots := encoderslots.New(opts.EncoderSlots, min(opts.LiveMaxTranscodes, opts.EncoderSlots))

	notifyingStorage := segment.NewNotifyingStorage(opts.Storage, opts.Coordinator, opts.Observer)

	videoPool := transcode.NewPool(
		opts.Coordinator,
		opts.VideoPoolSize,
		domain.StreamVideo,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
		transcode.PoolConfig{JobTimeout: opts.JobTimeout, Observer: opts.Observer, Profile: route, Slots: slots},
	)

	audioPool := transcode.NewPool(
		opts.Coordinator,
		opts.AudioPoolSize,
		domain.StreamAudio,
		opts.Storage,
		cmdBuilder,
		notifyingStorage,
		transcode.PoolConfig{JobTimeout: opts.JobTimeout, Observer: opts.Observer, Profile: route},
	)

	miscGen := misc.NewGeneratorWithCoordinator(opts.Storage, opts.Coordinator, hwConfig, opts.Observer)
	miscGen.SetSpriteStorage(opts.SpriteStorage)
	miscGen.SetGenerationTimeout(opts.GenerationTimeout)
	lifecycle := coordination.NewLifecycle()
	miscGen.SetLifecycle(lifecycle)

	engine := &Engine{
		opts:            opts,
		playlist:        playlist.NewGenerator(opts.PathGen),
		videoPool:       videoPool,
		audioPool:       audioPool,
		prober:          probe.NewProber(),
		miscGen:         miscGen,
		hwConfig:        hwConfig,
		pipelineVersion: pipeline,
		routeProfile:    route,
		channelPool:     newChannelPool(opts, cmdBuilder, notifyingStorage, route),
		flights:         newFlightRegistry(),
		lifecycle:       lifecycle,
	}
	engine.live = live.NewManager(live.Config{
		PathGen:     opts.PathGen,
		Builder:     cmdBuilder,
		Observe:     engine.observeLive,
		Slots:       slots,
		MaxSessions: opts.LiveMaxSessions,
	})
	engine.observe(ctx, domain.Event{Type: domain.EventHardwareSelected, Accelerator: hwConfig.Accelerator})
	if hwConfig.Error != "" {
		engine.observe(ctx, domain.Event{Type: domain.EventHardwareFallback, Accelerator: hwConfig.Accelerator, Error: hwConfig.Error})
	}
	return engine, nil
}

func (e *Engine) HWAccelConfig() domain.HWAccelConfig {
	if e.hwConfig == nil {
		return *hwaccel.NewConfig(domain.AccelNone)
	}
	config := *e.hwConfig
	config.Available = append([]domain.Accelerator(nil), e.hwConfig.Available...)
	config.DecodeFlags = append([]string(nil), e.hwConfig.DecodeFlags...)
	config.EncodeFlags = append([]string(nil), e.hwConfig.EncodeFlags...)
	return config
}

func (e *Engine) observe(ctx context.Context, event domain.Event) {
	if e.opts.Observer == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	e.opts.Observer.Observe(ctx, event)
}

func (e *Engine) Start(ctx context.Context) error {
	if e.lifecycle.Stopped() {
		return ErrControllerStopped
	}
	if err := e.videoPool.Start(ctx); err != nil {
		return fmt.Errorf("start video pool: %w", err)
	}
	if err := e.audioPool.Start(ctx); err != nil {
		e.videoPool.Stop()
		return fmt.Errorf("start audio pool: %w", err)
	}
	if err := e.startChannelPool(ctx); err != nil {
		e.videoPool.Stop()
		e.audioPool.Stop()
		return err
	}
	if e.lifecycle.Stopped() {
		e.videoPool.Stop()
		e.audioPool.Stop()
		e.stopChannelPool()
		return ErrControllerStopped
	}
	return nil
}

const stopWaitBound = 10 * time.Second

func (e *Engine) Stop() {
	e.lifecycle.Close()
	e.live.Stop()
	e.videoPool.Stop()
	e.audioPool.Stop()
	e.stopChannelPool()
	e.lifecycle.Wait(stopWaitBound)
	e.finishAllFlights()
}

func (e *Engine) ensureRunning() error {
	if e.lifecycle.Stopped() {
		return ErrControllerStopped
	}
	return nil
}
