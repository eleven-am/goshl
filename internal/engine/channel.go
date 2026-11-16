package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/transcode"
)

const (
	defaultChannelSegmentsPerJob = 10
	channelFailureTTL            = 10 * time.Second
	maxChannelFailures           = 1024
)

var (
	ErrInvalidChannelProfile = domain.ErrInvalidChannelProfile
	ErrChannelDisabled       = errors.New("channel API is disabled: ChannelPoolSize is 0")
)

type channelFailure struct {
	message string
	expires time.Time
}

type channelState struct {
	mu       sync.Mutex
	failures map[string]channelFailure
	now      func() time.Time
}

func (s *channelState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func channelFailureKey(mediaKey, renditionName string) string {
	return mediaKey + "\x00" + renditionName
}

func (s *channelState) recordOutcome(f *flight, status domain.JobStatus) {
	if f.streamType != domain.StreamChannel {
		return
	}
	key := channelFailureKey(f.mediaKey, f.rendition)
	s.mu.Lock()
	defer s.mu.Unlock()
	if status.State == domain.JobStateCompleted {
		delete(s.failures, key)
		return
	}
	now := s.clock()
	if s.failures == nil {
		s.failures = make(map[string]channelFailure)
	}
	if len(s.failures) >= maxChannelFailures {
		for other, failure := range s.failures {
			if now.After(failure.expires) {
				delete(s.failures, other)
			}
		}
	}
	if len(s.failures) >= maxChannelFailures {
		return
	}
	s.failures[key] = channelFailure{message: status.Error, expires: now.Add(channelFailureTTL)}
}

func (s *channelState) recentFailure(mediaKey, renditionName string) error {
	key := channelFailureKey(mediaKey, renditionName)
	s.mu.Lock()
	defer s.mu.Unlock()
	failure, found := s.failures[key]
	if !found {
		return nil
	}
	if s.clock().After(failure.expires) {
		delete(s.failures, key)
		return nil
	}
	return fmt.Errorf("segment error: %s", failure.message)
}

func (s *channelState) clearFailures(mediaKey string) {
	prefix := mediaKey + "\x00"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.failures {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(s.failures, key)
		}
	}
}

func (o *Config) setChannelDefaults() {
	if o.ChannelSegmentsPerJob == 0 {
		o.ChannelSegmentsPerJob = defaultChannelSegmentsPerJob
	}
}

func (o *Config) validateChannelConfig() error {
	if o.ChannelPoolSize < 0 || o.ChannelPoolSize > maxPoolSize {
		return fmt.Errorf("%w: ChannelPoolSize must be between 0 and %d", ErrInvalidOptions, maxPoolSize)
	}
	if o.ChannelSegmentsPerJob < 1 || o.ChannelSegmentsPerJob > maxSegmentsPerJob {
		return fmt.Errorf("%w: ChannelSegmentsPerJob must be between 1 and %d", ErrInvalidOptions, maxSegmentsPerJob)
	}
	return nil
}

func newChannelPool(opts Config, cmdBuilder *ffmpeg.CommandBuilder, segments domain.Storage, route string) *transcode.Pool {
	if opts.ChannelPoolSize <= 0 {
		return nil
	}
	return transcode.NewPool(
		opts.Coordinator,
		opts.ChannelPoolSize,
		domain.StreamChannel,
		opts.Storage,
		cmdBuilder,
		segments,
		transcode.PoolConfig{JobTimeout: opts.JobTimeout, Observer: opts.Observer, Profile: route},
	)
}

func (e *Engine) startChannelPool(ctx context.Context) error {
	if e.channelPool == nil {
		return nil
	}
	if err := e.channelPool.Start(ctx); err != nil {
		return fmt.Errorf("start channel pool: %w", err)
	}
	return nil
}

func (e *Engine) stopChannelPool() {
	if e.channelPool != nil {
		e.channelPool.Stop()
	}
}

type channelRequest struct {
	segmentRequest
	profile    domain.ChannelProfile
	audioTrack int
}

func (e *Engine) channelRequest(ctx context.Context, sourceURL string, profile domain.ChannelProfile, audioTrack int) (channelRequest, error) {
	if err := e.ensureRunning(); err != nil {
		return channelRequest{}, err
	}
	if e.opts.ChannelPoolSize <= 0 {
		return channelRequest{}, ErrChannelDisabled
	}
	normalised, err := profile.Normalised()
	if err != nil {
		return channelRequest{}, err
	}
	if audioTrack < 0 {
		return channelRequest{}, fmt.Errorf("%w: audio track %d", ErrRenditionNotFound, audioTrack)
	}
	mediaKey, err := e.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return channelRequest{}, err
	}
	return channelRequest{
		segmentRequest: segmentRequest{
			sourceURL:  sourceURL,
			mediaKey:   mediaKey,
			streamType: domain.StreamChannel,
			rendition:  rendition.ChannelName(normalised, audioTrack, e.opts.ChannelSegmentsPerJob),
			priority:   domain.PriorityChannel,
		},
		profile:    normalised,
		audioTrack: audioTrack,
	}, nil
}

type channelItem struct {
	meta     *domain.Metadata
	segments []domain.Segment
	video    domain.VideoRendition
	audio    domain.AudioRendition
	spec     domain.ChannelSpec
}

func (e *Engine) channelItem(ctx context.Context, request channelRequest) (*channelItem, error) {
	meta, err := e.getMetadataWithKey(ctx, request.sourceURL, request.mediaKey)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}
	video, audio, spec, err := rendition.Channel(meta, request.profile, request.audioTrack, e.opts.ChannelSegmentsPerJob)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidChannelProfile) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrRenditionNotFound, request.rendition, err)
	}
	spec.SourceFrameRate = meta.Video.FrameRate
	segments := playlist.CalculateChannelSegments(meta.Duration, request.profile)
	if len(segments) == 0 {
		return nil, fmt.Errorf("%w: source has no duration", ErrSegmentOutOfRange)
	}
	if video.Colour.Tonemap {
		if err := e.tonemap.check(ctx); err != nil {
			return nil, fmt.Errorf("channel: %w", err)
		}
	}
	return &channelItem{meta: meta, segments: segments, video: video, audio: audio, spec: spec}, nil
}

func (e *Engine) channelFlightPlan(item *channelItem, index int) *flightPlan {
	start, end := playlist.ChannelBatch(index, e.opts.ChannelSegmentsPerJob, len(item.segments))
	video, audio, spec := item.video, item.audio, item.spec
	return &flightPlan{
		segments:         item.segments,
		start:            start,
		end:              end,
		video:            &video,
		audio:            &audio,
		channel:          &spec,
		videoStreamIndex: item.meta.Video.Index,
		neighbours:       true,
	}
}

func (e *Engine) ChannelPlan(ctx context.Context, sourceURL string, profile domain.ChannelProfile, audioTrack int) (domain.ChannelPlan, error) {
	request, err := e.channelRequest(ctx, sourceURL, profile, audioTrack)
	if err != nil {
		return domain.ChannelPlan{}, err
	}
	item, err := e.channelItem(ctx, request)
	if err != nil {
		return domain.ChannelPlan{}, err
	}
	return domain.ChannelPlan{
		MediaKey:     request.mediaKey,
		Rendition:    request.rendition,
		Codecs:       item.video.Codec + "," + item.audio.CodecString,
		FrameRateNum: request.profile.FrameRateNum,
		FrameRateDen: request.profile.FrameRateDen,
		Frames:       item.spec.TotalFrames,
		Duration:     domain.ChannelFrameTime(item.spec.TotalFrames, request.profile.FrameRateNum, request.profile.FrameRateDen),
		Segments:     append([]domain.Segment(nil), item.segments...),
	}, nil
}

func (e *Engine) ChannelSegment(ctx context.Context, sourceURL string, profile domain.ChannelProfile, audioTrack int, index int) ([]byte, error) {
	request, err := e.channelRequest(ctx, sourceURL, profile, audioTrack)
	if err != nil {
		return nil, err
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, index)
	}
	request.index = index
	request.plan = func(ctx context.Context) (*flightPlan, error) {
		item, err := e.channelItem(ctx, request)
		if err != nil {
			return nil, err
		}
		if index >= len(item.segments) {
			return nil, fmt.Errorf("%w: %d", ErrSegmentOutOfRange, index)
		}
		return e.channelFlightPlan(item, index), nil
	}
	for attempt := 0; attempt < maxFlightReentries; attempt++ {
		data, err := e.requestSegment(ctx, request.segmentRequest)
		if !errors.Is(err, errFlightAbandoned) {
			return data, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	return nil, fmt.Errorf("%w: segment %d", ErrSegmentTimeout, index)
}

func (e *Engine) ChannelPrefetch(ctx context.Context, sourceURL string, profile domain.ChannelProfile, audioTrack int, from int, count int) error {
	request, err := e.channelRequest(ctx, sourceURL, profile, audioTrack)
	if err != nil {
		return err
	}
	if count <= 0 {
		return nil
	}
	if from < 0 {
		return fmt.Errorf("%w: %d", ErrSegmentOutOfRange, from)
	}
	if err := e.channel.recentFailure(request.mediaKey, request.rendition); err != nil {
		return err
	}
	item, err := e.channelItem(ctx, request)
	if err != nil {
		return err
	}
	if from >= len(item.segments) {
		return fmt.Errorf("%w: %d", ErrSegmentOutOfRange, from)
	}
	last := from + count - 1
	if last >= len(item.segments) || last < from {
		last = len(item.segments) - 1
	}
	return e.prefetchRange(ctx, request, item, from, last)
}

func (e *Engine) prefetchRange(ctx context.Context, request channelRequest, item *channelItem, from, last int) error {
	index := from
	for index <= last {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.ensureRunning(); err != nil {
			return err
		}
		if covering := e.coveringFlight(request.mediaKey, domain.StreamChannel, request.rendition, index); covering != nil {
			index = covering.end + 1
			continue
		}
		probe := request.segmentRequest
		probe.index = index
		exists, err := e.opts.Storage.SegmentExists(ctx, probe.info())
		if err != nil {
			return fmt.Errorf("check segment: %w", err)
		}
		if exists {
			index++
			continue
		}
		plan := e.channelFlightPlan(item, index)
		existing, created := e.joinFlight(request.mediaKey, domain.StreamChannel, request.rendition, plan.start, plan.end)
		if existing != nil {
			index = existing.end + 1
			continue
		}
		outcome, err := e.launchFlight(ctx, probe, plan, created, nil)
		if err != nil {
			return err
		}
		switch {
		case outcome.stored:
			index++
		case outcome.held:
			e.abandonFlight(created)
			index = created.end + 1
		default:
			index = created.end + 1
		}
	}
	return nil
}
