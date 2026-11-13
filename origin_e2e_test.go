package goshl

import (
	"context"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

const startOffset = 2.0

func TestControllerSubtitleCuesFollowVideoOnOffsetSource(t *testing.T) {
	testmedia.Require(t)
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			path := testmedia.H264(t, testmedia.Spec{
				Container:      container,
				Width:          320,
				Height:         240,
				FPS:            "24000/1001",
				BFrames:        2,
				KeyframeFrames: []int{0, 48, 96, 144, 192, 240},
				Duration:       10,
				Audio:          []testmedia.AudioSpec{{Codec: "aac"}},
				Subtitles:      true,
				StartOffset:    startOffset,
			})
			start := testmedia.FormatStart(t, path)
			if start < 1.5 {
				t.Fatalf("fixture start_time is %.3f, want a nonzero offset", start)
			}
			var keyframes []float64
			for _, packet := range testmedia.Packets(t, path, "v:0") {
				if packet.Key {
					keyframes = append(keyframes, packet.PTS-start)
				}
			}
			sort.Float64s(keyframes)

			controller := NewController(Options{
				Storage:        newTimelineStorage(),
				Coordinator:    newClusterCoordinator(),
				PathGen:        stubPathGen{},
				TargetDuration: 2,
				SegmentsPerJob: 2,
				SegmentTimeout: 2 * time.Minute,
				JobTimeout:     2 * time.Minute,
			})
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if err := controller.Start(ctx); err != nil {
				t.Fatalf("start controller: %v", err)
			}
			defer controller.Stop()

			meta, err := controller.Metadata(context.Background(), path)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			if math.Abs(meta.StartTime-start) > 0.001 {
				t.Fatalf("metadata start time %.3f, want %.3f", meta.StartTime, start)
			}
			if len(meta.Subtitles) == 0 {
				t.Fatalf("metadata lists no subtitle track")
			}
			vtt, err := controller.SubtitleVTT(context.Background(), path, meta.Subtitles[0].ID)
			if err != nil {
				t.Fatalf("subtitle vtt: %v", err)
			}

			offset := domain.OutputTimelineOffset.Seconds()
			wantMap := "X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000"
			if !strings.Contains(string(vtt), wantMap) {
				t.Fatalf("subtitle VTT lacks %q:\n%s", wantMap, vtt)
			}
			local, ok := firstCueStart(string(vtt))
			if !ok {
				t.Fatalf("subtitle VTT has no cue:\n%s", vtt)
			}
			cueOutput := local + offset
			cueSource := 1.0 + startOffset - start
			if math.Abs(cueOutput-(cueSource+offset)) > 0.0015 {
				t.Errorf("cue lands at %.3f on the output timeline, the source cue at %.3f belongs at %.3f:\n%s", cueOutput, cueSource, cueSource+offset, vtt)
			}

			segments := playlist.CalculateSegments(keyframes, meta.Duration, 2)
			containing := -1
			for index := range segments {
				if segments[index].Start <= cueSource+0.0015 {
					containing = index
				}
			}
			if containing < 0 {
				t.Fatalf("no planned segment contains source cue at %.3f", cueSource)
			}
			videos, err := rendition.GenerateVideo(meta, 2)
			if err != nil || len(videos) == 0 {
				t.Fatalf("generate video renditions: %v (%d renditions)", err, len(videos))
			}
			data, err := controller.Segment(context.Background(), path, StreamVideo, videos[0].Name, containing)
			if err != nil {
				t.Fatalf("video segment %d: %v", containing, err)
			}
			segment := readTimelineSegment(t, t.TempDir(), "video", containing, "v:0", data)
			if len(segment.pts) == 0 {
				t.Fatalf("video segment %d has no packets", containing)
			}
			first, last := segment.pts[0], segment.pts[len(segment.pts)-1]+timelineVideoFrame
			if cueOutput < first-0.0015 || cueOutput > last+0.0015 {
				t.Errorf("cue at %.3f on the output timeline is outside video segment %d spanning %.3f-%.3f", cueOutput, containing, first, last)
			}
		})
	}
}
