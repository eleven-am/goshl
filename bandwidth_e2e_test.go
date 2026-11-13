package goshl

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

type bandwidthPathGen struct{ stubPathGen }

func (bandwidthPathGen) VariantPlaylist(sourceURL string, rendition string, streamType domain.StreamType) string {
	return fmt.Sprintf("/variant/%s/%s", streamType, rendition)
}

type masterVariant struct {
	bandwidth int64
	stream    StreamType
	name      string
}

var (
	bandwidthAttr = regexp.MustCompile(`(?:^|,)BANDWIDTH=(\d+)`)
	extinfLine    = regexp.MustCompile(`^#EXTINF:([0-9.]+)`)
)

func parseMasterVariants(t *testing.T, master string) []masterVariant {
	t.Helper()
	var variants []masterVariant
	lines := strings.Split(master, "\n")
	for index, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") || index+1 >= len(lines) {
			continue
		}
		match := bandwidthAttr.FindStringSubmatch(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		if match == nil {
			t.Fatalf("variant without BANDWIDTH: %s", line)
		}
		value, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			t.Fatalf("bad BANDWIDTH in %s: %v", line, err)
		}
		parts := strings.Split(strings.TrimPrefix(lines[index+1], "/variant/"), "/")
		if len(parts) != 2 {
			t.Fatalf("unexpected variant URI %q", lines[index+1])
		}
		variants = append(variants, masterVariant{bandwidth: value, stream: StreamType(parts[0]), name: parts[1]})
	}
	return variants
}

func segmentDurations(t *testing.T, playlistText string) []float64 {
	t.Helper()
	var durations []float64
	for _, line := range strings.Split(playlistText, "\n") {
		if match := extinfLine.FindStringSubmatch(line); match != nil {
			value, err := strconv.ParseFloat(match[1], 64)
			if err != nil {
				t.Fatalf("bad EXTINF %q: %v", line, err)
			}
			durations = append(durations, value)
		}
	}
	return durations
}

func peakSegmentBitrate(t *testing.T, controller *Controller, source string, stream StreamType, name string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	text, err := controller.VariantPlaylist(ctx, source, stream, name)
	if err != nil {
		t.Fatalf("variant playlist %s: %v", name, err)
	}
	durations := segmentDurations(t, text)
	if len(durations) == 0 {
		t.Fatalf("%s playlist has no segments", name)
	}
	var peak float64
	for index, duration := range durations {
		data, err := controller.Segment(ctx, source, stream, name, index)
		if err != nil {
			t.Fatalf("%s segment %d: %v", name, index, err)
		}
		if rate := float64(len(data)) * 8 / duration; rate > peak {
			peak = rate
		}
	}
	return int64(peak)
}

func TestMasterBandwidthCoversRealPeakSegmentBitrate(t *testing.T) {
	testmedia.Require(t)
	source := testmedia.Spike(t, testmedia.SpikeSpec{Width: 1280, Height: 720, Duration: 36, SpikeStart: 12, SpikeEnd: 24})
	controller := NewController(Options{
		Storage:        newTimelineStorage(),
		Coordinator:    newClusterCoordinator(),
		PathGen:        bandwidthPathGen{},
		TargetDuration: 4,
		SegmentsPerJob: 3,
		SegmentTimeout: 3 * time.Minute,
		JobTimeout:     3 * time.Minute,
	})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("start controller: %v", err)
	}
	defer controller.Stop()

	master, err := controller.MasterPlaylist(context.Background(), source)
	if err != nil {
		t.Fatalf("master playlist: %v", err)
	}
	variants := parseMasterVariants(t, master)

	peaks := make(map[string]int64)
	for _, line := range strings.Split(master, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:TYPE=AUDIO") {
			continue
		}
		_, uri, found := strings.Cut(line, `URI="/variant/`)
		parts := strings.Split(strings.TrimSuffix(uri, `"`), "/")
		if !found || len(parts) != 2 {
			t.Fatalf("unexpected audio media line %q", line)
		}
		variants = append(variants, masterVariant{stream: StreamType(parts[0]), name: parts[1]})
	}
	var maxAudio int64
	var videoCount, audioCount int
	for _, variant := range variants {
		key := string(variant.stream) + "/" + variant.name
		if _, done := peaks[key]; done {
			continue
		}
		peaks[key] = peakSegmentBitrate(t, controller, source, variant.stream, variant.name)
		if variant.stream == StreamAudio && peaks[key] > maxAudio {
			maxAudio = peaks[key]
		}
	}
	meta, err := controller.Metadata(context.Background(), source)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	renditions, _, err := rendition.GenerateVideo(meta, 4)
	if err != nil {
		t.Fatalf("generate video renditions: %v", err)
	}
	methods := make(map[string]domain.PlaybackMethod)
	for _, video := range renditions {
		methods[video.Name] = video.Method
	}
	var direct, transcoded bool
	for _, variant := range variants {
		if variant.stream != StreamVideo {
			continue
		}
		if methods[variant.name] == domain.Transcode {
			transcoded = true
		} else {
			direct = true
		}
	}
	for _, variant := range variants {
		key := string(variant.stream) + "/" + variant.name
		want := peaks[key]
		if variant.stream != StreamVideo {
			audioCount++
			continue
		}
		videoCount++
		want += maxAudio
		if variant.bandwidth < want {
			t.Errorf("%s advertises BANDWIDTH=%d, real peak segment bitrate is %d (%.2fx)", key, variant.bandwidth, want, float64(want)/float64(variant.bandwidth))
		}
		t.Logf("%s BANDWIDTH=%d peak=%d", key, variant.bandwidth, want)
	}
	if !direct || !transcoded {
		t.Fatalf("master must offer a direct-stream and a transcoded video variant (direct %v, transcoded %v):\n%s", direct, transcoded, master)
	}
	if videoCount < 2 || audioCount == 0 {
		t.Fatalf("master must offer at least two video variants and an audio variant, got %d and %d:\n%s", videoCount, audioCount, master)
	}
}
