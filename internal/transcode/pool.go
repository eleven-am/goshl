package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

const defaultTargetDuration = 6.0

var ErrDeliveryLeaseLost = errors.New("job delivery lease lost")
var ErrJobExpired = errors.New("job deadline expired")

type Pool struct {
	coordinator    domain.Coordinator
	size           int
	streamType     domain.StreamType
	targetDuration float64
	storage        domain.Storage
	cmdBuilder     *ffmpeg.CommandBuilder
	segStorage     domain.Storage
	jobTimeout     time.Duration
	observer       domain.Observer
	profile        string

	runMu   sync.Mutex
	running map[string]context.CancelCauseFunc

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type PoolConfig struct {
	JobTimeout time.Duration
	Observer   domain.Observer
	Profile    string
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
	targetDuration float64,
	storage domain.Storage,
	cmdBuilder *ffmpeg.CommandBuilder,
	segStorage domain.Storage,
	settings ...PoolConfig,
) *Pool {
	if targetDuration <= 0 {
		targetDuration = defaultTargetDuration
	}

	config := PoolConfig{JobTimeout: 2 * time.Minute}
	if len(settings) > 0 {
		config = settings[0]
		if config.JobTimeout <= 0 {
			config.JobTimeout = 2 * time.Minute
		}
	}
	return &Pool{
		coordinator:    coordinator,
		size:           size,
		streamType:     streamType,
		targetDuration: targetDuration,
		storage:        storage,
		cmdBuilder:     cmdBuilder,
		segStorage:     segStorage,
		jobTimeout:     config.JobTimeout,
		observer:       config.Observer,
		profile:        config.Profile,
		running:        make(map[string]context.CancelCauseFunc),
	}
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
	p.cancel = cancel

	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.worker(ctx, jobs)
	}

	return nil
}

func (p *Pool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cancel := p.cancel

	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	p.cancel = nil
}

func (p *Pool) worker(ctx context.Context, jobs <-chan domain.JobDelivery) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case delivery, ok := <-jobs:
			if !ok {
				return
			}
			p.processJob(ctx, delivery)
		}
	}
}

func (p *Pool) processJob(ctx context.Context, delivery domain.JobDelivery) {
	job := delivery.Job
	started := time.Now()
	baseCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	if !p.beginJob(job.RunID, cancelCause) {
		return
	}
	defer p.endJob(job.RunID)
	if err := p.validateDelivery(delivery); err != nil {
		if delivery.Receipt != "" && job.ID != "" && job.RunID != "" {
			p.failJob(context.Background(), delivery, err, started)
		} else if delivery.Receipt != "" {
			p.ack(delivery, started, domain.EventJobFailed)
		}
		return
	}
	if !job.Deadline.After(time.Now()) {
		p.failJob(context.Background(), delivery, ErrJobExpired, started)
		return
	}
	jobCtx, cancel := p.jobContext(baseCtx, job)
	defer cancel()
	leaseTTL := p.leaseTTL(job)
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
	meta, err := p.getMetadata(jobCtx, job.MediaKey)
	if err != nil {
		fail(err)
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

	isVideo := p.streamType == domain.StreamVideo

	var args []string
	var skipFirst bool
	if isVideo {
		videoRendition := job.VideoRendition
		if videoRendition == nil {
			fail(fmt.Errorf("video rendition %s not found", job.Rendition))
			return
		}

		videoSegments := segments
		if job.Overlap != nil {
			videoSegments = append([]domain.Segment{*job.Overlap}, segments...)
			skipFirst = true
		}

		var actualSeekKeyframe float64
		if videoRendition.Method == domain.DirectStream && len(videoSegments) > 0 {
			actualSeekKeyframe, err = p.cmdBuilder.SeekLanding(jobCtx, job.SourceURL, videoSegments[0].Start, meta.Video.Index)
			if err != nil {
				fail(err)
				return
			}
		}

		args = p.cmdBuilder.Video(ffmpeg.VideoParams{
			InputURL:           job.SourceURL,
			StreamIndex:        meta.Video.Index,
			Rendition:          *videoRendition,
			Segments:           videoSegments,
			OutputDir:          tmpDir,
			ActualSeekKeyframe: actualSeekKeyframe,
		})
	} else {
		audioRendition := job.AudioRendition
		if audioRendition == nil {
			fail(fmt.Errorf("audio rendition %s not found", job.Rendition))
			return
		}
		args = p.cmdBuilder.Audio(ffmpeg.AudioParams{
			InputURL:    job.SourceURL,
			StreamIndex: audioRendition.StreamIndex,
			Rendition:   *audioRendition,
			Segments:    segments,
			OutputDir:   tmpDir,
		})
	}

	w := NewWorker(args, p.segStorage, job.MediaKey, job.Rendition, isVideo, tmpDir, skipFirst, job.ExpectedIndices)
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

func (p *Pool) validateDelivery(delivery domain.JobDelivery) error {
	if delivery.Receipt == "" {
		return fmt.Errorf("invalid job contract: delivery receipt is missing")
	}
	return p.validateJob(delivery.Job)
}

func (p *Pool) leaseTTL(job domain.Job) time.Duration {
	ttl := time.Until(job.Deadline) + 15*time.Second
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, stop := context.WithTimeout(ctx, ttl/3)
			err := p.coordinator.Renew(renewCtx, delivery.Receipt, ttl)
			stop()
			if err != nil {
				if !settlement.deferRenewalFailure(err) {
					p.reportLeaseFailure(delivery, err, cancel, started)
				}
				return
			}
		}
	}
}

func (p *Pool) reportLeaseFailure(delivery domain.JobDelivery, err error, cancel context.CancelCauseFunc, started time.Time) {
	job := delivery.Job
	p.observe(context.Background(), domain.Event{Type: domain.EventJobLeaseFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, Error: err.Error(), Duration: time.Since(started)})
	cancel(fmt.Errorf("%w: %v", ErrDeliveryLeaseLost, err))
}

func (p *Pool) validateJob(job domain.Job) error {
	if job.ID == "" || job.RunID == "" || job.MediaKey == "" || job.Profile == "" || job.SourceURL == "" || job.Rendition == "" || job.EnqueuedAt.IsZero() || job.Deadline.IsZero() {
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
		if segment.Index != job.StartIndex+index || job.ExpectedIndices[index] != segment.Index || segment.Start < 0 || segment.End <= segment.Start || segment.Duration <= 0 || math.Abs(segment.Duration-(segment.End-segment.Start)) > 0.000001 {
			return fmt.Errorf("invalid job contract: segment %d is inconsistent", segment.Index)
		}
	}
	if p.streamType == domain.StreamVideo && job.VideoRendition == nil || p.streamType == domain.StreamAudio && job.AudioRendition == nil {
		return fmt.Errorf("invalid job contract: rendition parameters are missing")
	}
	if job.VideoRendition != nil && job.VideoRendition.Name != job.Rendition || job.AudioRendition != nil && job.AudioRendition.Name != job.Rendition {
		return fmt.Errorf("invalid job contract: rendition identity is inconsistent")
	}
	if job.Overlap != nil && (p.streamType != domain.StreamVideo || job.Overlap.Index != job.StartIndex-1 || math.Abs(job.Overlap.End-job.Segments[0].Start) > 0.000001) {
		return fmt.Errorf("invalid job contract: overlap is inconsistent")
	}
	return nil
}

func (p *Pool) getMetadata(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	data, err := p.storage.GetMetadata(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	var meta domain.Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (p *Pool) waitForWorker(ctx context.Context, w *Worker) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		switch w.State() {
		case WorkerStateDone:
			return nil
		case WorkerStateError:
			if err := w.Err(); err != nil {
				return err
			}
			return fmt.Errorf("worker failed")
		}

		select {
		case <-ctx.Done():
			w.Kill()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Pool) extractSegments(keyframes []float64, duration float64, startIdx, endIdx int) []domain.Segment {
	var segments []domain.Segment

	for _, segment := range playlist.CalculateSegments(keyframes, duration, p.effectiveTargetDuration()) {
		if segment.Index >= startIdx && segment.Index <= endIdx {
			segments = append(segments, segment)
		}
		if segment.Index > endIdx {
			break
		}
	}

	return segments
}

func (p *Pool) effectiveTargetDuration() float64 {
	if p.targetDuration > 0 {
		return p.targetDuration
	}
	return defaultTargetDuration
}

func (p *Pool) findVideoRendition(meta *domain.Metadata, name string) *domain.VideoRendition {
	renditions := rendition.GenerateVideo(meta.Video)
	for _, r := range renditions {
		if r.Name == name {
			return &r
		}
	}
	return nil
}

func (p *Pool) findAudioRendition(meta *domain.Metadata, name string) *domain.AudioRendition {
	if len(meta.Audios) == 0 {
		return nil
	}

	renditions := rendition.GenerateAudioTracks(meta.Audios)
	for _, r := range renditions {
		if r.Name == name {
			return &r
		}
	}
	return nil
}

func (p *Pool) jobContext(ctx context.Context, job domain.Job) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(p.jobTimeout)
	if !job.Deadline.IsZero() && job.Deadline.Before(deadline) {
		deadline = job.Deadline
	}
	return context.WithDeadline(ctx, deadline)
}

func (p *Pool) segmentsExist(ctx context.Context, job domain.Job) bool {
	for _, index := range job.ExpectedIndices {
		info := domain.SegmentData{MediaKey: job.MediaKey, Index: index, Rendition: job.Rendition, IsVideo: job.StreamType == domain.StreamVideo}
		exists, err := p.storage.SegmentExists(ctx, info)
		if err != nil || !exists {
			return false
		}
	}
	return true
}

func (p *Pool) ack(delivery domain.JobDelivery, started time.Time, eventType domain.EventType) {
	job := delivery.Job
	ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = p.coordinator.Ack(ackCtx, delivery.Receipt); err == nil {
			p.observe(ackCtx, domain.Event{Type: eventType, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Duration: time.Since(started)})
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
	cause := context.Cause(ctx)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, ErrDeliveryLeaseLost) {
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
