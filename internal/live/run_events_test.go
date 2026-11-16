package live_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/live"
	"github.com/eleven-am/goshl/internal/live/livetest"
)

func (r *recorder) count(kind string) int {
	total := 0
	for _, name := range r.types() {
		if name == kind {
			total++
		}
	}
	return total
}

func TestSwitchDuringReconnectPauseEmitsSwitchedOnce(t *testing.T) {
	r := newRig(t, func(config *live.Config) {
		config.ReconnectInitial = 3 * time.Second
		config.ReconnectMax = 3 * time.Second
	})
	first, control := source(t, "h264_aac")
	session, err := r.manager.Live(context.Background(), "pause-switch", first, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	control.RefuseOpens(1000)
	control.Fail()
	waitFor(t, 20*time.Second, "paused between reconnect attempts", func() bool {
		return r.recorder.count(live.EventReconnecting) >= 2
	})
	second, _ := source(t, "mpeg2i_mp2")
	if err := session.SwitchSource(context.Background(), second); err != nil {
		t.Fatalf("SwitchSource: %v", err)
	}
	waitFor(t, 30*time.Second, "second source adopted", func() bool {
		return session.Label() == "mpeg2i_mp2" && session.Epochs() >= 2
	})
	time.Sleep(500 * time.Millisecond)
	if got := r.recorder.count(live.EventSwitched); got != 1 {
		t.Fatalf("%d switched events, want 1: %v", got, r.recorder.types())
	}
	r.recorder.mu.Lock()
	defer r.recorder.mu.Unlock()
	for _, event := range r.recorder.events {
		if event.kind == live.EventSwitched && event.label != "mpeg2i_mp2" {
			t.Fatalf("switched label = %q", event.label)
		}
	}
}

func TestInterlacedOnDeviceFramesFailsWithoutReconnect(t *testing.T) {
	r := newRig(t, func(config *live.Config) {
		config.Builder = ffmpeg.NewCommandBuilder(hwaccel.NewConfigWithMode(domain.AccelCUDA, domain.DecodeDevice))
		config.ReconnectInitial = 50 * time.Millisecond
		config.ReconnectMax = 100 * time.Millisecond
	})
	src, control := livetest.FileSource(fixture(t, "mpeg2i_mp2"), "interlaced")
	control.Burst = 5 * time.Second
	session, err := r.manager.Live(context.Background(), "interlaced-device", src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("session did not fail")
	}
	if state := session.State(); state.Status != live.StatusFailed || !errors.Is(state.Err, live.ErrUnsupportedCodec) {
		t.Fatalf("state = %+v", state)
	}
	if got := r.recorder.count(live.EventReconnecting); got != 0 {
		t.Fatalf("%d reconnecting events: %v", got, r.recorder.types())
	}
	if control.Opens() != 1 {
		t.Fatalf("opens = %d", control.Opens())
	}
}
