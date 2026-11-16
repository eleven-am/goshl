package live

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/encoderslots"
	"github.com/eleven-am/goshl/internal/ffmpeg"
)

const (
	EventStarted      = "live.started"
	EventReady        = "live.ready"
	EventReconnecting = "live.reconnecting"
	EventSwitched     = "live.switched"
	EventFailed       = "live.failed"
	EventStopped      = "live.stopped"
)

type Config struct {
	PathGen           domain.PathGenerator
	Builder           *ffmpeg.CommandBuilder
	Observe           func(kind, label, class string)
	Slots             *encoderslots.Budget
	MaxSessions       int
	ProbeBudget       time.Duration
	StallTimeout      time.Duration
	ReconnectAttempts int
	ReconnectInitial  time.Duration
	ReconnectMax      time.Duration
	Dir               string
}

func (c *Config) setDefaults() {
	if c.MaxSessions == 0 {
		c.MaxSessions = 16
	}
	if c.ProbeBudget == 0 {
		c.ProbeBudget = 10 * time.Second
	}
	if c.StallTimeout == 0 {
		c.StallTimeout = 10 * time.Second
	}
	if c.ReconnectAttempts == 0 {
		c.ReconnectAttempts = 5
	}
	if c.ReconnectInitial == 0 {
		c.ReconnectInitial = 500 * time.Millisecond
	}
	if c.ReconnectMax == 0 {
		c.ReconnectMax = 5 * time.Second
	}
}

type Manager struct {
	config Config

	mu       sync.Mutex
	sessions map[string]*Session
	stopped  bool
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewManager(config Config) *Manager {
	config.setDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{config: config, sessions: make(map[string]*Session), ctx: ctx, cancel: cancel}
}

func (m *Manager) Live(ctx context.Context, key string, src Source, opts Options) (*Session, error) {
	if key == "" || src.Open == nil {
		return nil, ErrInvalidOptions
	}
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, ErrStopped
	}
	if existing, ok := m.sessions[key]; ok {
		existing.touch()
		return existing, nil
	}
	if len(m.sessions) >= m.config.MaxSessions {
		return nil, ErrCapacity
	}
	dir, err := sessionDir(m.config.Dir)
	if err != nil {
		return nil, err
	}
	session := newSession(m, key, src, opts, dir)
	m.sessions[key] = session
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		session.run(src)
	}()
	return session, nil
}

func (m *Manager) Probe(ctx context.Context, src Source, budget time.Duration) (ProbeInfo, error) {
	if src.Open == nil {
		return ProbeInfo{}, ErrInvalidOptions
	}
	m.mu.Lock()
	stopped := m.stopped
	m.mu.Unlock()
	if stopped {
		return ProbeInfo{}, ErrStopped
	}
	if budget <= 0 {
		budget = m.config.ProbeBudget
	}
	options := Options{}
	options.setDefaults()
	probed, err := prepare(ctx, src, options, budget)
	if err != nil {
		return ProbeInfo{}, err
	}
	probed.reader.Close()
	return probed.info, nil
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *Manager) forget(session *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[session.key] == session {
		delete(m.sessions, session.key)
	}
}

func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) observe(kind, label string, err error) {
	if m.config.Observe == nil {
		return
	}
	class := ""
	if err != nil {
		class = classify(err)
	}
	m.config.Observe(kind, label, class)
}

func classify(err error) string {
	switch {
	case errors.Is(err, ErrCapacity):
		return "capacity"
	case errors.Is(err, ErrUnsupportedCodec):
		return "unsupported_codec"
	case errors.Is(err, ErrNoData):
		return "no_data"
	case errors.Is(err, ErrSourceUnavailable):
		return "source_unavailable"
	case errors.Is(err, errIdle):
		return "idle"
	case errors.Is(err, ErrStopped), errors.Is(err, context.Canceled):
		return "stopped"
	}
	return "internal"
}

func sessionDir(parent string) (string, error) {
	dir, err := os.MkdirTemp(parent, "goshl-live-*")
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return "", fmt.Errorf("live: create session directory: %w", pathErr.Err)
		}
		return "", errors.New("live: create session directory failed")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", errors.New("live: resolve session directory failed")
	}
	return abs, nil
}
