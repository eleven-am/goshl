package engine

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/playlist"
)

const channelTestSource = "file:///channel-item"

const channelSourceFrameRate = 24.0

type channelStore struct {
	*stubStorage
	lock     sync.Mutex
	stored   map[domain.SegmentData][]byte
	existsOn []int
	deleted  []string
}

func newChannelStore(t *testing.T, duration float64) *channelStore {
	t.Helper()
	metadata, err := jsonMetadata(testVideoMetadata(duration, []float64{0, 6, 12, 18, 24}))
	if err != nil {
		t.Fatal(err)
	}
	return &channelStore{stubStorage: &stubStorage{metaData: metadata, metaExists: true}, stored: make(map[domain.SegmentData][]byte)}
}

func (s *channelStore) MetadataExists(ctx context.Context, mediaKey string) (bool, error) {
	return true, nil
}

func (s *channelStore) has(info domain.SegmentData) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	_, found := s.stored[info]
	return found
}

func (s *channelStore) remove(info domain.SegmentData) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.stored, info)
}

func (s *channelStore) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.stored[info] = data
	return nil
}

func (s *channelStore) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.stored[info], nil
}

func (s *channelStore) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.existsOn = append(s.existsOn, info.Index)
	_, ok := s.stored[info]
	return ok, nil
}

func (s *channelStore) DeleteMedia(ctx context.Context, mediaKey string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.deleted = append(s.deleted, mediaKey)
	return nil
}

func (s *channelStore) existsCalls() []int {
	s.lock.Lock()
	defer s.lock.Unlock()
	return append([]int(nil), s.existsOn...)
}

func (s *channelStore) resetExistsCalls() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.existsOn = nil
}

type jobLog struct {
	mu   sync.Mutex
	jobs []domain.Job
}

func (l *jobLog) record(job domain.Job) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jobs = append(l.jobs, job)
}

func (l *jobLog) ranges(stream domain.StreamType) [][2]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ranges [][2]int
	for _, job := range l.jobs {
		if job.StreamType == stream {
			ranges = append(ranges, [2]int{job.StartIndex, job.EndIndex})
		}
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	return ranges
}

func (l *jobLog) all() []domain.Job {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.Job(nil), l.jobs...)
}

func newChannelEngine(store domain.Storage, coordinator domain.Coordinator, mutate func(*Config)) *Engine {
	config := Config{
		Storage:         store,
		Coordinator:     coordinator,
		PathGen:         stubPathGen{},
		HWAccelMode:     domain.AccelNone,
		SegmentTimeout:  2 * time.Second,
		JobTimeout:      time.Minute,
		ChannelPoolSize: 1,
	}
	if mutate != nil {
		mutate(&config)
	}
	return mustNew(config)
}

func TestChannelPlanIsAPureFunctionOfDurationAndProfile(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = log.record
	first := newChannelEngine(store, coordinator, nil)
	second := newChannelEngine(store, coordinator, nil)
	plan, err := first.ChannelPlan(context.Background(), channelTestSource, domain.ChannelProfile{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := second.ChannelPlan(context.Background(), channelTestSource, domain.ChannelProfile{Height: 720, FrameRateNum: 48000, FrameRateDen: 2002, SegmentFrames: 48, VideoBitrate: 2_500_000, AudioBitrate: 160_000}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, other) {
		t.Fatalf("replicas disagree on the plan:\n%+v\n%+v", plan, other)
	}
	normalised, _ := domain.ChannelProfile{}.Normalised()
	if !reflect.DeepEqual(plan.Segments, playlist.CalculateChannelSegments(100, normalised)) {
		t.Fatal("the plan is not the pure function of the stored duration")
	}
	if plan.Frames != 2398 || len(plan.Segments) != 50 || plan.Codecs != domain.ChannelCodecs || !strings.HasPrefix(plan.Rendition, "channel-720p-a0-") {
		t.Fatalf("unexpected plan: %d frames, %d segments, %s, %s", plan.Frames, len(plan.Segments), plan.Codecs, plan.Rendition)
	}
	if plan.SegmentData(7) != (domain.SegmentData{MediaKey: plan.MediaKey, Index: 7, Rendition: plan.Rendition, IsVideo: true}) {
		t.Fatalf("unexpected segment data %+v", plan.SegmentData(7))
	}
	if key, _ := first.MediaKey(context.Background(), channelTestSource); key != plan.MediaKey {
		t.Fatal("channel segments must live under the VOD media key")
	}
	if len(log.all()) != 0 || len(store.existsCalls()) != 0 {
		t.Fatal("planning must not enqueue or touch segment storage")
	}
}

func TestChannelPrefetchEnqueuesEachGridBatchOnce(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = log.record
	built := newChannelEngine(store, coordinator, nil)
	defer built.Stop()
	ctx := context.Background()
	prefetch := func(from, count int) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, from, count) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("prefetch %d+%d: %v", from, count, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("prefetch %d+%d blocked although no worker ever runs", from, count)
		}
	}
	prefetch(12, 15)
	if got := log.ranges(domain.StreamChannel); !reflect.DeepEqual(got, [][2]int{{10, 19}, {20, 29}}) {
		t.Fatalf("first prefetch enqueued %v", got)
	}
	store.resetExistsCalls()
	for tick := 0; tick < 5; tick++ {
		prefetch(12, 15)
	}
	prefetch(13, 15)
	prefetch(14, 16)
	if got := log.ranges(domain.StreamChannel); len(got) != 2 {
		t.Fatalf("overlapping prefetches enqueued again: %v", got)
	}
	if calls := store.existsCalls(); len(calls) != 0 {
		t.Fatalf("covered indices cost storage calls: %v", calls)
	}
	prefetch(16, 15)
	prefetch(18, 15)
	if got := log.ranges(domain.StreamChannel); !reflect.DeepEqual(got, [][2]int{{10, 19}, {20, 29}, {30, 39}}) {
		t.Fatalf("advancing the range enqueued %v", got)
	}
	prefetch(45, 500)
	if got := log.ranges(domain.StreamChannel); !reflect.DeepEqual(got, [][2]int{{10, 19}, {20, 29}, {30, 39}, {40, 49}}) {
		t.Fatalf("a range past the end must be clamped: %v", got)
	}
	for _, job := range log.all() {
		if job.Priority != domain.PriorityChannel || job.Channel == nil || job.VideoRendition == nil || job.AudioRendition == nil || job.Profile != built.routeProfile {
			t.Fatalf("incomplete channel job: %+v", job)
		}
		if job.StartIndex > 0 && job.Overlap == nil || job.EndIndex < 49 && job.Trailing == nil {
			t.Fatalf("channel job %d-%d lacks its neighbours", job.StartIndex, job.EndIndex)
		}
		if job.Digest != domain.JobDigest(job) {
			t.Fatal("channel job digest is wrong")
		}
		if job.Channel.SourceFrameRate != channelSourceFrameRate {
			t.Fatalf("channel job source frame rate = %v, want %v", job.Channel.SourceFrameRate, channelSourceFrameRate)
		}
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 50, 3); !errors.Is(err, ErrSegmentOutOfRange) {
		t.Fatalf("prefetch past the end: %v", err)
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, -1, 3); !errors.Is(err, ErrSegmentOutOfRange) {
		t.Fatalf("negative prefetch: %v", err)
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 5, 0); err != nil {
		t.Fatalf("empty prefetch: %v", err)
	}
}

func TestChannelPrefetchSkipsStoredSegments(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = log.record
	built := newChannelEngine(store, coordinator, nil)
	defer built.Stop()
	ctx := context.Background()
	plan, err := built.ChannelPlan(ctx, channelTestSource, domain.ChannelProfile{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for index := 10; index < 30; index++ {
		store.WriteSegment(ctx, plan.SegmentData(index), []byte("stored"))
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 12, 15); err != nil {
		t.Fatal(err)
	}
	if got := log.ranges(domain.StreamChannel); len(got) != 0 {
		t.Fatalf("a stored range enqueued %v", got)
	}
	store.remove(plan.SegmentData(25))
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 12, 15); err != nil {
		t.Fatal(err)
	}
	if got := log.ranges(domain.StreamChannel); !reflect.DeepEqual(got, [][2]int{{20, 29}}) {
		t.Fatalf("a hole must re-run its whole grid batch, got %v", got)
	}
}

func TestChannelPrefetchOnTwoReplicasRunsEachBatchOnce(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = log.record
	first := newChannelEngine(store, coordinator, nil)
	second := newChannelEngine(store, coordinator, nil)
	defer first.Stop()
	defer second.Stop()
	ctx := context.Background()
	var wait sync.WaitGroup
	for round := 0; round < 4; round++ {
		for _, replica := range []*Engine{first, second} {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if err := replica.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 3+round, 20); err != nil {
					t.Errorf("prefetch: %v", err)
				}
			}()
		}
	}
	wait.Wait()
	if got := log.ranges(domain.StreamChannel); !reflect.DeepEqual(got, [][2]int{{0, 9}, {10, 19}, {20, 29}}) {
		t.Fatalf("two replicas enqueued %v", got)
	}
	if released := coordinator.releaseCount(); released != 0 {
		t.Fatalf("%d locks were released although every job is still queued", released)
	}
}

func TestChannelSegmentJoinsPrefetchFlight(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = log.record
	built := newChannelEngine(store, coordinator, nil)
	defer built.Stop()
	ctx := context.Background()
	plan, err := built.ChannelPlan(ctx, channelTestSource, domain.ChannelProfile{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 20, 5); err != nil {
		t.Fatal(err)
	}
	result := make(chan []byte, 1)
	go func() {
		data, err := built.ChannelSegment(ctx, channelTestSource, domain.ChannelProfile{}, 0, 27)
		if err != nil {
			t.Errorf("segment: %v", err)
		}
		result <- data
	}()
	time.Sleep(50 * time.Millisecond)
	if got := log.ranges(domain.StreamChannel); !reflect.DeepEqual(got, [][2]int{{20, 29}}) {
		t.Fatalf("the request must join the prefetch flight, jobs %v", got)
	}
	info := plan.SegmentData(27)
	store.WriteSegment(ctx, info, []byte("produced"))
	if err := coordinator.NotifySegment(ctx, info, domain.SegmentStatus{State: domain.SegmentStateReady}); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-result:
		if string(data) != "produced" {
			t.Fatalf("unexpected data %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("the joined request did not return")
	}
}

func TestChannelSegmentMissRunsTheWholeGridBatch(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = func(job domain.Job) {
		log.record(job)
		for _, index := range job.ExpectedIndices {
			store.WriteSegment(context.Background(), domain.SegmentData{MediaKey: job.MediaKey, Index: index, Rendition: job.Rendition, IsVideo: true}, []byte("made"))
		}
	}
	built := newChannelEngine(store, coordinator, func(config *Config) { config.ChannelSegmentsPerJob = 6 })
	defer built.Stop()
	data, err := built.ChannelSegment(context.Background(), channelTestSource, domain.ChannelProfile{}, 0, 16)
	if err != nil || string(data) != "made" {
		t.Fatalf("segment: %q, %v", data, err)
	}
	jobs := log.all()
	if len(jobs) != 1 || jobs[0].StartIndex != 12 || jobs[0].EndIndex != 17 || jobs[0].RequestedIndex != 16 || jobs[0].Overlap.Index != 11 || jobs[0].Trailing.Index != 18 {
		t.Fatalf("unexpected job %+v", jobs)
	}
	if _, err := built.ChannelSegment(context.Background(), channelTestSource, domain.ChannelProfile{}, 0, 50); !errors.Is(err, ErrSegmentOutOfRange) {
		t.Fatalf("out of range: %v", err)
	}
}

func TestChannelPrefetchRemembersAFailedRun(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = log.record
	built := newChannelEngine(store, coordinator, nil)
	defer built.Stop()
	now := time.Now()
	built.channel.now = func() time.Time { return now }
	ctx := context.Background()
	deliveries, err := coordinator.Subscribe(ctx, domain.StreamChannel, built.routeProfile)
	if err != nil {
		t.Fatal(err)
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 0, 5); err != nil {
		t.Fatal(err)
	}
	delivery := <-deliveries
	failed := domain.JobStatus{JobID: delivery.Job.ID, RunID: delivery.Job.RunID, State: domain.JobStateFailed, Error: "source unreadable"}
	if err := coordinator.SettleJob(ctx, delivery.Receipt, failed); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, time.Second, func() bool {
		return built.coveringFlight(delivery.Job.MediaKey, domain.StreamChannel, delivery.Job.Rendition, 0) == nil
	})
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 0, 5); err == nil || !strings.Contains(err.Error(), "source unreadable") {
		t.Fatalf("a prefetch right after a failed run must report it, got %v", err)
	}
	if got := log.ranges(domain.StreamChannel); len(got) != 1 {
		t.Fatalf("the failed batch was enqueued again: %v", got)
	}
	if err := built.Invalidate(ctx, channelTestSource); err != nil {
		t.Fatal(err)
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 0, 5); err != nil {
		t.Fatalf("prefetch after Invalidate: %v", err)
	}
	if got := log.ranges(domain.StreamChannel); len(got) != 2 {
		t.Fatalf("Invalidate must clear the failure: %v", got)
	}
	second := <-deliveries
	failed = domain.JobStatus{JobID: second.Job.ID, RunID: second.Job.RunID, State: domain.JobStateFailed, Error: "source unreadable"}
	if err := coordinator.SettleJob(ctx, second.Receipt, failed); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, time.Second, func() bool {
		return built.coveringFlight(second.Job.MediaKey, domain.StreamChannel, second.Job.Rendition, 0) == nil
	})
	now = now.Add(channelFailureTTL + time.Second)
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 0, 5); err != nil {
		t.Fatalf("prefetch after the failure expired: %v", err)
	}
	if got := log.ranges(domain.StreamChannel); len(got) != 3 {
		t.Fatalf("an expired failure must allow a retry: %v", got)
	}
}

type subscribeLog struct {
	*clusterCoordinator
	mu      sync.Mutex
	streams []domain.StreamType
}

func (c *subscribeLog) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	c.mu.Lock()
	c.streams = append(c.streams, streamType)
	c.mu.Unlock()
	return c.clusterCoordinator.Subscribe(ctx, streamType, profile)
}

func TestChannelAPIIsOffByDefault(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := &subscribeLog{clusterCoordinator: newClusterCoordinator()}
	built := newChannelEngine(store, coordinator, func(config *Config) { config.ChannelPoolSize = 0 })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := built.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer built.Stop()
	if !reflect.DeepEqual(coordinator.streams, []domain.StreamType{domain.StreamVideo, domain.StreamAudio}) {
		t.Fatalf("a host without channels subscribed to %v", coordinator.streams)
	}
	if _, err := built.ChannelPlan(ctx, channelTestSource, domain.ChannelProfile{}, 0); !errors.Is(err, ErrChannelDisabled) {
		t.Fatalf("plan: %v", err)
	}
	if _, err := built.ChannelSegment(ctx, channelTestSource, domain.ChannelProfile{}, 0, 0); !errors.Is(err, ErrChannelDisabled) {
		t.Fatalf("segment: %v", err)
	}
	if err := built.ChannelPrefetch(ctx, channelTestSource, domain.ChannelProfile{}, 0, 0, 1); !errors.Is(err, ErrChannelDisabled) {
		t.Fatalf("prefetch: %v", err)
	}

	enabled := &subscribeLog{clusterCoordinator: newClusterCoordinator()}
	withChannels := newChannelEngine(store, enabled, nil)
	if err := withChannels.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer withChannels.Stop()
	if !reflect.DeepEqual(enabled.streams, []domain.StreamType{domain.StreamVideo, domain.StreamAudio, domain.StreamChannel}) {
		t.Fatalf("a host with channels subscribed to %v", enabled.streams)
	}
}

func TestChannelRequestsAreValidated(t *testing.T) {
	store := newChannelStore(t, 100)
	built := newChannelEngine(store, newClusterCoordinator(), nil)
	defer built.Stop()
	ctx := context.Background()
	for name, profile := range map[string]domain.ChannelProfile{
		"odd height":       {Height: 481},
		"tall":             {Height: 1080},
		"slow":             {FrameRateNum: 1, FrameRateDen: 2},
		"fast":             {FrameRateNum: 120, FrameRateDen: 1},
		"short segment":    {SegmentFrames: 10},
		"long segment":     {SegmentFrames: 480},
		"tiny video rate":  {VideoBitrate: 1000},
		"huge audio rate":  {AudioBitrate: 1_000_000},
		"negative frames":  {SegmentFrames: -48},
		"negative divisor": {FrameRateNum: 24000, FrameRateDen: -1001},
	} {
		if _, err := built.ChannelPlan(ctx, channelTestSource, profile, 0); !errors.Is(err, ErrInvalidChannelProfile) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := built.ChannelPlan(ctx, channelTestSource, domain.ChannelProfile{}, 1); !errors.Is(err, ErrRenditionNotFound) {
		t.Fatalf("audio track past the end: %v", err)
	}
	if _, err := built.ChannelPlan(ctx, channelTestSource, domain.ChannelProfile{}, -1); !errors.Is(err, ErrRenditionNotFound) {
		t.Fatalf("negative audio track: %v", err)
	}
	if _, err := built.ChannelSegment(ctx, channelTestSource, domain.ChannelProfile{}, 0, -1); !errors.Is(err, ErrSegmentOutOfRange) {
		t.Fatalf("negative index: %v", err)
	}
	store.resetExistsCalls()
	if _, err := built.Segment(ctx, channelTestSource, domain.StreamVideo, "channel-720p-a0-1a18aabd6d89", 0); !errors.Is(err, ErrRenditionNotFound) {
		t.Fatalf("the VOD API must reject channel renditions: %v", err)
	}
	if calls := store.existsCalls(); len(calls) != 0 {
		t.Fatalf("a channel rendition reached segment storage through the VOD API: %v", calls)
	}
	if _, err := built.VariantPlaylist(ctx, channelTestSource, domain.StreamVideo, "channel-720p-a0-1a18aabd6d89"); !errors.Is(err, ErrRenditionNotFound) {
		t.Fatalf("the VOD playlist must reject channel renditions: %v", err)
	}
	built.Stop()
	if _, err := built.ChannelPlan(ctx, channelTestSource, domain.ChannelProfile{}, 0); !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("stopped controller: %v", err)
	}
}

func TestChannelJobsRunWhileVideoLaneIsSaturated(t *testing.T) {
	store := newChannelStore(t, 100)
	coordinator := newClusterCoordinator()
	log := &jobLog{}
	coordinator.onEnqueue = log.record
	releaseVideo := make(chan struct{})
	videoStarted := make(chan struct{}, 8)
	coordinator.onRenew = func(ctx context.Context, job domain.Job) error {
		if job.StreamType == domain.StreamVideo {
			select {
			case videoStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseVideo:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, index := range job.ExpectedIndices {
			store.WriteSegment(ctx, domain.SegmentData{MediaKey: job.MediaKey, Index: index, Rendition: job.Rendition, IsVideo: true}, []byte(job.StreamType))
		}
		return nil
	}
	built := newChannelEngine(store, coordinator, func(config *Config) {
		config.VideoPoolSize = 1
		config.SegmentsPerJob = 1
		config.TargetDuration = 6
	})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := built.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer built.Stop()
	defer close(releaseVideo)

	for index := 0; index < 4; index++ {
		go built.Segment(ctx, channelTestSource, domain.StreamVideo, "1080p", index)
	}
	<-videoStarted
	waitUntil(t, time.Second, func() bool { return len(log.ranges(domain.StreamVideo)) == 4 })

	data, err := built.ChannelSegment(ctx, channelTestSource, domain.ChannelProfile{}, 0, 3)
	if err != nil {
		t.Fatalf("channel segment behind a saturated video lane: %v", err)
	}
	if string(data) != string(domain.StreamChannel) {
		t.Fatalf("unexpected channel data %q", data)
	}
	select {
	case <-videoStarted:
		t.Fatal("a second video job started on a pool of one")
	default:
	}
	if store.has(domain.SegmentData{MediaKey: log.all()[0].MediaKey, Index: 0, Rendition: "1080p", IsVideo: true}) {
		t.Fatal("the blocked video job must not have produced anything yet")
	}
	for _, job := range log.all() {
		want := domain.PriorityPlayback
		if job.StreamType == domain.StreamChannel {
			want = domain.PriorityChannel
		}
		if job.Priority != want {
			t.Fatalf("%s job has priority %d", job.StreamType, job.Priority)
		}
	}
}

func TestRouteProfileSeparatesRoutingFromCacheIdentity(t *testing.T) {
	config := Config{CacheVersion: "release-7", TargetDuration: 6}
	pipeline := pipelineVersion(config, hwaccel.NewConfig(domain.AccelNone))
	if pipeline != pipelineIdentity(config, hwaccel.NewConfig(domain.AccelNone), domain.OutputTimelineOffset, 5) {
		t.Fatal("the cache identity must stay on output contract 5")
	}
	if domain.JobContractVersion != 6 || domain.OutputContractVersion != 5 {
		t.Fatalf("contract versions are %d and %d", domain.JobContractVersion, domain.OutputContractVersion)
	}
	if routeProfile(pipeline) == pipeline || routeIdentity(pipeline, 5) == routeIdentity(pipeline, 6) {
		t.Fatal("the route must change with the job contract")
	}
	if got := routeProfile(pipeline); got != "goshl:a1ce2cb5cbd24507465dde785e2b9158100454dce7dd75b84c12765635fd6bd6" {
		t.Fatalf("route profile = %s", got)
	}
	if got := deterministicJobID("goshl:media", "channel-720p-a0-1a18aabd6d89", domain.StreamChannel, 10, 19); got != "ab610fc12230423a" {
		t.Fatalf("channel job id = %s", got)
	}
	built := mustNew(Config{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, HWAccelMode: domain.AccelNone, CacheVersion: "release-7"})
	if built.routeProfile != routeProfile(built.pipelineVersion) {
		t.Fatal("the engine must route on the derived profile")
	}
}
