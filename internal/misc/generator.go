package misc

import (
	"context"
	"time"

	"github.com/eleven-am/goshl/internal/coordination"
	"github.com/eleven-am/goshl/internal/domain"
	"golang.org/x/sync/singleflight"
)

const (
	defaultThumbWidth  = 160
	defaultThumbHeight = 90
	defaultInterval    = 5.0
	defaultCols        = 10
	defaultRows        = 10

	defaultGenerationTimeout = 10 * time.Minute
)

type Generator struct {
	storage     domain.Storage
	spriteStore domain.SpriteStorage
	hwAccel     *domain.HWAccelConfig
	sprites     singleflight.Group
	previews    singleflight.Group
	thumbnails  singleflight.Group
	subtitles   singleflight.Group
	coordinator domain.Coordinator
	observer    domain.Observer
	lifecycle   *coordination.Lifecycle

	generationTimeout time.Duration
	extractSubtitles  func(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) error

	thumbWidth  int
	thumbHeight int
	interval    float64
	cols        int
	rows        int
}

func NewGeneratorWithCoordinator(storage domain.Storage, coordinator domain.Coordinator, hwAccel *domain.HWAccelConfig, observers ...domain.Observer) *Generator {
	generator := NewGenerator(storage, hwAccel)
	generator.coordinator = coordinator
	if len(observers) > 0 {
		generator.observer = observers[0]
	}
	return generator
}

func NewGenerator(storage domain.Storage, hwAccel ...*domain.HWAccelConfig) *Generator {
	cfg := domain.HWAccelConfig{Accelerator: domain.AccelNone}
	if len(hwAccel) > 0 && hwAccel[0] != nil {
		cfg = *hwAccel[0]
	}

	return &Generator{
		storage:     storage,
		spriteStore: storage,
		hwAccel:     &cfg,
		thumbWidth:  defaultThumbWidth,
		thumbHeight: defaultThumbHeight,
		interval:    defaultInterval,
		cols:        defaultCols,
		rows:        defaultRows,
	}
}

func (g *Generator) timeout() time.Duration {
	if g.generationTimeout > 0 {
		return g.generationTimeout
	}
	return defaultGenerationTimeout
}

func (g *Generator) SetLifecycle(lifecycle *coordination.Lifecycle) {
	g.lifecycle = lifecycle
}

func (g *Generator) SetGenerationTimeout(timeout time.Duration) {
	g.generationTimeout = timeout
}

func (g *Generator) observe(ctx context.Context, event domain.Event) {
	if g.observer == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	g.observer.Observe(ctx, event)
}

func (g *Generator) runner() coordination.Runner {
	return coordination.Runner{Lifecycle: g.lifecycle, Kind: "asset", Coordinator: g.coordinator, Timeout: g.timeout, Observe: g.observe}
}
