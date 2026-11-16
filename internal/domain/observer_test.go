package domain

import (
	"context"
	"testing"
)

func TestLiveEventTypes(t *testing.T) {
	cases := map[EventType]string{
		EventLiveStarted:      "live.started",
		EventLiveReady:        "live.ready",
		EventLiveReconnecting: "live.reconnecting",
		EventLiveSwitched:     "live.switched",
		EventLiveFailed:       "live.failed",
		EventLiveStopped:      "live.stopped",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("event type %q, want %q", got, want)
		}
	}
}

func TestObserverFuncDeliversLabel(t *testing.T) {
	var got Event
	var obs Observer = ObserverFunc(func(_ context.Context, e Event) { got = e })
	obs.Observe(context.Background(), Event{Type: EventLiveReady, Label: "main"})
	if got.Type != EventLiveReady || got.Label != "main" {
		t.Fatalf("unexpected event %+v", got)
	}
}
