package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

const (
	channelSourcesEnv  = "GOSHL_CHANNEL_SOURCES"
	channelEvidenceEnv = "GOSHL_CHANNEL_EVIDENCE"
	channelTicks       = 90000
)

type channelSource struct {
	key  string
	path string
}

func channelSources(t *testing.T) []channelSource {
	t.Helper()
	raw := os.Getenv(channelSourcesEnv)
	if raw == "" {
		t.Skipf("%s is not set", channelSourcesEnv)
	}
	var sources []channelSource
	for _, entry := range strings.Split(raw, ";") {
		key, path, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("malformed %s entry %q", channelSourcesEnv, entry)
		}
		sources = append(sources, channelSource{key: key, path: path})
	}
	return sources
}

type channelRecorder struct {
	mu     sync.Mutex
	events []goshl.Event
}

func (r *channelRecorder) Observe(ctx context.Context, event goshl.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *channelRecorder) of(kind goshl.EventType, stream goshl.StreamType) []goshl.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matched []goshl.Event
	for _, event := range r.events {
		if event.Type == kind && (stream == "" || event.StreamType == stream) {
			matched = append(matched, event)
		}
	}
	return matched
}

type channelNode struct {
	controller  *goshl.Controller
	storage     *timelineStorage
	coordinator *clusterCoordinator
	recorder    *channelRecorder
	jobsMu      sync.Mutex
	jobs        []goshl.Job
}

func (n *channelNode) enqueued(stream goshl.StreamType) []goshl.Job {
	n.jobsMu.Lock()
	defer n.jobsMu.Unlock()
	var matched []goshl.Job
	for _, job := range n.jobs {
		if job.StreamType == stream {
			matched = append(matched, job)
		}
	}
	return matched
}

func startChannelNode(t *testing.T, storage *timelineStorage, segmentsPerJob int, mutate func(*goshl.Options)) *channelNode {
	t.Helper()
	node := &channelNode{storage: storage, coordinator: newClusterCoordinator(), recorder: &channelRecorder{}}
	node.coordinator.onEnqueue = func(job goshl.Job) {
		node.jobsMu.Lock()
		node.jobs = append(node.jobs, job)
		node.jobsMu.Unlock()
	}
	options := goshl.Options{
		Storage:               storage,
		Coordinator:           node.coordinator,
		PathGen:               stubPathGen{},
		HWAccelMode:           goshl.AccelNone,
		SegmentTimeout:        5 * time.Minute,
		JobTimeout:            5 * time.Minute,
		ProbeTimeout:          5 * time.Minute,
		ChannelPoolSize:       2,
		ChannelSegmentsPerJob: segmentsPerJob,
		Observer:              node.recorder,
	}
	if mutate != nil {
		mutate(&options)
	}
	node.controller = goshl.NewController(options)
	ctx, stop := context.WithCancel(context.Background())
	if err := node.controller.Start(ctx); err != nil {
		stop()
		t.Fatalf("start controller: %v", err)
	}
	t.Cleanup(func() {
		node.controller.Stop()
		stop()
	})
	return node
}

type channelPacket struct {
	pts int64
	key bool
}

type channelSegmentFacts struct {
	video []channelPacket
	audio []channelPacket
}

func probeChannelSegment(t *testing.T, data []byte) channelSegmentFacts {
	t.Helper()
	path := filepath.Join(t.TempDir(), "segment.ts")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "packet=codec_type,pts,flags", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var facts channelSegmentFacts
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		pts, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("packet line %q: %v", line, err)
		}
		packet := channelPacket{pts: pts, key: strings.HasPrefix(fields[2], "K")}
		if fields[0] == "video" {
			facts.video = append(facts.video, packet)
		} else {
			facts.audio = append(facts.audio, packet)
		}
	}
	sort.Slice(facts.video, func(i, j int) bool { return facts.video[i].pts < facts.video[j].pts })
	return facts
}

func channelDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

func channelFrameTicks(frame int64, plan goshl.ChannelPlan) int64 {
	numerator := frame * int64(plan.FrameRateDen) * channelTicks
	denominator := int64(plan.FrameRateNum)
	return (numerator + denominator/2) / denominator
}

func channelSegmentProblem(plan goshl.ChannelPlan, index int, facts channelSegmentFacts) string {
	wantFrames := 48
	if index == len(plan.Segments)-1 {
		wantFrames = plan.Frames - index*48
	}
	switch {
	case len(facts.video) != wantFrames:
		return fmt.Sprintf("%d video frames, want %d", len(facts.video), wantFrames)
	case len(facts.audio) == 0:
		return "no audio"
	case !facts.video[0].key:
		return "does not start with a keyframe"
	}
	if want := 10*channelTicks + channelFrameTicks(int64(index)*48, plan); facts.video[0].pts != want {
		return fmt.Sprintf("starts at %d ticks, planned %d", facts.video[0].pts, want)
	}
	for position := 1; position < len(facts.video); position++ {
		if step := facts.video[position].pts - facts.video[position-1].pts; step != 3753 && step != 3754 {
			return fmt.Sprintf("video step of %d ticks", step)
		}
	}
	return ""
}

func channelEvidence(t *testing.T, name string, lines []string) {
	t.Helper()
	for _, line := range lines {
		t.Log(line)
	}
	dir := os.Getenv(channelEvidenceEnv)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitChannelStored(t *testing.T, node *channelNode, plan goshl.ChannelPlan, first, last int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for index := first; index <= last; {
		stored, err := node.storage.SegmentExists(context.Background(), plan.SegmentData(index))
		if err != nil {
			t.Fatal(err)
		}
		if stored {
			index++
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("segment %d was not stored in time", index)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readChannelStored(t *testing.T, node *channelNode, plan goshl.ChannelPlan, index int) []byte {
	t.Helper()
	data, err := node.storage.ReadSegment(context.Background(), plan.SegmentData(index))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestChannelLibraryPlanIsDeterministic(t *testing.T) {
	var lines []string
	for _, source := range channelSources(t) {
		shared := newTimelineStorage()
		first := startChannelNode(t, shared, 10, nil)
		replica := startChannelNode(t, shared, 10, nil)
		isolated := startChannelNode(t, newTimelineStorage(), 10, nil)
		ctx := context.Background()
		plan, err := first.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
		if err != nil {
			t.Fatalf("%s: plan: %v", source.key, err)
		}
		explicit, err := first.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{Height: 720, FrameRateNum: 24000, FrameRateDen: 1001, SegmentFrames: 48, VideoBitrate: 2_500_000, AudioBitrate: 160_000}, 0)
		if err != nil {
			t.Fatalf("%s: explicit plan: %v", source.key, err)
		}
		again, err := first.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
		if err != nil {
			t.Fatalf("%s: second plan: %v", source.key, err)
		}
		fromReplica, err := replica.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
		if err != nil {
			t.Fatalf("%s: replica plan: %v", source.key, err)
		}
		fromIsolated, err := isolated.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
		if err != nil {
			t.Fatalf("%s: isolated plan: %v", source.key, err)
		}
		for name, other := range map[string]goshl.ChannelPlan{"explicit defaults": explicit, "second call": again, "replica on shared storage": fromReplica, "replica that probed by itself": fromIsolated} {
			if !reflect.DeepEqual(plan, other) {
				t.Errorf("%s: plan from %s differs", source.key, name)
			}
		}
		meta, err := first.controller.Metadata(ctx, source.path)
		if err != nil {
			t.Fatal(err)
		}
		normalised, err := goshl.ChannelProfile{}.Normalised()
		if err != nil {
			t.Fatal(err)
		}
		if pure := playlist.CalculateChannelSegments(meta.Duration, normalised); !reflect.DeepEqual(pure, plan.Segments) {
			t.Errorf("%s: plan is not the pure function of the probed duration", source.key)
		}
		if got := len(first.enqueued(goshl.StreamChannel)) + len(replica.enqueued(goshl.StreamChannel)); got != 0 {
			t.Errorf("%s: planning enqueued %d jobs", source.key, got)
		}
		sum := 0.0
		for index, segment := range plan.Segments {
			sum += segment.Duration
			if index < len(plan.Segments)-1 && math.Abs(segment.Start-float64(index)*2.002) > 1e-9 {
				t.Errorf("%s: segment %d starts at %.6f", source.key, index, segment.Start)
			}
		}
		if math.Abs(sum-plan.Duration) > 1e-6 {
			t.Errorf("%s: segment durations sum to %.6f, plan duration %.6f", source.key, sum, plan.Duration)
		}
		last := plan.Segments[len(plan.Segments)-1]
		lines = append(lines, fmt.Sprintf("%s: probed %.6f s -> %d frames, %d segments, plan %.6f s, last segment %.6f s, rendition %s, key %s, 5 plans identical", source.key, meta.Duration, plan.Frames, len(plan.Segments), plan.Duration, last.Duration, plan.Rendition, plan.MediaKey[:18]))
	}
	channelEvidence(t, "plan", lines)
}

func fetchChannelBatch(t *testing.T, node *channelNode, source channelSource, plan goshl.ChannelPlan, first, last int) [][]byte {
	t.Helper()
	batch := make([][]byte, 0, last-first+1)
	for index := first; index <= last; index++ {
		data, err := node.controller.ChannelSegment(context.Background(), source.path, goshl.ChannelProfile{}, 0, index)
		if err != nil {
			t.Fatalf("%s: segment %d: %v", source.key, index, err)
		}
		batch = append(batch, data)
	}
	return batch
}

func TestChannelLibrarySegmentsJoinFrameExact(t *testing.T) {
	const perJob = 10
	for _, source := range channelSources(t) {
		t.Run(source.key, func(t *testing.T) {
			var lines []string
			ctx := context.Background()
			direct := startChannelNode(t, newTimelineStorage(), perJob, nil)
			plan, err := direct.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			count := len(plan.Segments)
			if count < 4*perJob {
				t.Fatalf("source plans only %d segments", count)
			}
			start := count / 2 / perJob * perJob
			end := start + perJob - 1

			started := time.Now()
			if _, err := direct.controller.ChannelSegment(ctx, source.path, goshl.ChannelProfile{}, 0, start+5); err != nil {
				t.Fatalf("mid-batch request: %v", err)
			}
			firstWait := time.Since(started)
			reference := fetchChannelBatch(t, direct, source, plan, start, end)
			batchWall := time.Since(started)
			if jobs := direct.enqueued(goshl.StreamChannel); len(jobs) != 1 || jobs[0].StartIndex != start || jobs[0].EndIndex != end || jobs[0].RequestedIndex != start+5 || jobs[0].Priority != goshl.PriorityChannel {
				t.Fatalf("mid-batch request enqueued %+v", jobs)
			}

			prefetched := startChannelNode(t, newTimelineStorage(), perJob, nil)
			if err := prefetched.controller.ChannelPrefetch(ctx, source.path, goshl.ChannelProfile{}, 0, start-2, 6); err != nil {
				t.Fatalf("prefetch: %v", err)
			}
			waitChannelStored(t, prefetched, plan, start-perJob, end)

			partial := startChannelNode(t, newTimelineStorage(), perJob, nil)
			for offset := 0; offset < 4; offset++ {
				if err := partial.storage.WriteSegment(ctx, plan.SegmentData(start+offset), reference[offset]); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := partial.controller.ChannelSegment(ctx, source.path, goshl.ChannelProfile{}, 0, start+7); err != nil {
				t.Fatalf("partly stored batch: %v", err)
			}
			waitChannelStored(t, partial, plan, start, end)

			exact := 0
			for offset := 0; offset < perJob; offset++ {
				index := start + offset
				stored := map[string][]byte{"mid-batch request": reference[offset], "prefetch": readChannelStored(t, prefetched, plan, index), "partly stored": readChannelStored(t, partial, plan, index)}
				ok := true
				for name, data := range stored {
					if problem := channelSegmentProblem(plan, index, probeChannelSegment(t, data)); problem != "" {
						ok = false
						t.Errorf("segment %d from %s: %s", index, name, problem)
					}
				}
				if ok {
					exact++
				}
			}
			lines = append(lines, fmt.Sprintf("%s: batch %d-%d produced three ways (request for %d on empty storage; prefetch of %d-%d on empty storage; request for %d with %d-%d already stored): %d of %d segments frame-exact in all three", source.key, start, end, start+5, start-2, start+3, start+7, start, start+3, exact, perJob))
			lines = append(lines, fmt.Sprintf("%s: cold request for segment %d (6th of its batch) returned after %.2f s; whole batch stored after %.2f s", source.key, start+5, firstWait.Seconds(), batchWall.Seconds()))

			other := startChannelNode(t, newTimelineStorage(), 4, nil)
			otherPlan, err := other.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if otherPlan.Rendition == plan.Rendition {
				t.Errorf("a different ChannelSegmentsPerJob must use another rendition name")
			}
			if !reflect.DeepEqual(otherPlan.Segments, plan.Segments) {
				t.Errorf("the plan must not depend on ChannelSegmentsPerJob")
			}
			sameTimes := 0
			for offset := 0; offset < perJob; offset++ {
				data, err := other.controller.ChannelSegment(ctx, source.path, goshl.ChannelProfile{}, 0, start+offset)
				if err != nil {
					t.Fatalf("segment %d at 4 per job: %v", start+offset, err)
				}
				if reflect.DeepEqual(probeChannelSegment(t, data).video, probeChannelSegment(t, reference[offset]).video) {
					sameTimes++
				}
			}
			if sameTimes != perJob {
				t.Errorf("only %d of %d segments keep their video timestamps at 4 segments per job", sameTimes, perJob)
			}
			lines = append(lines, fmt.Sprintf("%s: same segments at ChannelSegmentsPerJob=4 (rendition %s, separate cache): %d of %d with identical video timestamps and keyframe flags", source.key, otherPlan.Rendition, sameTimes, perJob))

			var previousVideo, previousAudio int64
			videoSteps := map[int64]int{}
			audioSteps := map[int64]int{}
			startErrors, frames := 0, 0
			for index := start - perJob; index <= end; index++ {
				facts := probeChannelSegment(t, readChannelStored(t, prefetched, plan, index))
				if len(facts.video) != 48 {
					t.Errorf("segment %d holds %d video frames", index, len(facts.video))
				}
				if !facts.video[0].key {
					t.Errorf("segment %d does not start with a keyframe", index)
				}
				for position, packet := range facts.video {
					if position > 0 && packet.key {
						t.Errorf("segment %d has a second keyframe at frame %d", index, position)
					}
				}
				if want := 10*channelTicks + channelFrameTicks(int64(index)*48, plan); facts.video[0].pts != want {
					startErrors++
					t.Errorf("segment %d starts at %d ticks, planned %d", index, facts.video[0].pts, want)
				}
				for _, packet := range facts.video {
					if previousVideo != 0 {
						videoSteps[packet.pts-previousVideo]++
					}
					previousVideo = packet.pts
					frames++
				}
				for _, packet := range facts.audio {
					if previousAudio != 0 {
						audioSteps[packet.pts-previousAudio]++
					}
					previousAudio = packet.pts
				}
			}
			for step := range videoSteps {
				if step != 3753 && step != 3754 {
					t.Errorf("video step of %d ticks", step)
				}
			}
			for step := range audioSteps {
				if step != 1920 {
					t.Errorf("audio step of %d ticks", step)
				}
			}
			lines = append(lines, fmt.Sprintf("%s: segments %d-%d (two batches, join at %d|%d): %d frames, video steps %v, audio steps %v, segment start errors %d", source.key, start-perJob, end, start-1, start, frames, videoSteps, audioSteps, startErrors))

			head := fetchChannelBatch(t, direct, source, plan, 0, 0)
			headFacts := probeChannelSegment(t, head[0])
			if headFacts.video[0].pts != 10*channelTicks {
				t.Errorf("item starts at %d ticks", headFacts.video[0].pts)
			}
			tail := fetchChannelBatch(t, direct, source, plan, count-2, count-1)
			tailFacts := probeChannelSegment(t, tail[1])
			lastSegment := plan.Segments[count-1]
			wantFrames := plan.Frames - (count-1)*48
			if len(tailFacts.video) != wantFrames {
				t.Errorf("last segment holds %d frames, plan says %d", len(tailFacts.video), wantFrames)
			}
			beforeTail := probeChannelSegment(t, tail[0])
			if step := tailFacts.video[0].pts - beforeTail.video[len(beforeTail.video)-1].pts; step != 3753 && step != 3754 {
				t.Errorf("step into the last segment is %d ticks", step)
			}
			audioEnd := float64(tailFacts.audio[len(tailFacts.audio)-1].pts+1920)/channelTicks - 10
			lines = append(lines, fmt.Sprintf("%s: first segment starts at %d ticks (10 s), first audio packet at %d; last segment %d holds %d frames (plan %d, %.6f s), audio ends at %.3f s for a plan of %.3f s", source.key, headFacts.video[0].pts, headFacts.audio[0].pts, count-1, len(tailFacts.video), wantFrames, lastSegment.Duration, audioEnd, plan.Duration))
			if failures := direct.recorder.of(goshl.EventJobFailed, ""); len(failures) != 0 {
				t.Errorf("jobs failed: %+v", failures)
			}
			if dir := os.Getenv(channelEvidenceEnv); dir != "" {
				keep := filepath.Join(dir, "segments", source.key)
				if err := os.MkdirAll(keep, 0o755); err != nil {
					t.Fatal(err)
				}
				saved := map[int][]byte{0: head[0], count - 2: tail[0], count - 1: tail[1]}
				for offset := 0; offset < perJob; offset++ {
					saved[start+offset] = reference[offset]
				}
				for index, data := range saved {
					if err := os.WriteFile(filepath.Join(keep, fmt.Sprintf("%05d.ts", index)), data, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				durations := make([]string, 0, count)
				for _, segment := range plan.Segments {
					durations = append(durations, strconv.FormatFloat(segment.Duration, 'f', 6, 64))
				}
				if err := os.WriteFile(filepath.Join(keep, "plan.txt"), []byte(strings.Join(durations, "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			channelEvidence(t, "segments-"+source.key, lines)
		})
	}
}

func TestChannelLibraryMetadataIsStripped(t *testing.T) {
	for _, source := range channelSources(t) {
		node := startChannelNode(t, newTimelineStorage(), 10, nil)
		data, err := node.controller.ChannelSegment(context.Background(), source.path, goshl.ChannelProfile{}, 0, 3)
		if err != nil {
			t.Fatalf("%s: %v", source.key, err)
		}
		for _, banned := range []string{"Lavc", "Lavf", "x264", "FFmpeg", "Service01"} {
			if bytes.Contains(data, []byte(banned)) {
				t.Errorf("%s: segment carries %q", source.key, banned)
			}
		}
		if !bytes.Contains(data, []byte("goshl")) || !bytes.Contains(data, []byte("channel")) {
			t.Errorf("%s: segment lacks the fixed service names", source.key)
		}
	}
}

func TestChannelLibraryPrefetchRunsInItsOwnLane(t *testing.T) {
	source := channelSources(t)[0]
	var lines []string
	node := startChannelNode(t, newTimelineStorage(), 10, func(options *goshl.Options) {
		options.VideoPoolSize = 1
		options.AudioPoolSize = 1
		options.SegmentsPerJob = 4
		options.TargetDuration = 6
	})
	ctx := context.Background()
	meta, err := node.controller.Metadata(ctx, source.path)
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
	plan, err := node.controller.ChannelPlan(ctx, source.path, goshl.ChannelProfile{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	since := func(at time.Time) float64 { return at.Sub(began).Seconds() }
	const vodJobs = 6
	var vodWait sync.WaitGroup
	for job := 0; job < vodJobs; job++ {
		vodWait.Add(1)
		go func() {
			defer vodWait.Done()
			if _, err := node.controller.Segment(ctx, source.path, goshl.StreamVideo, vod, 100+50*job); err != nil {
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
	lines = append(lines, fmt.Sprintf("%s: VOD lane saturated at %.2f s: 1 video worker, %d jobs enqueued for rendition %s, %d started", source.key, since(time.Now()), len(node.enqueued(goshl.StreamVideo)), vod, len(node.recorder.of(goshl.EventJobStarted, goshl.StreamVideo))))

	const first, lead, ticks = 1000, 15, 10
	slowest := time.Duration(0)
	for tick := 0; tick < ticks; tick++ {
		called := time.Now()
		if err := node.controller.ChannelPrefetch(ctx, source.path, goshl.ChannelProfile{}, 0, first+2*tick, lead); err != nil {
			t.Fatalf("prefetch tick %d: %v", tick, err)
		}
		if took := time.Since(called); took > slowest {
			slowest = took
		}
		time.Sleep(200 * time.Millisecond)
	}
	if slowest > 500*time.Millisecond {
		t.Errorf("a prefetch call took %v", slowest)
	}
	waitChannelStored(t, node, plan, first, first+39)
	channelDone := time.Now()
	videoDoneBefore := 0
	for _, event := range node.recorder.of(goshl.EventJobCompleted, goshl.StreamVideo) {
		if event.Time.Before(channelDone) {
			videoDoneBefore++
		}
	}
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
	if !reflect.DeepEqual(ranges, []string{"1000-1009", "1010-1019", "1020-1029", "1030-1039"}) {
		t.Errorf("%d overlapping prefetch calls enqueued %v", ticks, ranges)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("job %s was enqueued %d times", id, times)
		}
	}
	lines = append(lines, fmt.Sprintf("%s: %d prefetch calls 200 ms apart, ranges %d+%d to %d+%d: channel jobs enqueued %v, each once; slowest call %.0f ms", source.key, ticks, first, lead, first+2*(ticks-1), lead, ranges, float64(slowest.Microseconds())/1000))
	for _, event := range node.recorder.of(goshl.EventJobStarted, goshl.StreamChannel) {
		lines = append(lines, fmt.Sprintf("%s: channel job %d-%d started at %.2f s after %.0f ms in the queue", source.key, event.StartIndex, event.EndIndex, since(event.Time), float64(event.Duration.Microseconds())/1000))
	}
	for _, event := range node.recorder.of(goshl.EventJobCompleted, goshl.StreamChannel) {
		lines = append(lines, fmt.Sprintf("%s: channel job %d-%d completed at %.2f s, ran %.2f s", source.key, event.StartIndex, event.EndIndex, since(event.Time), event.Duration.Seconds()))
	}
	lines = append(lines, fmt.Sprintf("%s: all 40 channel segments stored at %.2f s; VOD jobs completed by then: %d of %d", source.key, since(channelDone), videoDoneBefore, vodJobs))
	if videoDoneBefore >= vodJobs-1 {
		t.Errorf("the VOD lane was not saturated while the channel lane ran: %d of %d VOD jobs had completed", videoDoneBefore, vodJobs)
	}
	vodWait.Wait()
	for wait := time.Now().Add(30 * time.Second); len(node.recorder.of(goshl.EventJobCompleted, goshl.StreamVideo)) < vodJobs && time.Now().Before(wait); {
		time.Sleep(50 * time.Millisecond)
	}
	for _, event := range node.recorder.of(goshl.EventJobCompleted, goshl.StreamVideo) {
		lines = append(lines, fmt.Sprintf("%s: VOD job %d-%d completed at %.2f s, ran %.2f s", source.key, event.StartIndex, event.EndIndex, since(event.Time), event.Duration.Seconds()))
	}
	for _, job := range node.enqueued(goshl.StreamVideo) {
		if job.Priority != goshl.PriorityPlayback {
			t.Errorf("VOD job has priority %d", job.Priority)
		}
	}
	if failures := node.recorder.of(goshl.EventJobFailed, ""); len(failures) != 0 {
		t.Errorf("jobs failed: %+v", failures)
	}
	channelEvidence(t, "lane-"+source.key, lines)
}
