package goshl

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

func (c *Controller) Segment(ctx context.Context, sourceURL string, streamType StreamType, renditionName string, index int) ([]byte, error) {
	if err := c.ensureRunning(); err != nil {
		return nil, err
	}
	if streamType != domain.StreamVideo && streamType != domain.StreamAudio {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStreamType, streamType)
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, index)
	}
	if !rendition.ValidName(renditionName) {
		return nil, fmt.Errorf("%w: %q", ErrRenditionNotFound, renditionName)
	}
	for attempt := 0; attempt < maxFlightReentries; attempt++ {
		data, err := c.segmentOnce(ctx, sourceURL, streamType, renditionName, index)
		if !errors.Is(err, errFlightAbandoned) {
			return data, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	return nil, fmt.Errorf("%w: segment %d", ErrSegmentTimeout, index)
}

func (c *Controller) segmentOnce(ctx context.Context, sourceURL string, streamType StreamType, renditionName string, index int) ([]byte, error) {
	mediaKey, err := c.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	info := domain.SegmentData{
		MediaKey:  mediaKey,
		Index:     index,
		Rendition: renditionName,
		IsVideo:   streamType == domain.StreamVideo,
	}

	exists, err := c.opts.Storage.SegmentExists(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("check segment: %w", err)
	}

	if exists {
		c.observe(ctx, domain.Event{Type: domain.EventSegmentCacheHit, MediaKey: mediaKey, StreamType: streamType, Rendition: renditionName, Index: index})
		return c.opts.Storage.ReadSegment(ctx, info)
	}
	c.observe(ctx, domain.Event{Type: domain.EventSegmentCacheMiss, MediaKey: mediaKey, StreamType: streamType, Rendition: renditionName, Index: index})

	meta, err := c.getMetadataWithKey(ctx, sourceURL, mediaKey)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}
	videoRendition, audioRendition, err := c.resolveRendition(meta, streamType, renditionName)
	if err != nil {
		return nil, err
	}
	allSegments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, c.opts.TargetDuration)
	if index >= len(allSegments) {
		return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, index)
	}
	endIdx := index + c.opts.SegmentsPerJob - 1
	if endIdx >= len(allSegments) {
		endIdx = len(allSegments) - 1
	}
	for next := index + 1; next <= endIdx; next++ {
		nextInfo := info
		nextInfo.Index = next
		nextExists, err := c.opts.Storage.SegmentExists(ctx, nextInfo)
		if err != nil {
			return nil, fmt.Errorf("check segment: %w", err)
		}
		if nextExists {
			endIdx = next - 1
			break
		}
	}

	requested := time.Now()
	existing, created := c.joinFlight(mediaKey, streamType, renditionName, index, endIdx)
	if existing != nil {
		waitCtx, stopWaiting := context.WithCancel(ctx)
		defer stopWaiting()
		statusCh, err := c.opts.Coordinator.WaitSegment(waitCtx, info)
		if err != nil {
			return nil, fmt.Errorf("wait segment: %w", err)
		}
		c.observe(ctx, domain.Event{Type: domain.EventJobJoined, MediaKey: mediaKey, JobID: existing.jobID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: existing.start, EndIndex: existing.end})
		if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
			return data, err
		}
		return c.waitForSegment(ctx, info, statusCh, existing, nil, existing.runID, requested, nil)
	}
	startIdx, endIdx, jobID := created.start, created.end, created.jobID
	lockTTL := time.Until(created.deadline) + time.Minute
	setupCtx, stopSetup := c.flightSetupContext(ctx)
	defer stopSetup()
	token, activeRunID, lockErr := c.opts.Coordinator.AcquireJob(setupCtx, jobID, created.runID, lockTTL)

	if lockErr != nil {
		if !errors.Is(lockErr, domain.ErrLockHeld) {
			c.abandonFlight(created)
			return nil, fmt.Errorf("acquire segment lock: %w", lockErr)
		}
		defer c.abandonFlight(created)
		waitCtx, stopWaiting := context.WithCancel(ctx)
		defer stopWaiting()
		statusCh, err := c.opts.Coordinator.WaitSegment(waitCtx, info)
		if err != nil {
			return nil, fmt.Errorf("wait segment: %w", err)
		}
		c.observe(ctx, domain.Event{Type: domain.EventLockWait, MediaKey: mediaKey, JobID: jobID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: startIdx, EndIndex: endIdx})
		if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
			return data, err
		}
		if activeRunID == "" {
			return nil, fmt.Errorf("active run missing for locked job %s", jobID)
		}
		jobWaitCtx, stopJobWaiting := context.WithCancel(ctx)
		defer stopJobWaiting()
		jobStatusCh, err := c.opts.Coordinator.WaitJob(jobWaitCtx, activeRunID)
		if err != nil {
			return nil, fmt.Errorf("wait active job: %w", err)
		}
		lockFree := func() bool { return c.jobLockFree(jobID) }
		return c.waitForSegment(ctx, info, statusCh, created, jobStatusCh, activeRunID, requested, lockFree)
	}
	if !c.setFlightLock(created, token) {
		c.releaseFlightLocks([]*flight{created})
		return nil, c.flightGoneError(info)
	}

	exists, err = c.opts.Storage.SegmentExists(ctx, info)
	if err != nil {
		c.abandonFlight(created)
		return nil, fmt.Errorf("check segment: %w", err)
	}
	if exists {
		c.abandonFlight(created)
		return c.opts.Storage.ReadSegment(ctx, info)
	}

	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	statusCh, err := c.opts.Coordinator.WaitSegment(waitCtx, info)
	if err != nil {
		c.abandonFlight(created)
		return nil, fmt.Errorf("wait segment: %w", err)
	}
	jobWatchCtx, jobWatchCancel := context.WithCancel(c.lifecycle.Context())
	jobStatusCh, err := c.opts.Coordinator.WaitJob(jobWatchCtx, created.runID)
	if err != nil {
		jobWatchCancel()
		c.abandonFlight(created)
		return nil, fmt.Errorf("wait job: %w", err)
	}
	if !c.setFlightWatch(created, jobWatchCancel) {
		return nil, c.flightGoneError(info)
	}

	job := c.buildJob(created.runID, created.startBy, sourceURL, mediaKey, streamType, renditionName, index, startIdx, endIdx, allSegments, videoRendition, audioRendition, meta)
	if !c.startFlightWatch(created, jobStatusCh) {
		return nil, ErrControllerStopped
	}
	if err := ctx.Err(); err != nil {
		c.abandonFlight(created)
		return nil, fmt.Errorf("enqueue: %w", err)
	}
	if !c.markEnqueued(created) {
		return nil, c.flightGoneError(info)
	}
	if err := c.opts.Coordinator.Enqueue(setupCtx, job); err != nil {
		if setupCtx.Err() != nil || errors.Is(err, domain.ErrEnqueueRejected) {
			c.abandonFlight(created)
		}
		return nil, fmt.Errorf("enqueue: %w", err)
	}
	c.observe(ctx, domain.Event{Type: domain.EventJobEnqueued, MediaKey: mediaKey, JobID: job.ID, StreamType: streamType, Rendition: renditionName, Index: index, StartIndex: startIdx, EndIndex: endIdx})

	return c.waitForSegment(ctx, info, statusCh, created, nil, created.runID, requested, nil)
}

func (c *Controller) flightGoneError(info domain.SegmentData) error {
	if c.lifecycle.Stopped() {
		return ErrControllerStopped
	}
	return fmt.Errorf("%w: segment %d", ErrSegmentTimeout, info.Index)
}

func (c *Controller) flightSetupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	setupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.opts.SegmentTimeout)
	stop := context.AfterFunc(c.lifecycle.Context(), cancel)
	return setupCtx, func() {
		stop()
		cancel()
	}
}

func (c *Controller) waitForSegment(ctx context.Context, info domain.SegmentData, statusCh <-chan domain.SegmentStatus, f *flight, jobStatusCh <-chan domain.JobStatus, runID string, requested time.Time, lockFree func() bool) ([]byte, error) {
	timer := time.NewTimer(c.opts.SegmentTimeout)
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
		case <-c.lifecycle.Context().Done():
			return nil, ErrControllerStopped
		case <-timer.C:
			if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
			return nil, fmt.Errorf("%w: segment %d", ErrSegmentTimeout, info.Index)
		case <-poll.C:
			if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
			if lockFree != nil && lockFree() {
				return nil, errFlightAbandoned
			}
			poll.Reset(pollBackoff.NextBackOff())
		case <-terminal:
			if c.lifecycle.Stopped() {
				return nil, ErrControllerStopped
			}
			if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
				return data, err
			}
			status, found := c.flightTerminal(f)
			if abandoned, expired := c.flightEnded(f); !found && abandoned {
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
				if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
					return data, err
				}
				return nil, fmt.Errorf("segment %d was not produced", info.Index)
			}
		case status, ok := <-statusCh:
			if !ok {
				if data, found, err := c.readStoredSegment(ctx, info); err != nil || found {
					return data, err
				}
				statusCh = nil
				continue
			}
			if status.State != domain.SegmentStateReady {
				continue
			}
			data, err := c.opts.Storage.ReadSegment(ctx, info)
			if err == nil {
				c.observe(ctx, domain.Event{Type: domain.EventSegmentReady, MediaKey: info.MediaKey, Rendition: info.Rendition, Index: info.Index, Bytes: int64(len(data)), Duration: time.Since(requested)})
			}
			return data, err
		}
	}
}

func (c *Controller) readStoredSegment(ctx context.Context, info domain.SegmentData) ([]byte, bool, error) {
	exists, err := c.opts.Storage.SegmentExists(ctx, info)
	if err != nil || !exists {
		return nil, false, err
	}
	data, err := c.opts.Storage.ReadSegment(ctx, info)
	return data, true, err
}

func (c *Controller) resolveRendition(meta *domain.Metadata, streamType domain.StreamType, name string) (*domain.VideoRendition, *domain.AudioRendition, error) {
	switch streamType {
	case domain.StreamVideo:
		videos, err := rendition.GenerateVideo(meta, c.opts.TargetDuration)
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

func (c *Controller) buildJob(runID string, startBy time.Time, sourceURL, mediaKey string, streamType domain.StreamType, renditionName string, index, startIdx, endIdx int, allSegments []domain.Segment, video *domain.VideoRendition, audio *domain.AudioRendition, meta *domain.Metadata) domain.Job {
	segments := append([]domain.Segment(nil), allSegments[startIdx:endIdx+1]...)
	expected := make([]int, len(segments))
	for i, segment := range segments {
		expected[i] = segment.Index
	}
	copiedVideo := streamType == domain.StreamVideo && video != nil && video.Method == domain.DirectStream
	var overlap, trailing *domain.Segment
	if startIdx > 0 && (copiedVideo || streamType == domain.StreamAudio) {
		value := allSegments[startIdx-1]
		overlap = &value
	}
	if copiedVideo && endIdx+1 < len(allSegments) {
		value := allSegments[endIdx+1]
		trailing = &value
	}
	now := time.Now()
	return domain.Job{
		ID:               deterministicJobID(mediaKey, renditionName, streamType, startIdx, endIdx),
		RunID:            runID,
		MediaKey:         mediaKey,
		Profile:          c.pipelineVersion,
		SourceURL:        sourceURL,
		Rendition:        renditionName,
		StreamType:       streamType,
		StartIndex:       startIdx,
		EndIndex:         endIdx,
		RequestedIndex:   index,
		EnqueuedAt:       now,
		StartBy:          startBy,
		Segments:         segments,
		Overlap:          overlap,
		Trailing:         trailing,
		ExpectedIndices:  expected,
		VideoRendition:   video,
		AudioRendition:   audio,
		VideoStreamIndex: meta.Video.Index,
	}
}

func deterministicJobID(sourceURL, rendition string, streamType StreamType, startIdx, endIdx int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s:%s:%d:%d", sourceURL, rendition, streamType, startIdx, endIdx)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
