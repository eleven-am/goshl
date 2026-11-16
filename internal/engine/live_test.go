package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/live"
)

type liveEventLog struct {
	mu     sync.Mutex
	events []domain.Event
}

func (l *liveEventLog) Observe(_ context.Context, event domain.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func TestLiveEventsCarryOnlyTypeTimeLabelAndClass(t *testing.T) {
	log := &liveEventLog{}
	built := newChannelEngine(&stubStorage{}, newClusterCoordinator(), func(c *Config) { c.Observer = log })
	defer built.Stop()
	built.observeLive(live.EventReconnecting, "channel one", "source_unavailable")
	built.observeLive(live.EventStarted, "channel one", "")
	log.mu.Lock()
	defer log.mu.Unlock()
	var live []domain.Event
	for _, event := range log.events {
		if event.Label != "" {
			live = append(live, event)
		}
	}
	if len(live) != 2 {
		t.Fatalf("live events = %+v", live)
	}
	want := domain.Event{Type: domain.EventLiveReconnecting, Label: "channel one", Error: "source_unavailable"}
	got := live[0]
	if got.Time.IsZero() || time.Since(got.Time) > time.Minute {
		t.Errorf("event time = %v", got.Time)
	}
	got.Time = time.Time{}
	if got != want {
		t.Errorf("event = %+v, want %+v", got, want)
	}
	if live[1].Type != domain.EventLiveStarted || live[1].Error != "" {
		t.Errorf("started event = %+v", live[1])
	}
}

func TestLiveRejectsACallAfterStop(t *testing.T) {
	built := newChannelEngine(&stubStorage{}, newClusterCoordinator(), nil)
	built.Stop()
	if _, err := built.Live(context.Background(), "key", live.Source{}, live.Options{}); !errors.Is(err, ErrControllerStopped) {
		t.Errorf("Live after Stop = %v", err)
	}
	if _, err := built.LiveProbe(context.Background(), live.Source{}, time.Second); !errors.Is(err, ErrControllerStopped) {
		t.Errorf("LiveProbe after Stop = %v", err)
	}
}
