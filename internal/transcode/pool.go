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

var ErrJobAbandoned = errors.New("job abandoned: no remaining waiters")

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
	onJobDone      func(jobID string)

	runMu     sync.Mutex
	running   map[string]context.CancelCauseFunc
	abandoned map[string]struct{}

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type PoolConfig struct {
	JobTimeout time.Duration
	Observer   domain.Observer
	Profile    string
	OnJobDone  func(jobID string)
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
		onJobDone:      config.OnJobDone,
		running:        make(map[string]context.CancelCauseFunc),
		abandoned:      make(map[string]struct{}),
	}
}

func (p *Pool) CancelJob(jobID string) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if cancel, ok := p.running[jobID]; ok {
		cancel(ErrJobAbandoned)
		return
	}
	p.abandoned[jobID] = struct{}{}
}

func (p *Pool) ReviveJob(jobID string) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	delete(p.abandoned, jobID)
}

func (p *Pool) beginJob(jobID string, cancel context.CancelCauseFunc) (started bool, abandoned bool) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if _, ok := p.running[jobID]; ok {
		return false, false
	}
	p.running[jobID] = cancel
	if _, ok := p.abandoned[jobID]; ok {
		delete(p.abandoned, jobID)
		return true, true
	}
	return true, false
}

func (p *Pool) endJob(jobID string) {
	p.runMu.Lock()
	delete(p.running, jobID)
	p.runMu.Unlock()
	if p.onJobDone != nil {
		p.onJobDone(jobID)
	}
}

func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return fmt.Errorf("pool already started")
	}
	ctx, cancel := context.WithCancel(ctx)

	jobs, err := p.coordinator.Subscribe(ctx, p.streamType)
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

func (p *Pool) worker(ctx context.Context, jobs <-chan domain.Job) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			p.processJob(ctx, job)
		}
	}
}

func (p *Pool) processJob(ctx context.Context, job domain.Job) {
	started := time.Now()
	baseCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	begun, abandoned := p.beginJob(job.ID, cancelCause)
	if !begun {
		return
	}
	defer p.endJob(job.ID)
	if abandoned {
		return
	}
	jobCtx, cancel := p.jobContext(baseCtx, job)
	defer cancel()
	if err := p.validateJob(job); err != nil {
		p.failJob(jobCtx, job, err, started)
		return
	}
	p.observe(jobCtx, domain.Event{Type: domain.EventJobStarted, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Duration: time.Since(job.EnqueuedAt)})
	if p.segmentsExist(jobCtx, job) {
		p.ack(job, started)
		return
	}
	meta, err := p.getMetadata(jobCtx, job.MediaKey)
	if err != nil {
		p.failJob(jobCtx, job, err, started)
		return
	}

	segments := job.Segments
	if len(segments) == 0 {
		p.failJob(jobCtx, job, fmt.Errorf("no segments for range %d-%d", job.StartIndex, job.EndIndex), started)
		return
	}

	tmpDir, err := os.MkdirTemp("", "transcode-*")
	if err != nil {
		p.failJob(jobCtx, job, fmt.Errorf("create temp dir: %w", err), started)
		return
	}
	defer os.RemoveAll(tmpDir)

	isVideo := p.streamType == domain.StreamVideo

	var args []string
	var skipFirst bool
	if isVideo {
		videoRendition := job.VideoRendition
		if videoRendition == nil {
			p.failJob(jobCtx, job, fmt.Errorf("video rendition %s not found", job.Rendition), started)
			return
		}

		videoSegments := segments
		if job.Overlap != nil {
			videoSegments = append([]domain.Segment{*job.Overlap}, segments...)
			skipFirst = true
		}

		var actualSeekKeyframe float64
		if videoRendition.Method == domain.DirectStream && len(videoSegments) > 0 {
			actualSeekKeyframe = findNearestKeyframe(meta.Keyframes, videoSegments[0].Start)
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
			p.failJob(jobCtx, job, fmt.Errorf("audio rendition %s not found", job.Rendition), started)
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
		p.failJob(jobCtx, job, err, started)
		return
	}

	if err := p.waitForWorker(jobCtx, w); err != nil {
		p.failJob(jobCtx, job, err, started)
		return
	}

	p.ack(job, started)
}

func (p *Pool) validateJob(job domain.Job) error {
	if job.ID == "" || job.MediaKey == "" || job.Profile == "" || job.SourceURL == "" || job.Rendition == "" || job.EnqueuedAt.IsZero() || job.Deadline.IsZero() {
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

func (p *Pool) publishError(ctx context.Context, job domain.Job, err error) {
	notifyCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		notifyCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	for i := job.StartIndex; i <= job.EndIndex; i++ {
		info := domain.SegmentData{
			MediaKey:  job.MediaKey,
			Index:     i,
			Rendition: job.Rendition,
			IsVideo:   p.streamType == domain.StreamVideo,
		}
		status := domain.SegmentStatus{
			State: domain.SegmentStateError,
			Error: err.Error(),
		}

		if notifyErr := p.coordinator.NotifySegment(notifyCtx, info, status); notifyErr != nil {
			p.observe(notifyCtx, domain.Event{Type: domain.EventSegmentNotifyFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, Index: i, Error: notifyErr.Error()})
		}
	}
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

func (p *Pool) ack(job domain.Job, started time.Time) {
	ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = p.coordinator.Ack(ackCtx, job.ID); err == nil {
			p.observe(ackCtx, domain.Event{Type: domain.EventJobCompleted, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Duration: time.Since(started)})
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

func (p *Pool) failJob(ctx context.Context, job domain.Job, err error, started time.Time) {
	if !errors.Is(context.Cause(ctx), ErrJobAbandoned) {
		p.publishError(ctx, job, err)
	}
	p.observe(ctx, domain.Event{Type: domain.EventJobFailed, MediaKey: job.MediaKey, JobID: job.ID, StreamType: job.StreamType, Rendition: job.Rendition, StartIndex: job.StartIndex, EndIndex: job.EndIndex, Error: err.Error(), Duration: time.Since(started)})
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

func findNearestKeyframe(keyframes []float64, target float64) float64 {
	if len(keyframes) == 0 {
		return 0
	}

	var prevKeyframe float64
	var result float64
	for _, kf := range keyframes {
		if kf <= target+0.001 {
			prevKeyframe = result
			result = kf
		} else {
			break
		}
	}

	if prevKeyframe > 0 && target-result < 0.01 {
		return prevKeyframe
	}
	return result
}
