package transcode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/encoderslots"
	"github.com/eleven-am/goshl/internal/ffmpeg"
)

const maxResubscribeDelay = 30 * time.Second

var ErrDeliveryLeaseLost = errors.New("job delivery lease lost")
var ErrJobExpired = errors.New("job start-by time expired")

type Pool struct {
	coordinator domain.Coordinator
	size        int
	streamType  domain.StreamType
	storage     domain.Storage
	cmdBuilder  *ffmpeg.CommandBuilder
	segStorage  domain.Storage
	jobTimeout  time.Duration
	observer    domain.Observer
	profile     string
	slots       *encoderslots.Budget
	newBackoff  func() backoff.BackOff

	runMu   sync.Mutex
	running map[string]context.CancelCauseFunc

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     *sync.WaitGroup
}

type subscription struct {
	mu       sync.Mutex
	ch       <-chan domain.JobDelivery
	gen      int
	received atomic.Bool
	delays   backoff.BackOff
}

func (s *subscription) current() (<-chan domain.JobDelivery, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ch, s.gen
}

type PoolConfig struct {
	JobTimeout time.Duration
	Observer   domain.Observer
	Profile    string
	Slots      *encoderslots.Budget
}

type leaseSettlement struct {
	mu         sync.Mutex
	settling   bool
	renewalErr error
}

func (s *leaseSettlement) begin() {
	s.mu.Lock()
	s.settling = true
	s.mu.Unlock()
}

func (s *leaseSettlement) deferRenewalFailure(err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.settling {
		return false
	}
	if s.renewalErr == nil {
		s.renewalErr = err
	}
	return true
}

func (s *leaseSettlement) finish(settled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if settled {
		return nil
	}
	return s.renewalErr
}

func NewPool(
	coordinator domain.Coordinator,
	size int,
	streamType domain.StreamType,
	storage domain.Storage,
	cmdBuilder *ffmpeg.CommandBuilder,
	segStorage domain.Storage,
	settings ...PoolConfig,
) *Pool {
	config := PoolConfig{JobTimeout: 2 * time.Minute}
	if len(settings) > 0 {
		config = settings[0]
		if config.JobTimeout <= 0 {
			config.JobTimeout = 2 * time.Minute
		}
	}
	return &Pool{
		coordinator: coordinator,
		size:        size,
		streamType:  streamType,
		storage:     storage,
		cmdBuilder:  cmdBuilder,
		segStorage:  segStorage,
		jobTimeout:  config.JobTimeout,
		observer:    config.Observer,
		profile:     config.Profile,
		slots:       config.Slots,
		running:     make(map[string]context.CancelCauseFunc),
		newBackoff:  defaultResubscribeBackoff,
	}
}

func defaultResubscribeBackoff() backoff.BackOff {
	delays := backoff.NewExponentialBackOff()
	delays.InitialInterval = 500 * time.Millisecond
	delays.MaxInterval = maxResubscribeDelay
	return delays
}

func (p *Pool) beginJob(runID string, cancel context.CancelCauseFunc) bool {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if _, ok := p.running[runID]; ok {
		return false
	}
	p.running[runID] = cancel
	return true
}

func (p *Pool) endJob(runID string) {
	p.runMu.Lock()
	delete(p.running, runID)
	p.runMu.Unlock()
}

func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return fmt.Errorf("pool already started")
	}
	ctx, cancel := context.WithCancel(ctx)

	jobs, err := p.coordinator.Subscribe(ctx, p.streamType, p.profile)
	if err != nil {
		cancel()
		return fmt.Errorf("subscribe: %w", err)
	}
	newBackoff := p.newBackoff
	if newBackoff == nil {
		newBackoff = defaultResubscribeBackoff
	}
	sub := &subscription{ch: jobs, delays: newBackoff()}
	wg := &sync.WaitGroup{}
	p.cancel = cancel
	p.wg = wg

	for i := 0; i < p.size; i++ {
		wg.Add(1)
		go p.worker(ctx, wg, sub)
	}

	return nil
}

func (p *Pool) Stop() {
	p.mu.Lock()
	cancel, wg := p.cancel, p.wg
	p.cancel, p.wg = nil, nil
	p.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	wg.Wait()
}

func (p *Pool) worker(ctx context.Context, wg *sync.WaitGroup, sub *subscription) {
	defer wg.Done()

	for {
		jobs, gen := sub.current()
		select {
		case <-ctx.Done():
			return
		case delivery, ok := <-jobs:
			if !ok {
				if !p.resubscribe(ctx, sub, gen) {
					return
				}
				continue
			}
			sub.received.Store(true)
			p.processJob(ctx, delivery)
		}
	}
}

func (p *Pool) resubscribe(ctx context.Context, sub *subscription, gen int) bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if sub.gen != gen {
		return true
	}
	p.observe(ctx, domain.Event{Type: domain.EventPoolSubscriptionLost, StreamType: p.streamType, Error: "job subscription closed"})
	if sub.received.Swap(false) {
		sub.delays.Reset()
	}
	for {
		delay := sub.delays.NextBackOff()
		if delay == backoff.Stop {
			delay = maxResubscribeDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		jobs, err := p.coordinator.Subscribe(ctx, p.streamType, p.profile)
		if err == nil {
			sub.ch = jobs
			sub.gen++
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		p.observe(ctx, domain.Event{Type: domain.EventPoolSubscriptionLost, StreamType: p.streamType, Error: err.Error()})
	}
}

func (p *Pool) processJob(ctx context.Context, delivery domain.JobDelivery) {
	job := delivery.Job
	started := time.Now()
	baseCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	if err := p.validateDelivery(delivery); err != nil {
		p.rejectDelivery(delivery, err, started)
		return
	}
	if !p.beginJob(job.RunID, cancelCause) {
		return
	}
	defer p.endJob(job.RunID)
	if !job.StartBy.After(time.Now()) {
		p.failJob(context.Background(), delivery, ErrJobExpired, started)
		return
	}
	jobCtx, cancel := p.jobContext(baseCtx)
	defer cancel()
	leaseTTL := p.leaseTTL()
	if err := p.coordinator.Renew(jobCtx, delivery.Receipt, leaseTTL); err != nil {
		p.observe(jobCtx, domain.Event{Type: domain.EventJobLeaseFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, Error: err.Error(), Duration: time.Since(started)})
		return
	}
	leaseCtx, stopLease := context.WithCancel(baseCtx)
	leaseDone := make(chan struct{})
	settlement := &leaseSettlement{}
	go func() {
		defer close(leaseDone)
		p.renewLease(leaseCtx, delivery, leaseTTL, cancelCause, started, settlement)
	}()
	var stopLeaseOnce sync.Once
	finishLease := func() {
		stopLeaseOnce.Do(func() {
			stopLease()
			<-leaseDone
		})
	}
	defer finishLease()
	finishSettlement := func(settled bool) {
		finishLease()
		if err := settlement.finish(settled); err != nil {
			p.reportLeaseFailure(delivery, err, cancelCause, started)
		}
	}
	fail := func(err error) {
		settlement.begin()
		finishSettlement(p.failJob(jobCtx, delivery, err, started))
	}
	complete := func() {
		settlement.begin()
		finishSettlement(p.completeJob(delivery, started))
	}
	p.observe(jobCtx, domain.Event{Type: domain.EventJobStarted, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Duration: time.Since(job.EnqueuedAt)})
	if p.segmentsExist(jobCtx, job) {
		complete()
		return
	}
	segments := job.Segments
	if len(segments) == 0 {
		fail(fmt.Errorf("no segments for range %d-%d", job.StartIndex, job.EndIndex))
		return
	}

	tmpDir, err := os.MkdirTemp("", "transcode-*")
	if err != nil {
		fail(fmt.Errorf("create temp dir: %w", err))
		return
	}
	defer os.RemoveAll(tmpDir)

	isVideo := p.streamType != domain.StreamAudio

	var args []string
	if p.streamType == domain.StreamChannel {
		args, err = p.cmdBuilder.Channel(p.channelParams(job, segments, tmpDir))
	} else if isVideo {
		params := ffmpeg.VideoParams{
			InputURL:    job.SourceURL,
			StreamIndex: job.VideoStreamIndex,
			Rendition:   *job.VideoRendition,
			Segments:    segments,
			Overlap:     job.Overlap,
			Trailing:    job.Trailing,
			OutputDir:   tmpDir,
		}
		if job.VideoRendition.Method == domain.DirectStream {
			landing, err := p.cmdBuilder.SeekLanding(jobCtx, job.SourceURL, jobSeekStart(job), job.VideoStreamIndex)
			if err != nil {
				fail(err)
				return
			}
			params.Landing = &landing
		} else if jobSeekStart(job) <= domain.BoundaryTolerance {
			landing, err := p.cmdBuilder.SeekLanding(jobCtx, job.SourceURL, 0, job.VideoStreamIndex)
			if err != nil {
				fail(err)
				return
			}
			params.Landing = &landing
		}
		args, err = p.cmdBuilder.Video(params)
	} else {
		params := ffmpeg.AudioParams{
			InputURL:    job.SourceURL,
			StreamIndex: job.AudioRendition.StreamIndex,
			Rendition:   *job.AudioRendition,
			Segments:    segments,
			Overlap:     job.Overlap,
			Trailing:    job.Trailing,
			OutputDir:   tmpDir,
		}
		if job.AudioRendition.Method == domain.DirectStream {
			landing, err := p.cmdBuilder.SeekLanding(jobCtx, job.SourceURL, params.LandingStart(), job.AudioRendition.StreamIndex)
			if err != nil {
				fail(err)
				return
			}
			params.Landing = &landing
		}
		args, err = p.cmdBuilder.Audio(params)
	}
	if err != nil {
		fail(err)
		return
	}

	if p.streamType == domain.StreamVideo && job.VideoRendition != nil && job.VideoRendition.Method == domain.Transcode {
		if err := p.slots.Acquire(jobCtx); err != nil {
			fail(err)
			return
		}
		defer p.slots.Release()
	}

	w := NewWorker(WorkerSpec{
		Args:      args,
		Storage:   p.segStorage,
		MediaKey:  job.MediaKey,
		Rendition: job.Rendition,
		IsVideo:   isVideo,
		TmpDir:    tmpDir,
		Expected:  job.ExpectedIndices,
		Overlap:   segmentIndex(job.Overlap),
		Trailing:  segmentIndex(job.Trailing),
		Ends:      segmentEnds(job, isVideo),
	})
	if err := w.Start(jobCtx); err != nil {
		fail(err)
		return
	}

	if err := p.waitForWorker(jobCtx, w); err != nil {
		fail(err)
		return
	}

	complete()
}

const (
	videoEndTolerance = 0.1
	audioEndTolerance = 0.06
)

func segmentEnds(job domain.Job, isVideo bool) map[int]SegmentEnd {
	tolerance := audioEndTolerance
	if isVideo {
		tolerance = videoEndTolerance
	}
	offset := domain.OutputTimelineOffset.Seconds()
	ends := make(map[int]SegmentEnd, len(job.Segments)+1)
	if job.Overlap != nil {
		ends[job.Overlap.Index] = SegmentEnd{Time: offset + job.Overlap.End, Early: tolerance, Late: tolerance}
	}
	for position, segment := range job.Segments {
		end := SegmentEnd{Time: offset + segment.End, Early: tolerance, Late: tolerance}
		if position == len(job.Segments)-1 && job.Trailing == nil {
			end.Early = domain.StreamTailSlack
		}
		ends[segment.Index] = end
	}
	return ends
}

func segmentIndex(segment *domain.Segment) *int {
	if segment == nil {
		return nil
	}
	index := segment.Index
	return &index
}

func jobSeekStart(job domain.Job) float64 {
	if job.Overlap != nil {
		return job.Overlap.Start
	}
	return job.Segments[0].Start
}

func (p *Pool) validateDelivery(delivery domain.JobDelivery) error {
	if delivery.Receipt == "" {
		return fmt.Errorf("invalid job contract: delivery receipt is missing")
	}
	return p.validateJob(delivery.Job)
}

func (p *Pool) leaseTTL() time.Duration {
	ttl := p.jobTimeout + 15*time.Second
	if ttl > 30*time.Second {
		ttl = 30 * time.Second
	}
	if ttl < 15*time.Second {
		ttl = 15 * time.Second
	}
	return ttl
}

func (p *Pool) renewLease(ctx context.Context, delivery domain.JobDelivery, ttl time.Duration, cancel context.CancelCauseFunc, started time.Time, settlement *leaseSettlement) {
	ticker := time.NewTicker(ttl / 3)
	defer ticker.Stop()
	lastRenewal := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, stop := context.WithTimeout(ctx, ttl/3)
			err := p.coordinator.Renew(renewCtx, delivery.Receipt, ttl)
			stop()
			if err == nil {
				lastRenewal = time.Now()
				continue
			}
			if time.Now().Before(lastRenewal.Add(ttl - ttl/3)) {
				continue
			}
			if !settlement.deferRenewalFailure(err) {
				p.reportLeaseFailure(delivery, err, cancel, started)
			}
			return
		}
	}
}

func (p *Pool) reportLeaseFailure(delivery domain.JobDelivery, err error, cancel context.CancelCauseFunc, started time.Time) {
	job := delivery.Job
	p.observe(context.Background(), domain.Event{Type: domain.EventJobLeaseFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, Error: err.Error(), Duration: time.Since(started)})
	cancel(fmt.Errorf("%w: %v", ErrDeliveryLeaseLost, err))
}

func (p *Pool) validateJob(job domain.Job) error {
	if job.ID == "" || job.RunID == "" || job.MediaKey == "" || job.Profile == "" || job.SourceURL == "" || job.Rendition == "" || job.EnqueuedAt.IsZero() || job.StartBy.IsZero() {
		return fmt.Errorf("invalid job contract: required identity is missing")
	}
	if p.profile != "" && job.Profile != p.profile {
		return fmt.Errorf("invalid job contract: profile mismatch")
	}
	if job.StreamType != p.streamType || len(job.Segments) == 0 || len(job.ExpectedIndices) != len(job.Segments) {
		return fmt.Errorf("invalid job contract: stream or segment range mismatch")
	}
	if job.StartIndex != job.Segments[0].Index || job.EndIndex != job.Segments[len(job.Segments)-1].Index || job.RequestedIndex < job.StartIndex || job.RequestedIndex > job.EndIndex {
		return fmt.Errorf("invalid job contract: indexes are inconsistent")
	}
	for index, segment := range job.Segments {
		if segment.Index != job.StartIndex+index || job.ExpectedIndices[index] != segment.Index || segment.Start < 0 || segment.End <= segment.Start || segment.Duration <= 0 || math.Abs(segment.Duration-(segment.End-segment.Start)) > domain.BoundaryTolerance {
			return fmt.Errorf("invalid job contract: segment %d is inconsistent", segment.Index)
		}
	}
	if p.streamType == domain.StreamVideo && job.VideoRendition == nil || p.streamType == domain.StreamAudio && job.AudioRendition == nil {
		return fmt.Errorf("invalid job contract: rendition parameters are missing")
	}
	if p.streamType == domain.StreamChannel {
		return validateChannelJob(job)
	}
	if job.Channel != nil {
		return fmt.Errorf("invalid job contract: channel parameters on a %s job", p.streamType)
	}
	if job.VideoRendition != nil && job.VideoRendition.Name != job.Rendition || job.AudioRendition != nil && job.AudioRendition.Name != job.Rendition {
		return fmt.Errorf("invalid job contract: rendition identity is inconsistent")
	}
	if err := validateRendition(job); err != nil {
		return err
	}
	copiedVideo := p.streamType == domain.StreamVideo && job.VideoRendition.Method == domain.DirectStream
	if job.Overlap == nil && job.StartIndex > 0 && (copiedVideo || p.streamType == domain.StreamAudio) {
		return fmt.Errorf("invalid job contract: overlap is missing")
	}
	if job.Overlap != nil && !domain.Adjacent(*job.Overlap, job.Segments[0]) {
		return fmt.Errorf("invalid job contract: overlap is inconsistent")
	}
	if job.Trailing != nil {
		last := job.Segments[len(job.Segments)-1]
		if !copiedVideo && p.streamType != domain.StreamAudio || !domain.Adjacent(last, *job.Trailing) {
			return fmt.Errorf("invalid job contract: trailing is inconsistent")
		}
	}
	if p.streamType == domain.StreamVideo && job.VideoStreamIndex < 0 {
		return fmt.Errorf("invalid job contract: video stream index is missing")
	}
	if job.Digest == "" || job.Digest != domain.JobDigest(job) {
		return fmt.Errorf("invalid job contract: digest mismatch, the coordinator dropped or altered a job field")
	}
	return nil
}

func validateRendition(job domain.Job) error {
	if video := job.VideoRendition; job.StreamType == domain.StreamVideo {
		if video.Method != domain.DirectStream && video.Method != domain.Transcode {
			return fmt.Errorf("invalid job contract: video method %q is unknown", video.Method)
		}
		return nil
	}
	audio := job.AudioRendition
	if audio.Method != domain.DirectStream && audio.Method != domain.Transcode {
		return fmt.Errorf("invalid job contract: audio method %q is unknown", audio.Method)
	}
	if audio.Method == domain.Transcode && (audio.Channels <= 0 || audio.Bitrate <= 0) {
		return fmt.Errorf("invalid job contract: audio channels or bitrate are missing")
	}
	if audio.Method == domain.DirectStream && audio.Codec == "" {
		return fmt.Errorf("invalid job contract: audio codec is missing")
	}
	var track int
	if matched, _ := fmt.Sscanf(audio.Name, "audio%d_", &track); matched == 1 && track != audio.StreamIndex {
		return fmt.Errorf("invalid job contract: audio stream index is inconsistent")
	}
	return nil
}

func (p *Pool) waitForWorker(ctx context.Context, w *Worker) error {
	select {
	case <-w.Done():
	case <-ctx.Done():
		w.Kill()
		<-w.Done()
	}
	if w.State() == WorkerStateDone {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.Err(); err != nil {
		return err
	}
	return fmt.Errorf("worker failed")
}

func (p *Pool) jobContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.jobTimeout)
}

func (p *Pool) segmentsExist(ctx context.Context, job domain.Job) bool {
	for _, index := range job.ExpectedIndices {
		info := domain.SegmentData{MediaKey: job.MediaKey, Index: index, Rendition: job.Rendition, IsVideo: job.StreamType != domain.StreamAudio}
		exists, err := p.storage.SegmentExists(ctx, info)
		if err != nil || !exists {
			return false
		}
	}
	return true
}

func (p *Pool) rejectDelivery(delivery domain.JobDelivery, reason error, started time.Time) {
	job := delivery.Job
	switch {
	case delivery.Receipt == "":
	case job.ID != "" && job.RunID != "":
		p.settleJob(delivery, domain.JobStateFailed, reason.Error())
	default:
		p.ack(delivery, started)
	}
	p.observe(context.Background(), domain.Event{Type: domain.EventJobDeliveryRejected, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Error: reason.Error(), Duration: time.Since(started)})
}

func (p *Pool) ack(delivery domain.JobDelivery, started time.Time) {
	job := delivery.Job
	ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = p.coordinator.Ack(ackCtx, delivery.Receipt); err == nil {
			return
		}
		select {
		case <-ackCtx.Done():
			attempt = 3
		case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
		}
	}
	p.observe(ackCtx, domain.Event{Type: domain.EventJobAckFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, Error: err.Error(), Duration: time.Since(started)})
}

func (p *Pool) settleJob(delivery domain.JobDelivery, state domain.JobState, message string) error {
	job := delivery.Job
	status := domain.JobStatus{JobID: job.ID, RunID: job.RunID, State: state, Error: message}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = p.coordinator.SettleJob(ctx, delivery.Receipt, status)
		cancel()
		if err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
		}
	}
	p.observe(context.Background(), domain.Event{Type: domain.EventJobSettleFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, Error: err.Error()})
	return err
}

func (p *Pool) completeJob(delivery domain.JobDelivery, started time.Time) bool {
	job := delivery.Job
	if p.settleJob(delivery, domain.JobStateCompleted, "") == nil {
		p.observe(context.Background(), domain.Event{Type: domain.EventJobCompleted, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Duration: time.Since(started)})
		return true
	}
	return false
}

func (p *Pool) failJob(ctx context.Context, delivery domain.JobDelivery, err error, started time.Time) bool {
	job := delivery.Job
	if errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	if p.settleJob(delivery, domain.JobStateFailed, err.Error()) == nil {
		p.observe(context.Background(), domain.Event{Type: domain.EventJobFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Error: err.Error(), Duration: time.Since(started)})
		return true
	}
	return false
}

func (p *Pool) observe(ctx context.Context, event domain.Event) {
	if p.observer == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	p.observer.Observe(ctx, event)
}

func validateChannelJob(job domain.Job) error {
	spec := job.Channel
	if spec == nil || job.VideoRendition == nil || job.AudioRendition == nil {
		return fmt.Errorf("invalid job contract: channel parameters are missing")
	}
	if job.VideoRendition.Name != job.Rendition || job.AudioRendition.Name != job.Rendition {
		return fmt.Errorf("invalid job contract: rendition identity is inconsistent")
	}
	if spec.SourceFrameRate < 0 || math.IsNaN(spec.SourceFrameRate) || math.IsInf(spec.SourceFrameRate, 0) {
		return fmt.Errorf("invalid job contract: channel source frame rate %v is unusable", spec.SourceFrameRate)
	}
	if spec.FormatVersion != domain.ChannelFormatVersion {
		return fmt.Errorf("invalid job contract: channel format version %d is not %d", spec.FormatVersion, domain.ChannelFormatVersion)
	}
	if spec.SegmentsPerJob <= 0 || job.StartIndex%spec.SegmentsPerJob != 0 || len(job.Segments) > spec.SegmentsPerJob {
		return fmt.Errorf("invalid job contract: channel batch is off the grid")
	}
	if job.VideoRendition.Method != domain.Transcode || job.AudioRendition.Method != domain.Transcode {
		return fmt.Errorf("invalid job contract: channel renditions must be transcoded")
	}
	if job.Overlap == nil && job.StartIndex > 0 {
		return fmt.Errorf("invalid job contract: overlap is missing")
	}
	if job.Overlap != nil && !domain.Adjacent(*job.Overlap, job.Segments[0]) {
		return fmt.Errorf("invalid job contract: overlap is inconsistent")
	}
	if job.Trailing != nil && !domain.Adjacent(job.Segments[len(job.Segments)-1], *job.Trailing) {
		return fmt.Errorf("invalid job contract: trailing is inconsistent")
	}
	if job.VideoStreamIndex < 0 {
		return fmt.Errorf("invalid job contract: video stream index is missing")
	}
	if job.Digest == "" || job.Digest != domain.JobDigest(job) {
		return fmt.Errorf("invalid job contract: digest mismatch, the coordinator dropped or altered a job field")
	}
	return nil
}

func (p *Pool) channelParams(job domain.Job, segments []domain.Segment, outputDir string) ffmpeg.ChannelParams {
	video := *job.VideoRendition
	video.FrameRate = job.Channel.SourceFrameRate
	return ffmpeg.ChannelParams{
		InputURL:         job.SourceURL,
		VideoStreamIndex: job.VideoStreamIndex,
		Video:            video,
		Audio:            *job.AudioRendition,
		Spec:             *job.Channel,
		Segments:         segments,
		Overlap:          job.Overlap,
		Trailing:         job.Trailing,
		OutputDir:        outputDir,
	}
}
