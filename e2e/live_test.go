package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
	"github.com/eleven-am/goshl/internal/live/livetest"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func liveController(t *testing.T, options goshl.Options) *goshl.Controller {
	t.Helper()
	options.Storage = newTimelineStorage()
	options.Coordinator = newClusterCoordinator()
	options.PathGen = stubPathGen{}
	controller := goshl.NewController(options)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("start controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	return controller
}

func liveSource(t *testing.T, kind testmedia.LiveKind) (goshl.LiveSource, *livetest.Control) {
	t.Helper()
	source, control := livetest.FileSource(testmedia.Live(t, kind), string(kind))
	control.Burst = 8 * time.Second
	return source, control
}

func segmentCodecs(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "segment.ts")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe segment: %v", err)
	}
	fields := strings.Fields(string(out))
	return strings.Join(fields, ",")
}

func TestLiveSessionsProduceH264AndAACSegments(t *testing.T) {
	testmedia.Require(t)
	controller := liveController(t, goshl.Options{})
	for _, kind := range []testmedia.LiveKind{testmedia.LiveH264AAC, testmedia.LiveMPEG2MP2} {
		source, _ := liveSource(t, kind)
		session, err := controller.Live(context.Background(), string(kind), source, goshl.LiveOptions{SegmentDuration: 2 * time.Second, WindowSegments: 4})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		playlist, err := session.Playlist(ctx)
		if err != nil {
			cancel()
			t.Fatalf("%s playlist: %v", kind, err)
		}
		if !strings.Contains(playlist, "#EXTM3U") || strings.Contains(playlist, "#EXT-X-ENDLIST") {
			t.Fatalf("%s playlist:\n%s", kind, playlist)
		}
		data, err := session.Segment(ctx, "0")
		cancel()
		if err != nil {
			t.Fatalf("%s segment: %v", kind, err)
		}
		codecs := segmentCodecs(t, data)
		if !strings.Contains(codecs, "h264") || !strings.Contains(codecs, "aac") {
			t.Fatalf("%s segment codecs = %q", kind, codecs)
		}
		session.Stop()
	}
}

func TestLiveTranscodesShareTheEncoderBudget(t *testing.T) {
	testmedia.Require(t)
	controller := liveController(t, goshl.Options{EncoderSlots: 1, LiveMaxTranscodes: 1})
	first, _ := liveSource(t, testmedia.LiveMPEG2MP2)
	holder, err := controller.Live(context.Background(), "first", first, goshl.LiveOptions{SegmentDuration: 2 * time.Second, WindowSegments: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := holder.WaitReady(ctx); err != nil {
		t.Fatalf("first transcode session: %v", err)
	}
	second, _ := liveSource(t, testmedia.LiveHEVCAC3)
	rival, err := controller.Live(context.Background(), "second", second, goshl.LiveOptions{SegmentDuration: 2 * time.Second, WindowSegments: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := rival.WaitReady(ctx); !errors.Is(err, goshl.ErrLiveCapacity) {
		t.Fatalf("second transcode session = %v, want ErrLiveCapacity", err)
	}
	copied, _ := liveSource(t, testmedia.LiveH264AAC)
	passthrough, err := controller.Live(context.Background(), "third", copied, goshl.LiveOptions{SegmentDuration: 2 * time.Second, WindowSegments: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := passthrough.WaitReady(ctx); err != nil {
		t.Fatalf("a copy session must not need a slot: %v", err)
	}
	holder.Stop()
}

func TestLiveSwitchSourceKeepsTheSessionPlaying(t *testing.T) {
	testmedia.Require(t)
	controller := liveController(t, goshl.Options{})
	first, _ := liveSource(t, testmedia.LiveH264AAC)
	session, err := controller.Live(context.Background(), "switch", first, goshl.LiveOptions{SegmentDuration: 2 * time.Second, WindowSegments: 6})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := session.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := liveSource(t, testmedia.LiveMPEG2MP2)
	if err := session.SwitchSource(ctx, second); err != nil {
		t.Fatalf("SwitchSource: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		playlist, err := session.Playlist(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(playlist, "#EXT-X-DISCONTINUITY") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no discontinuity after the switch:\n%s", playlist)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if state := session.State(); state.Status != goshl.LiveRunning {
		t.Fatalf("state = %+v", state)
	}
}
