package coordination

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"golang.org/x/sync/singleflight"
)

var ErrStopped = errors.New("controller stopped")

type Lifecycle struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	active  int
	idle    chan struct{}
}

func NewLifecycle() *Lifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &Lifecycle{ctx: ctx, cancel: cancel}
}

func (l *Lifecycle) Context() context.Context {
	return l.ctx
}

func (l *Lifecycle) Stopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

func (l *Lifecycle) Enter() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return false
	}
	l.active++
	return true
}

func (l *Lifecycle) Exit() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.active == 0 && l.idle != nil {
		close(l.idle)
		l.idle = nil
	}
}

func (l *Lifecycle) Close() {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()
	l.cancel()
}

func (l *Lifecycle) Wait(bound time.Duration) bool {
	l.mu.Lock()
	if l.active == 0 {
		l.mu.Unlock()
		return true
	}
	if l.idle == nil {
		l.idle = make(chan struct{})
	}
	idle := l.idle
	l.mu.Unlock()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

type Runner struct {
	Lifecycle   *Lifecycle
	Kind        string
	Coordinator domain.Coordinator
	Timeout     func() time.Duration
	Observe     func(ctx context.Context, event domain.Event)
}

func (r Runner) Once(ctx context.Context, group *singleflight.Group, key string, ready func(context.Context) (bool, error), generate func(context.Context) error) error {
	base := context.Background()
	if r.Lifecycle != nil {
		if r.Lifecycle.Stopped() {
			return ErrStopped
		}
		base = r.Lifecycle.Context()
	}
	result := group.DoChan(key, func() (any, error) {
		if r.Lifecycle != nil {
			if !r.Lifecycle.Enter() {
				return nil, ErrStopped
			}
			defer r.Lifecycle.Exit()
		}
		runCtx, cancel := context.WithTimeout(base, r.Timeout())
		defer cancel()
		return nil, r.Coordinate(runCtx, key, ready, generate)
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outcome := <-result:
		if outcome.Err != nil && r.Lifecycle != nil && r.Lifecycle.Stopped() {
			return ErrStopped
		}
		return outcome.Err
	}
}

var ErrMissingKind = errors.New("coordination: runner kind is required")

func (r Runner) Coordinate(ctx context.Context, key string, ready func(context.Context) (bool, error), generate func(context.Context) error) error {
	if r.Kind == "" {
		return ErrMissingKind
	}
	if r.Coordinator == nil {
		isReady, err := ready(ctx)
		if err != nil || isReady {
			return err
		}
		return generate(ctx)
	}
	kind := r.Kind
	lockKey := kind + ":" + key
	ttl := r.Timeout() + time.Minute
	runLocked := func(token string) error {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if releaseErr := r.Coordinator.ReleaseLock(cleanup, lockKey, token); releaseErr != nil {
				r.Observe(cleanup, domain.Event{Type: domain.EventLockReleaseFailed, MediaKey: key, Error: releaseErr.Error()})
			}
		}()
		isReady, err := ready(ctx)
		if err != nil || isReady {
			return err
		}
		return generate(ctx)
	}
	token, err := r.Coordinator.AcquireLock(ctx, lockKey, ttl)
	if err == nil {
		return runLocked(token)
	}
	if !errors.Is(err, domain.ErrLockHeld) {
		return fmt.Errorf("acquire %s lock: %w", kind, err)
	}
	r.Observe(ctx, domain.Event{Type: domain.EventLockWait, MediaKey: key})
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			isReady, readyErr := ready(ctx)
			if readyErr != nil {
				return readyErr
			}
			if isReady {
				return nil
			}
			token, err := r.Coordinator.AcquireLock(ctx, lockKey, ttl)
			if err == nil {
				return runLocked(token)
			}
			if !errors.Is(err, domain.ErrLockHeld) {
				return fmt.Errorf("acquire %s lock: %w", kind, err)
			}
		}
	}
}
