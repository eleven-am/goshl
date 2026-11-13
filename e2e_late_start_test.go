package goshl

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

const lateVideoStart = 0.0834

func lateStartFixture(t *testing.T) timelineSource {
	t.Helper()
	path := testmedia.H264(t, testmedia.Spec{
		Container:      "mp4",
		Width:          480,
		Height:         400,
		FPS:            "24000/1001",
		BFrames:        3,
		Pyramid:        true,
		VideoDelay:     lateVideoStart,
		KeyframeFrames: []int{0, 25, 50, 101, 125, 149, 200, 251, 299, 350, 401, 449, 500, 551},
		Duration:       24,
		Audio:          []testmedia.AudioSpec{{Codec: "aac"}},
	})
	source := timelineSource{path: path, start: testmedia.FormatStart(t, path), video: testmedia.Packets(t, path, "v:0")}
	for _, packet := range source.video {
		if packet.Key {
			source.keyframes = append(source.keyframes, packet.PTS-source.start)
		}
	}
	sort.Float64s(source.keyframes)
	return source
}

func TestControllerLateVideoStartDirectStreamSharesOneTimeline(t *testing.T) {
	testmedia.Require(t)
	source := lateStartFixture(t)
	if first := source.firstVideoPTS(); math.Abs(first-lateVideoStart) > 0.002 {
		t.Fatalf("fixture video starts at %.4f, want %.4f", first, lateVideoStart)
	}
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

	meta, err := controller.Metadata(context.Background(), source.path)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if !meta.Video.StartsWithKeyframe {
		t.Fatalf("late-start source must report StartsWithKeyframe, start %.4f keyframes %v", meta.Video.Start, meta.Keyframes)
	}
	source.segments = playlist.CalculateSegments(source.keyframes, meta.Duration, 2)
	if len(source.segments) < 8 {
		t.Fatalf("fixture must plan at least 8 segments, planned %d", len(source.segments))
	}
	videos, err := rendition.GenerateVideo(meta, 2)
	if err != nil {
		t.Fatalf("generate video renditions: %v", err)
	}
	if videos[0].Method != domain.DirectStream {
		t.Fatalf("source rung method = %s, want direct_stream", videos[0].Method)
	}

	video := fetchTimelineRendition(t, controller, source, StreamVideo, videos[0].Name, "v:0")
	assertVideoTimeline(t, videos[0].Name+" (copy)", source, video, false, 2*timelineTick, 2*timelineTick)

	var audio []timelineSegment
	var audioName string
	for _, track := range rendition.GenerateAudioTracks(meta.Audios) {
		if strings.HasSuffix(track.Name, "aac_stereo") {
			audioName = track.Name
			audio = fetchTimelineRendition(t, controller, source, StreamAudio, track.Name, "a:0")
			assertAudioTimeline(t, track.Name, source, track.StreamIndex, audio, 1024.0/48000)
			break
		}
	}
	if len(audio) == 0 {
		t.Fatalf("no aac_stereo rendition")
	}

	offset := domain.OutputTimelineOffset.Seconds()
	if got := video[0].pts[0] - offset; math.Abs(got-source.firstVideoPTS()) > 2*timelineTick {
		t.Errorf("job 0 video starts %.4f s into the timeline, want %.4f", got, source.firstVideoPTS())
	}
	for index := range video {
		want := source.expectedStart(index) + offset
		if diff := video[index].pts[0] - want; math.Abs(diff) > 2*timelineTick {
			t.Errorf("segment %d (job %d) carries offset %+.4f, want %+.4f", index, index/2, video[index].pts[0]-source.expectedStart(index), offset)
		}
	}
	packets := testmedia.Packets(t, source.path, strconv.Itoa(1))
	gap := video[0].pts[0] - audio[0].pts[0]
	wantGap := source.firstVideoPTS() - (packets[0].PTS - source.start)
	if math.Abs(gap-wantGap) > 1024.0/48000+2*timelineTick {
		t.Errorf("%s: video leads audio by %.4f at the start, the source has %.4f", audioName, gap, wantGap)
	}
}
