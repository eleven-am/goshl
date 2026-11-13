package probe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
)

func TestProbeMatroskaUsesCueIndexAndMatchesPacketScan(t *testing.T) {
	fixtures := map[string][]string{
		"indexed.mkv":  {"-bf", "2"},
		"offset.mkv":   {"-bf", "2", "-output_ts_offset", "5"},
		"indexed.webm": {"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8"},
	}
	for name, extra := range fixtures {
		t.Run(name, func(t *testing.T) {
			logPath := ffprobeCallLog(t)
			path := makeMedia(t, name, extra...)
			meta, err := NewProber().Probe(context.Background(), path)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if got := packetPassCount(t, logPath); got != 0 {
				t.Fatalf("packet pass ran %d times for a matroska file with cues", got)
			}
			want := referenceKeyframes(t, path, meta)
			if len(want) != 6 || fmt.Sprint(meta.Keyframes) != fmt.Sprint(want) {
				t.Fatalf("keyframes %v, packet scan has %v", meta.Keyframes, want)
			}
			if !meta.Video.StartsWithKeyframe {
				t.Fatal("expected StartsWithKeyframe")
			}
		})
	}
}

func TestProbeIndexesRemoteMatroskaWithoutFullDownload(t *testing.T) {
	path := largeMP4(t, "remote.mkv")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewProber().Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("local probe: %v", err)
	}
	server := serveRanges(t, path, math.MaxInt64)
	remote, err := NewProber().Probe(context.Background(), server.URL+"/remote.mkv")
	if err != nil {
		t.Fatalf("remote probe: %v", err)
	}
	if len(remote.Keyframes) != 30 || fmt.Sprint(remote.Keyframes) != fmt.Sprint(local.Keyframes) {
		t.Fatalf("remote keyframes %v, local %v", remote.Keyframes, local.Keyframes)
	}
	if served, limit := server.served.Load(), info.Size()/5; served > limit {
		t.Fatalf("probe downloaded %d of %d bytes in %d requests, limit %d", served, info.Size(), server.requests.Load(), limit)
	}
	if reads := server.goReads.Load(); reads > 3 {
		t.Fatalf("cue index issued %d range requests, want size probe, head and cues", reads)
	}
}

func TestProbeFailsWhenMatroskaIndexCannotBeRead(t *testing.T) {
	path := largeMP4(t, "offline.mkv")
	for _, healthyReads := range []int64{1, 2} {
		server := serveRanges(t, path, healthyReads)
		_, err := NewProber().Probe(context.Background(), server.URL+"/offline.mkv")
		if err == nil {
			t.Fatalf("probe hid a failed index read after %d good reads behind the packet scan", healthyReads)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}
