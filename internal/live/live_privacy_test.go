package live_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/live"
)

func TestEventsCarryOnlyTheLabel(t *testing.T) {
	const sentinel = "LABEL-SENTINEL-7f3a"
	const key = "private-key-KEYSECRET"
	r := newRig(t, func(config *live.Config) {
		config.ReconnectAttempts = 2
		config.ReconnectInitial = 20 * time.Millisecond
	})
	inner, _ := source(t, "h264_aac")
	var opens atomic.Int32
	src := live.Source{Label: sentinel, Open: func(ctx context.Context) (io.ReadCloser, error) {
		if opens.Add(1) > 1 {
			return nil, errSecret
		}
		reader, err := inner.Open(ctx)
		if err != nil {
			return nil, err
		}
		return &secretReader{inner: reader, limit: 3 << 20}, nil
	}}
	session, err := r.manager.Live(context.Background(), key, src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(40 * time.Second):
		t.Fatal("session did not fail")
	}
	if state := session.State(); !errors.Is(state.Err, live.ErrSourceUnavailable) || !errors.Is(state.Err, errSecret) {
		t.Fatalf("state = %+v", state)
	}
	r.recorder.mu.Lock()
	defer r.recorder.mu.Unlock()
	if len(r.recorder.events) < 3 {
		t.Fatalf("only %d events", len(r.recorder.events))
	}
	allowed := map[string]bool{"": true, "source_unavailable": true}
	for _, event := range r.recorder.events {
		if event.label != sentinel || !allowed[event.class] || !strings.HasPrefix(event.kind, "live.") {
			t.Fatalf("event = %+v", event)
		}
		for _, field := range []string{event.kind, event.label, event.class} {
			for _, secret := range []string{"KEYSECRET", "SECRETTOKEN", "secret-host"} {
				if strings.Contains(field, secret) {
					t.Fatalf("event leaks %q: %+v", secret, event)
				}
			}
		}
	}
}

func TestFailedFFmpegErrorHoldsNeitherKeyNorDirectory(t *testing.T) {
	const key = "private-key-KEYSECRET"
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"cannot open $*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newRig(t, func(config *live.Config) {
		config.ReconnectAttempts = 2
		config.ReconnectInitial = 20 * time.Millisecond
	})
	src, _ := source(t, "h264_aac")
	session, err := r.manager.Live(context.Background(), key, src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(40 * time.Second):
		t.Fatal("session did not fail")
	}
	state := session.State()
	if state.Status != live.StatusFailed || state.Err == nil {
		t.Fatalf("state = %+v", state)
	}
	text := state.Err.Error()
	if !strings.Contains(text, "live transcoder failed") {
		t.Fatalf("error = %v", state.Err)
	}
	for _, leak := range []string{key, r.root, "goshl-live-"} {
		if strings.Contains(text, leak) {
			t.Fatalf("error leaks %q: %s", leak, text)
		}
	}
	for _, sentinel := range []error{live.ErrSourceUnavailable, live.ErrNoData, live.ErrUnsupportedCodec, live.ErrCapacity} {
		if errors.Is(state.Err, sentinel) {
			t.Fatalf("ffmpeg failure classified as %v", sentinel)
		}
	}
	r.recorder.mu.Lock()
	defer r.recorder.mu.Unlock()
	last := r.recorder.events[len(r.recorder.events)-1]
	if last.kind != live.EventFailed || last.class != "internal" {
		t.Fatalf("last event = %+v", last)
	}
}

func TestHardwareTranscodeFacts(t *testing.T) {
	config := hwaccel.DetectBestContext(context.Background())
	if config.Accelerator != domain.AccelCUDA {
		t.Skipf("NVENC is not available (accelerator %q)", config.Accelerator)
	}
	r := newRig(t, func(c *live.Config) { c.Builder = ffmpeg.NewCommandBuilder(config) })
	src, _ := source(t, "hevc_ac3")
	session, err := r.manager.Live(context.Background(), "hardware", src, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ready(t, session)
	list := playlist(t, session)
	facts := inspect(t, session, list.sequences[0])
	if facts.video != "h264" || facts.audio != "aac" || facts.height != 720 || facts.primaries != "bt709" || !facts.startsWithKeyframe {
		t.Fatalf("hardware segment facts = %+v", facts)
	}
}

func TestPlaylistHoldsReadySegmentsWhenReady(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			t.Parallel()
			r := newRig(t, nil)
			src, control := source(t, "h264_aac")
			control.Burst = 0
			options := fastOptions()
			options.ReadySegments = count
			session, err := r.manager.Live(context.Background(), "ready", src, options)
			if err != nil {
				t.Fatal(err)
			}
			ready(t, session)
			if got := len(playlist(t, session).sequences); got != count {
				t.Fatalf("playlist holds %d segments, want %d", got, count)
			}
		})
	}
}
