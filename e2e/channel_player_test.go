package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
)

const (
	channelPlayerDirEnv = "GOSHL_CHANNEL_PLAYER_DIR"
	channelChromeEnv    = "GOSHL_CHANNEL_CHROME"
	channelClipSegments = 6
	channelWindow       = 8
	channelPublishLead  = 3
)

type channelEntry struct {
	source        channelSource
	index         int
	duration      float64
	discontinuity bool
	end           float64
}

type channelBroadcast struct {
	node    *channelNode
	entries []channelEntry
	mu      sync.Mutex
	epoch   time.Time
	reports []map[string]any
	served  map[int]int
}

func (b *channelBroadcast) restart() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.epoch = time.Now()
	b.reports = nil
	b.served = make(map[int]int)
}

func (b *channelBroadcast) playlist() string {
	b.mu.Lock()
	elapsed := time.Since(b.epoch).Seconds()
	b.mu.Unlock()
	lead := float64(channelPublishLead) * b.entries[0].duration
	published := 0
	for published < len(b.entries) && b.entries[published].end <= elapsed+lead {
		published++
	}
	first := published - channelWindow
	if first < 0 {
		first = 0
	}
	sequence := 0
	for _, entry := range b.entries[:first+1] {
		if entry.discontinuity {
			sequence++
		}
	}
	target := 0.0
	for _, entry := range b.entries {
		target = math.Max(target, entry.duration)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", int(math.Ceil(target)), first, sequence)
	for position := first; position < published; position++ {
		entry := b.entries[position]
		if entry.discontinuity && position > first {
			out.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&out, "#EXTINF:%.6f,\nsegment/%d.ts\n", entry.duration, position)
	}
	if published == len(b.entries) {
		out.WriteString("#EXT-X-ENDLIST\n")
	}
	return out.String()
}

func (b *channelBroadcast) handler(playerPage string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, b.playlist())
	})
	mux.HandleFunc("/segment/", func(w http.ResponseWriter, r *http.Request) {
		position, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/segment/"), ".ts"))
		if err != nil || position < 0 || position >= len(b.entries) {
			http.NotFound(w, r)
			return
		}
		entry := b.entries[position]
		data, err := b.node.controller.ChannelSegment(r.Context(), entry.source.path, goshl.ChannelProfile{}, 0, entry.index)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		b.mu.Lock()
		b.served[position]++
		b.mu.Unlock()
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(data)
	})
	mux.HandleFunc("/player.html", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, playerPage)
	})
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		var report map[string]any
		if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
			b.mu.Lock()
			b.reports = append(b.reports, report)
			b.mu.Unlock()
		}
	})
	return mux
}

func TestChannelLibraryPlaysInRealPlayers(t *testing.T) {
	sources := channelSources(t)
	playerDir := os.Getenv(channelPlayerDirEnv)
	chrome := os.Getenv(channelChromeEnv)
	if playerDir == "" || chrome == "" {
		t.Skipf("%s and %s are not set", channelPlayerDirEnv, channelChromeEnv)
	}
	var lines []string
	ctx := context.Background()
	node := startChannelNode(t, newTimelineStorage(), 10, func(options *goshl.Options) { options.ChannelPoolSize = 3 })
	broadcast := &channelBroadcast{node: node}

	type clip struct {
		source channelSource
		plan   goshl.ChannelPlan
		from   int
	}
	plans := make([]goshl.ChannelPlan, len(sources))
	for position, source := range sources {
		plan, err := node.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
		if err != nil {
			t.Fatalf("%s: plan: %v", source.key, err)
		}
		plans[position] = plan
	}
	var clips []clip
	for position := range sources {
		next := (position + 1) % len(sources)
		clips = append(clips, clip{sources[position], plans[position], len(plans[position].Segments) - channelClipSegments})
		clips = append(clips, clip{sources[next], plans[next], 0})
	}
	generation := time.Now()
	for _, piece := range clips {
		if err := node.controller.ChannelPrefetch(ctx, piece.source.path, goshl.ChannelProfile{}, 0, piece.from, channelClipSegments); err != nil {
			t.Fatalf("%s: prefetch: %v", piece.source.key, err)
		}
	}
	clock := 0.0
	var order []string
	for position, piece := range clips {
		waitChannelStored(t, node, piece.plan, piece.from, piece.from+channelClipSegments-1)
		for offset := 0; offset < channelClipSegments; offset++ {
			segment := piece.plan.Segments[piece.from+offset]
			clock += segment.Duration
			broadcast.entries = append(broadcast.entries, channelEntry{source: piece.source, index: segment.Index, duration: segment.Duration, discontinuity: offset == 0 && position > 0, end: clock})
		}
		order = append(order, fmt.Sprintf("%s %d-%d", piece.source.key, piece.from, piece.from+channelClipSegments-1))
	}
	lines = append(lines, fmt.Sprintf("schedule: %s", strings.Join(order, " | ")))
	lines = append(lines, fmt.Sprintf("%d segments, %.3f s, %d discontinuities, produced through ChannelPrefetch by %d channel jobs in %.1f s", len(broadcast.entries), clock, len(clips)-1, len(node.enqueued(goshl.StreamChannel)), time.Since(generation).Seconds()))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: broadcast.handler(filepath.Join(playerDir, "player.html"))}
	go server.Serve(listener)
	defer func() {
		server.Close()
		if _, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
			t.Errorf("the playlist server still accepts connections")
		}
	}()
	base := "http://" + listener.Addr().String()
	runFor := time.Duration(clock+8) * time.Second

	broadcast.restart()
	lines = append(lines, "playlist at start:", broadcast.playlist())
	profile := t.TempDir()
	browserCtx, stopBrowser := context.WithTimeout(ctx, runFor)
	browser := exec.CommandContext(browserCtx, chrome, "--headless=new", "--disable-gpu", "--mute-audio", "--no-first-run", "--no-default-browser-check", "--autoplay-policy=no-user-gesture-required", "--user-data-dir="+profile, base+"/player.html")
	browser.Run()
	stopBrowser()
	broadcast.mu.Lock()
	reports := broadcast.reports
	served := len(broadcast.served)
	broadcast.mu.Unlock()
	if len(reports) < 60 {
		t.Fatalf("hls.js sent only %d reports", len(reports))
	}
	final := reports[len(reports)-1]
	firstTime := reports[0]["time"].(float64)
	stuck, previous := 0, -1.0
	for _, report := range reports {
		now := report["time"].(float64)
		if now == previous && report["ended"] != true {
			stuck++
		}
		previous = now
	}
	fatal := 0
	errorsSeen, _ := final["errors"].([]any)
	for _, raw := range errorsSeen {
		if failure, ok := raw.(map[string]any); ok && failure["fatal"] == true {
			fatal++
		}
	}
	lines = append(lines, fmt.Sprintf("hls.js %v in headless Chrome: %d reports, currentTime %.1f -> %.1f of %.1f s, ended %v, %d item changes, %d fragments played, %d segments fetched, errors %d (%d fatal) %v, waiting events %d, samples without progress %d, frames %v, dropped %v, picture %vx%v, resize events %v",
		final["version"], len(reports), firstTime, final["time"], clock, final["ended"], int(final["changes"].(float64)), int(final["fragments"].(float64)), served, len(errorsSeen), fatal, errorsSeen, int(final["waiting"].(float64)), stuck, final["frames"], final["dropped"], final["width"], final["height"], final["resizes"]))
	if fatal != 0 || int(final["changes"].(float64)) < len(clips)-2 || final["time"].(float64) < clock-3 {
		t.Errorf("hls.js did not play through the channel: %+v", final)
	}

	broadcast.restart()
	out, err := exec.Command(filepath.Join(playerDir, "avtest"), base+"/live.m3u8", strconv.Itoa(int(runFor.Seconds()))).Output()
	if err != nil {
		t.Fatalf("AVFoundation run: %v", err)
	}
	var native struct {
		Status             int
		ItemError          string
		Failed             string
		StallNotifications int
		Frames             int
		LargestFrameGap    float64
		LargestFrameGapAt  float64
		GapsOver100        int
		Sizes              map[string]int
		ErrorLog           []string
		Access             map[string]any
		PlayedToEnd        bool
		Samples            []map[string]any
	}
	if err := json.Unmarshal(out, &native); err != nil {
		t.Fatalf("AVFoundation result: %v: %s", err, out)
	}
	broadcast.mu.Lock()
	served = len(broadcast.served)
	broadcast.mu.Unlock()
	waiting, stalled, previous, startTime, endTime := 0, 0, -1.0, 0.0, 0.0
	for position, sample := range native.Samples {
		now, _ := sample["time"].(float64)
		if position == 0 {
			startTime = now
		}
		if sample["waiting"] == true {
			waiting++
		}
		if position > 3 && now == previous && now < clock-1 {
			stalled++
		}
		previous = now
		endTime = now
	}
	lines = append(lines, fmt.Sprintf("AVFoundation (AVPlayer): status %d, currentTime %.1f -> %.1f of %.1f s, played to end %v, %d frames delivered, largest gap between frames %.0f ms at %.1f s, gaps over 100 ms %d, picture sizes %v, %d segments fetched, stall notifications %d, samples waiting %d, samples without progress %d, error log %v, item error %q, access log %v",
		native.Status, startTime, endTime, clock, native.PlayedToEnd, native.Frames, native.LargestFrameGap*1000, native.LargestFrameGapAt, native.GapsOver100, native.Sizes, served, native.StallNotifications, waiting, stalled, native.ErrorLog, native.ItemError+native.Failed, native.Access))
	if native.Status != 1 || native.ItemError != "" || native.Failed != "" || len(native.ErrorLog) != 0 || native.StallNotifications != 0 || endTime < clock-3 {
		t.Errorf("AVFoundation did not play through the channel")
	}
	channelEvidence(t, "players", lines)
}
