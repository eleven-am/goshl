package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
)

const (
	maxFlightReentries  = 5
	pollInitialInterval = 100 * time.Millisecond
	pollMaxInterval     = time.Second
)

var errFlightAbandoned = errors.New("segment flight abandoned before enqueue")

func (e *Engine) Segment(ctx context.Context, sourceURL string, streamType domain.StreamType, renditionName string, index int) ([]byte, error) {
	if err := e.ensureRunning(); err != nil {
		return nil, err
	}
	if streamType != domain.StreamVideo && streamType != domain.StreamAudio {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStreamType, streamType)
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, index)
	}
	if !rendition.ValidName(renditionName) || rendition.IsChannelName(renditionName) {
		return nil, fmt.Errorf("%w: %q", ErrRenditionNotFound, renditionName)
	}
	for attempt := 0; attempt < maxFlightReentries; attempt++ {
		data, err := e.segmentOnce(ctx, sourceURL, streamType, renditionName, index)
		if !errors.Is(err, errFlightAbandoned) {
			return data, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	return nil, fmt.Errorf("%w: segment %d", ErrSegmentTimeout, index)
}

type segmentRequest struct {
	sourceURL  string
	mediaKey   string
	streamType domain.StreamType
	rendition  string
	index      int
	priority   domain.JobPriority
	plan       func(context.Context) (*flightPlan, error)
}

type flightPlan struct {
	segments         []domain.Segment
	start            int
	end              int
	video            *domain.VideoRendition
	audio            *domain.AudioRendition
	channel          *domain.ChannelSpec
	videoStreamIndex int
	neighbours       bool
}

type launchOutcome struct {
	held        bool
	activeRunID string
	stored      bool
}

func (r segmentRequest) info() domain.SegmentData {
	return domain.SegmentData{
		MediaKey:  r.mediaKey,
		Index:     r.index,
		Rendition: r.rendition,
		IsVideo:   r.streamType != domain.StreamAudio,
	}
}

func (e *Engine) segmentOnce(ctx context.Context, sourceURL string, streamType domain.StreamType, renditionName string, index int) ([]byte, error) {
	mediaKey, err := e.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	request := segmentRequest{
		sourceURL:  sourceURL,
		mediaKey:   mediaKey,
		streamType: streamType,
		rendition:  renditionName,
		index:      index,
		priority:   domain.PriorityPlayback,
	}
	request.plan = func(ctx context.Context) (*flightPlan, error) {
		return e.playbackPlan(ctx, request)
	}
	return e.requestSegment(ctx, request)
}

func (e *Engine) playbackPlan(ctx context.Context, request segmentRequest) (*flightPlan, error) {
	info := request.info()
	meta, err := e.getMetadataWithKey(ctx, request.sourceURL, request.mediaKey)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}
	videoRendition, audioRendition, err := e.resolveRendition(meta, request.streamType, request.rendition)
	if err != nil {
		return nil, err
	}
	allSegments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, e.opts.TargetDuration)
	if request.index >= len(allSegments) {
		return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, request.index)
	}
	endIdx := request.index + e.opts.SegmentsPerJob - 1
	if endIdx >= len(allSegments) {
		endIdx = len(allSegments) - 1
	}
	for next := request.index + 1; next <= endIdx; next++ {
		nextInfo := info
		nextInfo.Index = next
		nextExists, err := e.opts.Storage.SegmentExists(ctx, nextInfo)
		if err != nil {
			return nil, fmt.Errorf("check segment: %w", err)
		}
		if nextExists {
			endIdx = next - 1
			break
		}
	}
	copiedVideo := request.streamType == domain.StreamVideo && videoRendition != nil && videoRendition.Method == domain.DirectStream
	return &flightPlan{
		segments:         allSegments,
		start:            request.index,
		end:              endIdx,
		video:            videoRendition,
		audio:            audioRendition,
		videoStreamIndex: meta.Video.Index,
		neighbours:       copiedVideo || request.streamType == domain.StreamAudio,
	}, nil
}

func (e *Engine) requestSegment(ctx context.Context, request segmentRequest) ([]byte, error) {
	mediaKey, streamType, renditionName, index := request.mediaKey, request.streamType, request.rendition, request.index
	info := request.info()

	exists, err := e.opts.Storage.SegmentExists(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("check segment: %w", err)
	}

	if exists {
		e.observe(ctx, domain.Event{Type: domain.EventSegmentCacheHit, MediaKey: mediaKey, StreamType: streamType, Rendition: renditionName, Index: index})
		return e.opts.Storage.ReadSegment(ctx, info)
	}
	e.observe(ctx, domain.Event{Type: domain.EventSegmentCacheMiss, MediaKey: mediaKey, StreamType: streamType, Rendition: renditionName, Index: index})

	plan, err := request.plan(ctx)
	if err != nil {
		return nil, err
	}

	requested := time.Now()
	existing, created := e.joinFlight(mediaKey, streamType, renditionName, plan.start, plan.end)
	if existing != nil {
		waitCtx, stopWaiting := context.WithCancel(ctx)
		defer stopWaiting()
		statusCh, err := e.opts.Coordinator.WaitSegment(waitCtx, info)
		if err != nil {
			return nil, fmt.Errorf("wait segment: %w", err)
		}
		e.observe(ctx, domain.Event{Type: domain.EventJobJoined, MediaKey: mediaKey, JobID: existing.jobID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: existing.start, EndIndex: existing.end})
		if data, found, err := e.readStoredSegment(ctx, info); err != nil || found {
			return data, err
		}
		return e.waitForSegment(ctx, info, statusCh, existing, nil, existing.runID, requested, nil)
	}
	startIdx, endIdx, jobID := created.start, created.end, created.jobID

	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	var statusCh <-chan domain.SegmentStatus
	subscribe := func() error {
		var err error
		statusCh, err = e.opts.Coordinator.WaitSegment(waitCtx, info)
		if err != nil {
			return fmt.Errorf("wait segment: %w", err)
		}
		return nil
	}
	outcome, err := e.launchFlight(ctx, request, plan, created, subscribe)
	if err != nil {
		return nil, err
	}
	if outcome.stored {
		return e.opts.Storage.ReadSegment(ctx, info)
	}
	if outcome.held {
		defer e.abandonFlight(created)
		if err := subscribe(); err != nil {
			return nil, err
		}
		e.observe(ctx, domain.Event{Type: domain.EventLockWait, MediaKey: mediaKey, JobID: jobID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: startIdx, EndIndex: endIdx})
		if data, found, err := e.readStoredSegment(ctx, info); err != nil || found {
			return data, err
		}
		if outcome.activeRunID == "" {
			return nil, fmt.Errorf("active run missing for locked job %s", jobID)
		}
		jobWaitCtx, stopJobWaiting := context.WithCancel(ctx)
		defer stopJobWaiting()
		jobStatusCh, err := e.opts.Coordinator.WaitJob(jobWaitCtx, outcome.activeRunID)
		if err != nil {
			return nil, fmt.Errorf("wait active job: %w", err)
		}
		lockFree := func() bool { return e.jobLockFree(jobID) }
		return e.waitForSegment(ctx, info, statusCh, created, jobStatusCh, outcome.activeRunID, requested, lockFree)
	}
	return e.waitForSegment(ctx, info, statusCh, created, nil, created.runID, requested, nil)
}

func (e *Engine) launchFlight(ctx context.Context, request segmentRequest, plan *flightPlan, created *flight, subscribe func() error) (launchOutcome, error) {
	info := request.info()
	lockTTL := time.Until(created.deadline) + time.Minute
	setupCtx, stopSetup := e.flightSetupContext(ctx)
	defer stopSetup()
	token, activeRunID, lockErr := e.opts.Coordinator.AcquireJob(setupCtx, created.jobID, created.runID, lockTTL)
	if lockErr != nil {
		if !errors.Is(lockErr, domain.ErrLockHeld) {
			e.abandonFlight(created)
			return launchOutcome{}, fmt.Errorf("acquire segment lock: %w", lockErr)
		}
		return launchOutcome{held: true, activeRunID: activeRunID}, nil
	}
	if !e.setFlightLock(created, token) {
		e.releaseFlightLocks([]*flight{created})
		return launchOutcome{}, e.flightGoneError(info)
	}

	exists, err := e.opts.Storage.SegmentExists(ctx, info)
	if err != nil {
		e.abandonFlight(created)
		return launchOutcome{}, fmt.Errorf("check segment: %w", err)
	}
	if exists {
		e.abandonFlight(created)
		return launchOutcome{stored: true}, nil
	}

	if subscribe != nil {
		if err := subscribe(); err != nil {
			e.abandonFlight(created)
			return launchOutcome{}, err
		}
	}
	jobWatchCtx, jobWatchCancel := context.WithCancel(e.lifecycle.Context())
	jobStatusCh, err := e.opts.Coordinator.WaitJob(jobWatchCtx, created.runID)
	if err != nil {
		jobWatchCancel()
		e.abandonFlight(created)
		return launchOutcome{}, fmt.Errorf("wait job: %w", err)
	}
	if !e.setFlightWatch(created, jobWatchCancel) {
		return launchOutcome{}, e.flightGoneError(info)
	}

	job := e.assembleJob(created, request, plan)
	if !e.startFlightWatch(created, jobStatusCh) {
		return launchOutcome{}, ErrControllerStopped
	}
	if err := ctx.Err(); err != nil {
		e.abandonFlight(created)
		return launchOutcome{}, fmt.Errorf("enqueue: %w", err)
	}
	if !e.markEnqueued(created) {
		return launchOutcome{}, e.flightGoneError(info)
	}
	if err := e.opts.Coordinator.Enqueue(setupCtx, job); err != nil {
		if setupCtx.Err() != nil || errors.Is(err, domain.ErrEnqueueRejected) {
			e.abandonFlight(created)
		}
		return launchOutcome{}, fmt.Errorf("enqueue: %w", err)
	}
	e.observe(ctx, domain.Event{Type: domain.EventJobEnqueued, MediaKey: request.mediaKey, JobID: job.ID, StreamType: request.streamType, Rendition: request.rendition, Index: request.index, StartIndex: created.start, EndIndex: created.end})
	return launchOutcome{}, nil
}

func (e *Engine) flightGoneError(info domain.SegmentData) error {
	if e.lifecycle.Stopped() {
		return ErrControllerStopped
	}
	return fmt.Errorf("%w: segment %d", ErrSegmentTimeout, info.Index)
}

func (e *Engine) flightSetupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	setupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.opts.SegmentTimeout)
	stop := context.AfterFunc(e.lifecycle.Context(), cancel)
	return setupCtx, func() {
		stop()
		cancel()
	}
}

func (e *Engine) waitForSegment(ctx context.Context, info domain.SegmentData, statusCh <-chan domain.SegmentStatus, f *flight, jobStatusCh <-chan domain.JobStatus, runID string, requested time.Time, lockFree func() bool) ([]byte, error) {
	timer := time.NewTimer(e.opts.SegmentTimeout)
	defer timer.Stop()
	pollBackoff := backoff.NewExponentialBackOff()
	pollBackoff.InitialInterval = pollInitialInterval
	pollBackoff.MaxInterval = pollMaxInterval
	pollBackoff.RandomizationFactor = 0
	pollBackoff.Reset()
	poll := time.NewTimer(pollBackoff.NextBackOff())
	defer poll.Stop()
	var terminal <-chan struct{}
	if f != nil {
		terminal = f.done
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.lifecycle.Context().Done():
			return nil, ErrControllerStopped
		case <-timer.C:
			if data, found, err := e.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
			return nil, fmt.Errorf("%w: segment %d", ErrSegmentTimeout, info.Index)
		case <-poll.C:
			if data, found, err := e.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
			if lockFree != nil && lockFree() {
				return nil, errFlightAbandoned
			}
			poll.Reset(pollBackoff.NextBackOff())
		case <-terminal:
			if e.lifecycle.Stopped() {
				return nil, ErrControllerStopped
			}
			if data, found, err := e.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
			status, found := e.flightTerminal(f)
			if abandoned, expired := e.flightEnded(f); !found && abandoned {
				return nil, errFlightAbandoned
			} else if !found && expired {
				return nil, fmt.Errorf("%w: segment %d", ErrSegmentTimeout, info.Index)
			}
			if found && status.State == domain.JobStateFailed {
				return nil, fmt.Errorf("segment error: %s", status.Error)
			}
			if found && status.State == domain.JobStateCancelled {
				return nil, fmt.Errorf("segment job cancelled: %s", status.Error)
			}
			return nil, fmt.Errorf("segment %d was not produced", info.Index)
		case status, ok := <-jobStatusCh:
			if !ok {
				jobStatusCh = nil
				continue
			}
			if status.RunID != runID {
				continue
			}
			switch status.State {
			case domain.JobStateFailed:
				return nil, fmt.Errorf("segment error: %s", status.Error)
			case domain.JobStateCancelled:
				return nil, fmt.Errorf("segment job cancelled: %s", status.Error)
			case domain.JobStateCompleted:
				if data, found, err := e.readStoredSegment(ctx, info); err != nil || found {
					return data, err
				}
				return nil, fmt.Errorf("segment %d was not produced", info.Index)
			}
		case status, ok := <-statusCh:
			if !ok {
				if data, found, err := e.readStoredSegment(ctx, info); err != nil || found {
					return data, err
				}
				statusCh = nil
				continue
			}
			if status.State != domain.SegmentStateReady {
				continue
			}
			data, err := e.opts.Storage.ReadSegment(ctx, info)
			if err == nil {
				e.observe(ctx, domain.Event{Type: domain.EventSegmentReady, MediaKey: info.MediaKey, Rendition: info.Rendition, Index: info.Index, Bytes: int64(len(data)), Duration: time.Since(requested)})
			}
			return data, err
		}
	}
}

func (e *Engine) readStoredSegment(ctx context.Context, info domain.SegmentData) ([]byte, bool, error) {
	exists, err := e.opts.Storage.SegmentExists(ctx, info)
	if err != nil || !exists {
		return nil, false, err
	}
	data, err := e.opts.Storage.ReadSegment(ctx, info)
	return data, true, err
}

func (e *Engine) resolveRendition(meta *domain.Metadata, streamType domain.StreamType, name string) (*domain.VideoRendition, *domain.AudioRendition, error) {
	switch streamType {
	case domain.StreamVideo:
		videos, _, err := rendition.GenerateVideo(meta, e.opts.TargetDuration)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %v", ErrRenditionNotFound, name, err)
		}
		for _, candidate := range videos {
			if candidate.Name == name {
				value := candidate
				return &value, nil, nil
			}
		}
	case domain.StreamAudio:
		for _, candidate := range rendition.GenerateAudioTracks(meta.Audios) {
			if candidate.Name == name {
				value := candidate
				return nil, &value, nil
			}
		}
	default:
		return nil, nil, fmt.Errorf("%w: %q", ErrInvalidStreamType, streamType)
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrRenditionNotFound, name)
}

func (e *Engine) buildJob(runID string, startBy time.Time, sourceURL, mediaKey string, streamType domain.StreamType, renditionName string, index, startIdx, endIdx int, allSegments []domain.Segment, video *domain.VideoRendition, audio *domain.AudioRendition, meta *domain.Metadata) domain.Job {
	copiedVideo := streamType == domain.StreamVideo && video != nil && video.Method == domain.DirectStream
	request := segmentRequest{sourceURL: sourceURL, mediaKey: mediaKey, streamType: streamType, rendition: renditionName, index: index, priority: domain.PriorityPlayback}
	plan := &flightPlan{segments: allSegments, video: video, audio: audio, videoStreamIndex: meta.Video.Index, neighbours: copiedVideo || streamType == domain.StreamAudio}
	return e.assembleJob(&flight{runID: runID, startBy: startBy, start: startIdx, end: endIdx}, request, plan)
}

func (e *Engine) assembleJob(f *flight, request segmentRequest, plan *flightPlan) domain.Job {
	startIdx, endIdx := f.start, f.end
	segments := append([]domain.Segment(nil), plan.segments[startIdx:endIdx+1]...)
	expected := make([]int, len(segments))
	for i, segment := range segments {
		expected[i] = segment.Index
	}
	var overlap, trailing *domain.Segment
	if startIdx > 0 && plan.neighbours {
		value := plan.segments[startIdx-1]
		overlap = &value
	}
	if plan.neighbours && endIdx+1 < len(plan.segments) {
		value := plan.segments[endIdx+1]
		trailing = &value
	}
	job := domain.Job{
		ID:               deterministicJobID(request.mediaKey, request.rendition, request.streamType, startIdx, endIdx),
		RunID:            f.runID,
		MediaKey:         request.mediaKey,
		Profile:          e.routeProfile,
		SourceURL:        request.sourceURL,
		Rendition:        request.rendition,
		StreamType:       request.streamType,
		StartIndex:       startIdx,
		EndIndex:         endIdx,
		RequestedIndex:   request.index,
		EnqueuedAt:       time.Now(),
		StartBy:          f.startBy,
		Segments:         segments,
		Overlap:          overlap,
		Trailing:         trailing,
		ExpectedIndices:  expected,
		VideoRendition:   plan.video,
		AudioRendition:   plan.audio,
		VideoStreamIndex: plan.videoStreamIndex,
		Priority:         request.priority,
		Channel:          plan.channel,
	}
	job.Digest = domain.JobDigest(job)
	return job
}

func deterministicJobID(sourceURL, rendition string, streamType domain.StreamType, startIdx, endIdx int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s:%s:%d:%d", sourceURL, rendition, streamType, startIdx, endIdx)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
