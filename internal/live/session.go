package live

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errIdle = errors.New("live session idle")

type switchRequest struct {
	src      Source
	prepared *prepared
}

type Session struct {
	manager  *Manager
	key      string
	opts     Options
	dir      string
	ctx      context.Context
	cancel   context.CancelCauseFunc
	switchMu sync.Mutex
	switches chan switchRequest
	ready    chan struct{}
	done     chan struct{}

	mu      sync.Mutex
	label   string
	status  Status
	err     error
	win     window
	next    int
	pending bool
	touched time.Time
	slot    bool
	info    ProbeInfo
	epochs  int
}

func newSession(manager *Manager, key string, src Source, opts Options, dir string) *Session {
	ctx, cancel := context.WithCancelCause(manager.ctx)
	return &Session{
		manager:  manager,
		key:      key,
		opts:     opts,
		dir:      dir,
		ctx:      ctx,
		cancel:   cancel,
		switches: make(chan switchRequest),
		ready:    make(chan struct{}),
		done:     make(chan struct{}),
		label:    src.Label,
		status:   StatusStarting,
		win:      window{size: opts.WindowSegments, target: int(opts.SegmentDuration.Seconds() + 0.5)},
		touched:  time.Now(),
	}
}

func (s *Session) Key() string {
	return s.key
}

func (s *Session) Label() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.label
}

func (s *Session) Info() ProbeInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *Session) Epochs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epochs
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return State{Status: s.status, Err: s.err}
}

func (s *Session) Done() <-chan struct{} {
	return s.done
}

func (s *Session) Stop() {
	s.cancel(ErrStopped)
	<-s.done
}

func (s *Session) touch() {
	s.mu.Lock()
	s.touched = time.Now()
	s.mu.Unlock()
}

func (s *Session) terminal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return ErrStopped
}

func (s *Session) WaitReady(ctx context.Context) error {
	s.touch()
	select {
	case <-s.done:
		return s.terminal()
	default:
	}
	select {
	case <-s.ready:
		s.touch()
		return nil
	case <-s.done:
		return s.terminal()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) Playlist(ctx context.Context) (string, error) {
	if err := s.WaitReady(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.win.render(s.manager.config.PathGen, s.key), nil
}

func (s *Session) Segment(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-s.done:
		return nil, s.terminal()
	default:
	}
	sequence, err := sequenceOf(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrSegmentNotFound, name)
	}
	s.mu.Lock()
	item, ok := s.win.find(sequence)
	s.touched = time.Now()
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrSegmentNotFound, sequence)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, item.file))
	if err != nil {
		return nil, fmt.Errorf("%w: %d", ErrSegmentNotFound, sequence)
	}
	return data, nil
}

func sequenceOf(name string) (int, error) {
	name = strings.TrimSuffix(filepath.Base(name), ".ts")
	if cut := strings.LastIndexAny(name, "-_"); cut >= 0 {
		name = name[cut+1:]
	}
	sequence, err := strconv.Atoi(name)
	if err != nil || sequence < 0 {
		return 0, fmt.Errorf("invalid segment name")
	}
	return sequence, nil
}

func (s *Session) SwitchSource(ctx context.Context, src Source) error {
	if src.Open == nil {
		return ErrInvalidOptions
	}
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	select {
	case <-s.done:
		return s.terminal()
	default:
	}
	next, err := s.prepareSource(ctx, src)
	if err != nil {
		return err
	}
	select {
	case s.switches <- switchRequest{src: src, prepared: next}:
		s.touch()
		return nil
	case <-s.done:
		s.discard(next)
		return s.terminal()
	case <-ctx.Done():
		s.discard(next)
		return ctx.Err()
	}
}

func (s *Session) prepareSource(caller context.Context, src Source) (*prepared, error) {
	ctx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(caller, cancel)
	next, err := prepare(ctx, src, s.opts, s.manager.config.ProbeBudget)
	stop()
	if err != nil {
		cancel()
		if caller.Err() != nil {
			return nil, caller.Err()
		}
		return nil, err
	}
	next.cancel = cancel
	if err := s.reserve(next); err != nil {
		next.close()
		return nil, err
	}
	return next, nil
}

func (s *Session) watchIdle() {
	interval := min(s.opts.IdleTimeout/4, time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			idle := time.Since(s.touched)
			s.mu.Unlock()
			if idle > s.opts.IdleTimeout {
				s.cancel(errIdle)
				return
			}
		}
	}
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	s.status = StatusFailed
	s.err = err
	s.mu.Unlock()
}

func (s *Session) finish() {
	cause := context.Cause(s.ctx)
	s.cancel(ErrStopped)
	s.mu.Lock()
	failed := s.status == StatusFailed
	if !failed {
		s.status = StatusStopped
	}
	err := s.err
	s.releaseSlot()
	label := s.label
	s.mu.Unlock()
	os.RemoveAll(s.dir)
	s.manager.forget(s)
	close(s.done)
	if failed {
		s.manager.observe(EventFailed, label, err)
		return
	}
	if cause == nil {
		cause = ErrStopped
	}
	s.manager.observe(EventStopped, label, cause)
}
