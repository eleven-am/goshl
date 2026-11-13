package goshl

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func TestControllerServesAudioPastTheSourceEnd(t *testing.T) {
	testmedia.Require(t)
	keyframes := make([]int, 0, 10)
	for frame := 0; frame < 240; frame += 24 {
		keyframes = append(keyframes, frame)
	}
	path := testmedia.H264(t, testmedia.Spec{
		Container:      "mp4",
		FPS:            "24",
		KeyframeFrames: keyframes,
		Duration:       10,
		Audio:          []testmedia.AudioSpec{{Codec: "aac", Duration: 7.5}, {Codec: "ac3", Duration: 7.5}},
	})
	observer := &timelineObserver{}
	controller := NewController(Options{
		Storage:        newTimelineStorage(),
		Coordinator:    newClusterCoordinator(),
		PathGen:        stubPathGen{},
		TargetDuration: 1,
		SegmentsPerJob: 2,
		SegmentTimeout: 2 * time.Minute,
		JobTimeout:     2 * time.Minute,
		Observer:       observer,
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
	source := timelineSource{path: path, segments: playlist.CalculateSegments(meta.Keyframes, meta.Duration, 1)}
	if len(source.segments) != 10 {
		t.Fatalf("fixture must plan 10 segments, planned %d", len(source.segments))
	}
	frames := map[string]float64{"aac_stereo": 1024.0 / 48000, "ac3_passthrough": 1536.0 / 48000}
	var served int
	for _, audio := range rendition.GenerateAudioTracks(meta.Audios) {
		if audio.SourceEnd < 7.3 || audio.SourceEnd > 7.6 {
			t.Fatalf("rendition %s must know its source ends near 7.5 s, got %.3f", audio.Name, audio.SourceEnd)
		}
		for suffix, frame := range frames {
			if !strings.HasSuffix(audio.Name, suffix) {
				continue
			}
			served++
			segments := fetchTimelineRendition(t, controller, source, StreamAudio, audio.Name, "a:0")
			assertContinuousJoins(t, audio.Name, segments, frame, 2*timelineTick)
			last := segments[len(segments)-1]
			if len(last.pts) == 0 {
				continue
			}
			if end := last.pts[len(last.pts)-1] + frame; end < 10+domain.OutputTimelineOffset.Seconds()-2*frame {
				t.Errorf("%s ends at %.6f, want audio up to the plan end", audio.Name, end)
			}
		}
	}
	if served != 3 {
		t.Fatalf("fixture must serve two AAC renditions and one AC3 rendition, served %d", served)
	}
	if failed := observer.failed(); len(failed) != 0 {
		t.Errorf("jobs failed: %v", failed)
	}
}
