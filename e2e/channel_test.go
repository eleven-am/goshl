package e2e

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

type channelItemFacts struct {
	plan     goshl.ChannelPlan
	segments [][]byte
	facts    []channelSegmentFacts
}

func produceChannelItem(t *testing.T, node *channelNode, path string, order []int) channelItemFacts {
	t.Helper()
	return produceChannelTrack(t, node, path, 0, order)
}

func produceChannelTrack(t *testing.T, node *channelNode, path string, track int, order []int) channelItemFacts {
	t.Helper()
	ctx := context.Background()
	plan, err := node.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{}, track)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	item := channelItemFacts{plan: plan, segments: make([][]byte, len(plan.Segments)), facts: make([]channelSegmentFacts, len(plan.Segments))}
	for _, index := range order {
		if _, err := node.controller.ChannelSegment(ctx, path, goshl.ChannelProfile{}, track, index); err != nil {
			t.Fatalf("segment %d: %v", index, err)
		}
	}
	for index := range plan.Segments {
		data, err := node.controller.ChannelSegment(ctx, path, goshl.ChannelProfile{}, track, index)
		if err != nil {
			t.Fatalf("segment %d: %v", index, err)
		}
		item.segments[index] = data
		item.facts[index] = probeChannelSegment(t, data)
	}
	if failures := node.recorder.of(goshl.EventJobFailed, ""); len(failures) != 0 {
		t.Fatalf("jobs failed: %+v", failures)
	}
	return item
}

func checkChannelTimeline(t *testing.T, item channelItemFacts) {
	t.Helper()
	plan := item.plan
	var previousVideo, previousAudio int64
	frames := 0
	for index, facts := range item.facts {
		wantFrames := 48
		if index == len(item.facts)-1 {
			wantFrames = plan.Frames - index*48
		}
		if len(facts.video) != wantFrames {
			t.Errorf("segment %d holds %d frames, want %d", index, len(facts.video), wantFrames)
			continue
		}
		if len(facts.audio) == 0 {
			t.Errorf("segment %d has no audio", index)
			continue
		}
		if !facts.video[0].key {
			t.Errorf("segment %d does not start with a keyframe", index)
		}
		if want := 10*channelTicks + channelFrameTicks(int64(index)*48, plan); facts.video[0].pts != want {
			t.Errorf("segment %d starts at %d ticks, planned %d", index, facts.video[0].pts, want)
		}
		for _, packet := range facts.video {
			if step := packet.pts - previousVideo; previousVideo != 0 && step != 3753 && step != 3754 {
				t.Errorf("segment %d: video step of %d ticks", index, step)
			}
			previousVideo = packet.pts
			frames++
		}
		for _, packet := range facts.audio {
			if step := packet.pts - previousAudio; previousAudio != 0 && step != 1920 {
				t.Errorf("segment %d: audio step of %d ticks", index, step)
			}
			previousAudio = packet.pts
		}
		if lead := facts.audio[0].pts - facts.video[0].pts; lead < -1920 || lead > 1920 {
			t.Errorf("segment %d: audio starts %d ticks from the video", index, lead)
		}
	}
	if frames != plan.Frames {
		t.Errorf("item holds %d frames, plan says %d", frames, plan.Frames)
	}
	last := item.facts[len(item.facts)-1]
	audioEnd := last.audio[len(last.audio)-1].pts + 1920
	videoEnd := 10*channelTicks + channelFrameTicks(int64(plan.Frames), plan)
	if gap := audioEnd - videoEnd; gap < -1920 || gap > 1920 {
		t.Errorf("audio ends %d ticks from the planned end", gap)
	}
}

func channelStreamShape(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shape.ts")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,profile,level,width,height,pix_fmt,sample_aspect_ratio,r_frame_rate,color_range,color_space,color_transfer,color_primaries,sample_rate,channels,id,extradata_hash", "-show_data_hash", "md5", "-of", "compact", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestChannelItemFollowsItsPlanAcrossBatches(t *testing.T) {
	testmedia.Require(t)
	path := testmedia.H264(t, testmedia.Spec{Width: 640, Height: 360, Duration: 21.3, KeyframeFrames: []int{0, 100, 250, 400}, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	scattered := produceChannelItem(t, startChannelNode(t, newTimelineStorage(), 3, nil), path, []int{7, 2, 10, 4})
	ordered := produceChannelItem(t, startChannelNode(t, newTimelineStorage(), 3, nil), path, nil)
	if len(scattered.plan.Segments) != 11 || scattered.plan.Frames != 511 {
		t.Fatalf("plan has %d segments and %d frames", len(scattered.plan.Segments), scattered.plan.Frames)
	}
	checkChannelTimeline(t, scattered)
	checkChannelTimeline(t, ordered)
	for index := range scattered.facts {
		if len(scattered.facts[index].video) != len(ordered.facts[index].video) {
			t.Errorf("segment %d holds another frame count between two request orders", index)
		}
	}
	shape := channelStreamShape(t, scattered.segments[5])
	for _, part := range []string{"codec_name=h264", "profile=High", "level=31", "width=1280", "height=720", "pix_fmt=yuv420p", "r_frame_rate=24000/1001", "color_space=bt709", "color_transfer=bt709", "color_primaries=bt709", "color_range=tv", "codec_name=aac", "sample_rate=48000", "channels=2"} {
		if !strings.Contains(shape, part) {
			t.Errorf("segment lacks %s: %s", part, shape)
		}
	}
}

func TestChannelFormatIsIdenticalAcrossSources(t *testing.T) {
	testmedia.Require(t)
	sources := map[string]testmedia.Spec{
		"standard definition at 25":   {Width: 640, Height: 480, FPS: "25", Duration: 9, Audio: []testmedia.AudioSpec{{Codec: "aac", SampleRate: 44100}}},
		"scope at 30000/1001":         {Width: 960, Height: 400, FPS: "30000/1001", Duration: 9, Audio: []testmedia.AudioSpec{{Codec: "ac3"}}},
		"matroska half a frame late":  {Container: "mkv", Width: 640, Height: 360, Duration: 9, VideoDelay: 0.021, Audio: []testmedia.AudioSpec{{Codec: "aac"}}},
		"no audio":                    {Width: 640, Height: 360, Duration: 9},
		"audio ends early":            {Width: 640, Height: 360, Duration: 9, Audio: []testmedia.AudioSpec{{Codec: "aac", Duration: 2.5}}},
		"video ends early":            {Width: 640, Height: 360, Duration: 3, Audio: []testmedia.AudioSpec{{Codec: "aac", Duration: 9}}},
		"second audio track selected": {Width: 640, Height: 360, Duration: 9, Audio: []testmedia.AudioSpec{{Codec: "aac"}, {Codec: "aac", SampleRate: 44100}}},
	}
	shapes := map[string]string{}
	for name, spec := range sources {
		t.Run(name, func(t *testing.T) {
			path := testmedia.H264(t, spec)
			node := startChannelNode(t, newTimelineStorage(), 2, nil)
			track := len(spec.Audio) - 1
			if track < 0 {
				track = 0
			}
			item := produceChannelTrack(t, node, path, track, []int{3})
			checkChannelTimeline(t, item)
			shapes[name] = channelStreamShape(t, item.segments[1])
			if track > 0 {
				first := produceChannelTrack(t, node, path, 0, nil)
				if first.plan.Rendition == item.plan.Rendition || channelDigest(first.segments[1]) == channelDigest(item.segments[1]) {
					t.Error("two audio tracks must not share a rendition or its bytes")
				}
			}
		})
	}
	reference := ""
	for name, shape := range shapes {
		if reference == "" {
			reference = shape
		}
		if shape != reference {
			t.Errorf("%s has another stream shape:\n%s\n%s", name, shape, reference)
		}
	}
}

func TestChannelRejectsSourcesWithoutVideo(t *testing.T) {
	testmedia.Require(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "audio.m4a")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-t", "5", "-i", "sine=frequency=440", "-c:a", "aac", path).CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, out)
	}
	node := startChannelNode(t, newTimelineStorage(), 2, nil)
	if _, err := node.controller.ChannelPlan(context.Background(), path, goshl.ChannelProfile{}, 0); !errors.Is(err, goshl.ErrRenditionNotFound) {
		t.Fatalf("audio-only source: %v", err)
	}
}

func TestChannelPlanIsDeterministicAndPureOfDuration(t *testing.T) {
	testmedia.Require(t)
	path := testmedia.H264(t, testmedia.Spec{Width: 640, Height: 360, Duration: 21.3, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	shared := newTimelineStorage()
	first := startChannelNode(t, shared, 10, nil)
	replica := startChannelNode(t, shared, 10, nil)
	isolated := startChannelNode(t, newTimelineStorage(), 10, nil)
	ctx := context.Background()
	plan, err := first.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{}, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	explicit, err := first.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{Height: 720, FrameRateNum: 24000, FrameRateDen: 1001, SegmentFrames: 48, VideoBitrate: 2_500_000, AudioBitrate: 160_000}, 0)
	if err != nil {
		t.Fatalf("explicit plan: %v", err)
	}
	again, err := first.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{}, 0)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	fromReplica, err := replica.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{}, 0)
	if err != nil {
		t.Fatalf("replica plan: %v", err)
	}
	fromIsolated, err := isolated.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{}, 0)
	if err != nil {
		t.Fatalf("isolated plan: %v", err)
	}
	for name, other := range map[string]goshl.ChannelPlan{"explicit defaults": explicit, "second call": again, "replica on shared storage": fromReplica, "replica that probed by itself": fromIsolated} {
		if !reflect.DeepEqual(plan, other) {
			t.Errorf("plan from %s differs", name)
		}
	}
	meta, err := first.controller.Metadata(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	normalised, err := goshl.ChannelProfile{}.Normalised()
	if err != nil {
		t.Fatal(err)
	}
	if pure := playlist.CalculateChannelSegments(meta.Duration, normalised); !reflect.DeepEqual(pure, plan.Segments) {
		t.Error("plan is not the pure function of the probed duration")
	}
	if got := len(first.enqueued(goshl.StreamChannel)) + len(replica.enqueued(goshl.StreamChannel)); got != 0 {
		t.Errorf("planning enqueued %d jobs", got)
	}
	sum := 0.0
	for index, segment := range plan.Segments {
		sum += segment.Duration
		if index < len(plan.Segments)-1 && math.Abs(segment.Start-float64(index)*2.002) > 1e-9 {
			t.Errorf("segment %d starts at %.6f", index, segment.Start)
		}
	}
	if math.Abs(sum-plan.Duration) > 1e-6 {
		t.Errorf("segment durations sum to %.6f, plan duration %.6f", sum, plan.Duration)
	}
}

func TestChannelPrefetchEnqueuesEachBatchOnce(t *testing.T) {
	testmedia.Require(t)
	path := testmedia.H264(t, testmedia.Spec{Width: 640, Height: 360, Duration: 90, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	node := startChannelNode(t, newTimelineStorage(), 10, nil)
	ctx := context.Background()
	plan, err := node.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	const first, lead, ticks = 10, 15, 6
	for tick := 0; tick < ticks; tick++ {
		if err := node.controller.ChannelPrefetch(ctx, path, goshl.ChannelProfile{}, 0, first+2*tick, lead); err != nil {
			t.Fatalf("prefetch tick %d: %v", tick, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitChannelStored(t, node, plan, first, first+2*(ticks-1)+lead-1)
	var ranges []string
	seen := map[string]int{}
	for _, job := range node.enqueued(goshl.StreamChannel) {
		ranges = append(ranges, fmt.Sprintf("%d-%d", job.StartIndex, job.EndIndex))
		seen[job.ID]++
		if job.Priority != goshl.PriorityChannel {
			t.Errorf("channel job has priority %d", job.Priority)
		}
	}
	sort.Strings(ranges)
	if !reflect.DeepEqual(ranges, []string{"10-19", "20-29", "30-39"}) {
		t.Errorf("overlapping prefetch calls enqueued %v", ranges)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("job %s was enqueued %d times", id, times)
		}
	}
	if failures := node.recorder.of(goshl.EventJobFailed, ""); len(failures) != 0 {
		t.Errorf("jobs failed: %+v", failures)
	}
}

type gatedStorage struct {
	goshl.Storage
	rendition string
	release   chan struct{}
}

func (g *gatedStorage) WriteSegment(ctx context.Context, info goshl.SegmentData, data []byte) error {
	if info.Rendition == g.rendition {
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.Storage.WriteSegment(ctx, info, data)
}

func TestChannelSegmentsAreProducedWhileVODPoolsAreSaturated(t *testing.T) {
	testmedia.Require(t)
	path := testmedia.H264(t, testmedia.Spec{Width: 1280, Height: 720, Duration: 120, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	gate := &gatedStorage{release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	node := startChannelNode(t, newTimelineStorage(), 10, func(options *goshl.Options) {
		gate.Storage = options.Storage
		options.Storage = gate
		options.VideoPoolSize = 1
		options.AudioPoolSize = 1
		options.SegmentsPerJob = 4
		options.TargetDuration = 6
	})
	ctx := context.Background()
	meta, err := node.controller.Metadata(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	videos, _, err := rendition.GenerateVideo(meta, 6)
	if err != nil {
		t.Fatal(err)
	}
	vod := ""
	for _, video := range videos {
		if video.Method == goshl.Transcode {
			vod = video.Name
			break
		}
	}
	if vod == "" {
		t.Fatal("source has no transcoded VOD rendition")
	}
	gate.rendition = vod
	plan, err := node.controller.ChannelPlan(ctx, path, goshl.ChannelProfile{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	const vodJobs = 5
	var vodWait sync.WaitGroup
	for job := 0; job < vodJobs; job++ {
		vodWait.Add(1)
		go func() {
			defer vodWait.Done()
			if _, err := node.controller.Segment(ctx, path, goshl.StreamVideo, vod, 4*job); err != nil {
				t.Errorf("VOD segment: %v", err)
			}
		}()
	}
	deadline := time.Now().Add(time.Minute)
	for len(node.enqueued(goshl.StreamVideo)) < vodJobs || len(node.recorder.of(goshl.EventJobStarted, goshl.StreamVideo)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the VOD lane did not fill")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := node.controller.ChannelPrefetch(ctx, path, goshl.ChannelProfile{}, 0, 0, 10); err != nil {
		t.Fatal(err)
	}
	waitChannelStored(t, node, plan, 0, 9)
	if completed := len(node.recorder.of(goshl.EventJobCompleted, goshl.StreamVideo)); completed != 0 {
		t.Errorf("%d VOD jobs completed while their storage writes were blocked", completed)
	}
	release()
	vodWait.Wait()
	if failures := node.recorder.of(goshl.EventJobFailed, ""); len(failures) != 0 {
		t.Errorf("jobs failed: %+v", failures)
	}
}

func TestChannelDisabledWithoutPool(t *testing.T) {
	node := startChannelNode(t, newTimelineStorage(), 10, func(options *goshl.Options) {
		options.ChannelPoolSize = 0
	})
	ctx := context.Background()
	if _, err := node.controller.ChannelPlan(ctx, "missing.mkv", goshl.ChannelProfile{}, 0); !errors.Is(err, goshl.ErrChannelDisabled) {
		t.Errorf("plan: %v", err)
	}
	if _, err := node.controller.ChannelSegment(ctx, "missing.mkv", goshl.ChannelProfile{}, 0, 0); !errors.Is(err, goshl.ErrChannelDisabled) {
		t.Errorf("segment: %v", err)
	}
	if err := node.controller.ChannelPrefetch(ctx, "missing.mkv", goshl.ChannelProfile{}, 0, 0, 5); !errors.Is(err, goshl.ErrChannelDisabled) {
		t.Errorf("prefetch: %v", err)
	}
}

func TestChannelRejectsInvalidProfiles(t *testing.T) {
	node := startChannelNode(t, newTimelineStorage(), 10, nil)
	ctx := context.Background()
	profiles := map[string]goshl.ChannelProfile{
		"odd height":      {Height: 721},
		"tiny height":     {Height: 2},
		"negative rate":   {FrameRateNum: -24, FrameRateDen: 1},
		"huge bitrate":    {VideoBitrate: 1 << 40},
		"negative frames": {SegmentFrames: -1},
	}
	for name, profile := range profiles {
		if _, err := node.controller.ChannelPlan(ctx, "missing.mkv", profile, 0); !errors.Is(err, goshl.ErrInvalidChannelProfile) {
			t.Errorf("%s: plan: %v", name, err)
		}
		if _, err := node.controller.ChannelSegment(ctx, "missing.mkv", profile, 0, 0); !errors.Is(err, goshl.ErrInvalidChannelProfile) {
			t.Errorf("%s: segment: %v", name, err)
		}
	}
}
