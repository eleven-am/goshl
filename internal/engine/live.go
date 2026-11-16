package engine

import (
	"context"
	"errors"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/live"
)

func (e *Engine) observeLive(kind, label, class string) {
	e.observe(context.Background(), domain.Event{Type: domain.EventType(kind), Label: label, Error: class})
}

func (e *Engine) Live(ctx context.Context, key string, src live.Source, opts live.Options) (*live.Session, error) {
	if err := e.ensureRunning(); err != nil {
		return nil, err
	}
	session, err := e.live.Live(ctx, key, src, opts)
	if errors.Is(err, live.ErrStopped) {
		return nil, ErrControllerStopped
	}
	return session, err
}

func (e *Engine) LiveProbe(ctx context.Context, src live.Source, budget time.Duration) (live.ProbeInfo, error) {
	if err := e.ensureRunning(); err != nil {
		return live.ProbeInfo{}, err
	}
	info, err := e.live.Probe(ctx, src, budget)
	if errors.Is(err, live.ErrStopped) {
		return live.ProbeInfo{}, ErrControllerStopped
	}
	return info, err
}
