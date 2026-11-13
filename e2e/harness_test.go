package e2e

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/eleven-am/goshl"
	"github.com/google/uuid"
)

type clusterRoute struct {
	stream  goshl.StreamType
	profile string
}

type clusterLock struct {
	token   string
	expires time.Time
}

type clusterDelivery struct {
	job     goshl.Job
	receipt string
	expires time.Time
}

type clusterCoordinator struct {
	mu             sync.Mutex
	queues         map[clusterRoute]chan goshl.JobDelivery
	jobWaiters     map[string]map[chan goshl.JobStatus]struct{}
	jobStatuses    map[string]goshl.JobStatus
	segmentWaiters map[string]map[chan goshl.SegmentStatus]struct{}
	locks          map[string]clusterLock
	activeJobs     map[string]string
	deliveries     map[string]clusterDelivery
	runReceipts    map[string]string
	settled        map[string]goshl.JobStatus
	releases       []string
	acks           []string
	onEnqueue      func(goshl.Job)
	enqueueErr     error
	enqueueRejects bool
	onRenew        func(context.Context, goshl.Job) error
	onSettle       func(context.Context, string, goshl.JobStatus) error
}

func newClusterCoordinator() *clusterCoordinator {
	return &clusterCoordinator{
		queues:         make(map[clusterRoute]chan goshl.JobDelivery),
		jobWaiters:     make(map[string]map[chan goshl.JobStatus]struct{}),
		jobStatuses:    make(map[string]goshl.JobStatus),
		segmentWaiters: make(map[string]map[chan goshl.SegmentStatus]struct{}),
		locks:          make(map[string]clusterLock),
		activeJobs:     make(map[string]string),
		deliveries:     make(map[string]clusterDelivery),
		runReceipts:    make(map[string]string),
		settled:        make(map[string]goshl.JobStatus),
	}
}

func (c *clusterCoordinator) queue(route clusterRoute) chan goshl.JobDelivery {
	queue := c.queues[route]
	if queue == nil {
		queue = make(chan goshl.JobDelivery, 8)
		c.queues[route] = queue
	}
	return queue
}

func (c *clusterCoordinator) Enqueue(ctx context.Context, job goshl.Job) error {
	if c.enqueueRejects {
		return fmt.Errorf("queue refused job: %w", goshl.ErrEnqueueRejected)
	}
	if c.onEnqueue != nil {
		c.onEnqueue(job)
	}
	receipt := uuid.NewString()
	c.mu.Lock()
	queue := c.queue(clusterRoute{stream: job.StreamType, profile: job.Profile})
	delivery := clusterDelivery{job: job, receipt: receipt, expires: time.Now().Add(5 * time.Second)}
	c.deliveries[receipt] = delivery
	c.runReceipts[job.RunID] = receipt
	c.mu.Unlock()
	select {
	case queue <- goshl.JobDelivery{Job: job, Receipt: receipt}:
		return c.enqueueErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *clusterCoordinator) Subscribe(ctx context.Context, streamType goshl.StreamType, profile string) (<-chan goshl.JobDelivery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queue(clusterRoute{stream: streamType, profile: profile}), nil
}

func (c *clusterCoordinator) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	c.mu.Lock()
	delivery, found := c.deliveries[receipt]
	if !found {
		c.mu.Unlock()
		return errors.New("unknown receipt")
	}
	onRenew := c.onRenew
	c.mu.Unlock()
	if onRenew != nil {
		if err := onRenew(ctx, delivery.job); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, found := c.deliveries[receipt]; !found {
		return errors.New("unknown receipt")
	}
	delivery.expires = time.Now().Add(ttl)
	c.deliveries[receipt] = delivery
	return nil
}

func (c *clusterCoordinator) Ack(ctx context.Context, receipt string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delivery, found := c.deliveries[receipt]
	if !found {
		return errors.New("unknown receipt")
	}
	delete(c.deliveries, receipt)
	if c.runReceipts[delivery.job.RunID] == receipt {
		delete(c.runReceipts, delivery.job.RunID)
	}
	c.acks = append(c.acks, receipt)
	return nil
}

func (c *clusterCoordinator) redeliverExpired(runID string, now time.Time) (goshl.JobDelivery, bool, error) {
	c.mu.Lock()
	receipt := c.runReceipts[runID]
	delivery, found := c.deliveries[receipt]
	if !found {
		c.mu.Unlock()
		return goshl.JobDelivery{}, false, errors.New("active delivery not found")
	}
	if now.Before(delivery.expires) {
		c.mu.Unlock()
		return goshl.JobDelivery{}, false, nil
	}
	delete(c.deliveries, receipt)
	newReceipt := uuid.NewString()
	delivery.receipt = newReceipt
	delivery.expires = time.Now().Add(5 * time.Second)
	c.deliveries[newReceipt] = delivery
	c.runReceipts[runID] = newReceipt
	queue := c.queue(clusterRoute{stream: delivery.job.StreamType, profile: delivery.job.Profile})
	c.mu.Unlock()

	redelivery := goshl.JobDelivery{Job: delivery.job, Receipt: newReceipt}
	queue <- redelivery
	return redelivery, true, nil
}

func (c *clusterCoordinator) deliveryExpiry(receipt string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delivery, found := c.deliveries[receipt]
	return delivery.expires, found
}

func (c *clusterCoordinator) SettleJob(ctx context.Context, receipt string, status goshl.JobStatus) error {
	if c.onSettle != nil {
		if err := c.onSettle(ctx, receipt, status); err != nil {
			return err
		}
	}
	c.mu.Lock()
	if previous, found := c.settled[receipt]; found {
		c.mu.Unlock()
		if previous == status {
			return nil
		}
		return errors.New("receipt already settled with another status")
	}
	delivery, found := c.deliveries[receipt]
	if !found {
		c.mu.Unlock()
		return errors.New("unknown receipt")
	}
	if delivery.job.ID != status.JobID || delivery.job.RunID != status.RunID || !terminalJobState(status.State) {
		c.mu.Unlock()
		return errors.New("invalid terminal status")
	}
	if previous, found := c.jobStatuses[status.RunID]; found && terminalJobState(previous.State) {
		c.mu.Unlock()
		return errors.New("run already settled")
	}
	c.jobStatuses[status.RunID] = status
	c.settled[receipt] = status
	delete(c.deliveries, receipt)
	if c.runReceipts[delivery.job.RunID] == receipt {
		delete(c.runReceipts, delivery.job.RunID)
	}
	c.acks = append(c.acks, receipt)
	waiters := make([]chan goshl.JobStatus, 0, len(c.jobWaiters[status.RunID]))
	for waiter := range c.jobWaiters[status.RunID] {
		waiters = append(waiters, waiter)
	}
	c.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter <- status:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func terminalJobState(state goshl.JobState) bool {
	return state == goshl.JobStateCompleted || state == goshl.JobStateFailed || state == goshl.JobStateCancelled
}

func (c *clusterCoordinator) WaitJob(ctx context.Context, runID string) (<-chan goshl.JobStatus, error) {
	waiter := make(chan goshl.JobStatus, 1)
	c.mu.Lock()
	if c.jobWaiters[runID] == nil {
		c.jobWaiters[runID] = make(map[chan goshl.JobStatus]struct{})
	}
	c.jobWaiters[runID][waiter] = struct{}{}
	status, found := c.jobStatuses[runID]
	c.mu.Unlock()
	if found {
		waiter <- status
	}
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		delete(c.jobWaiters[runID], waiter)
		c.mu.Unlock()
	}()
	return waiter, nil
}

func clusterSegmentKey(info goshl.SegmentData) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%t", info.MediaKey, info.Rendition, info.Index, info.IsVideo)
}

func (c *clusterCoordinator) NotifySegment(ctx context.Context, info goshl.SegmentData, status goshl.SegmentStatus) error {
	key := clusterSegmentKey(info)
	c.mu.Lock()
	waiters := make([]chan goshl.SegmentStatus, 0, len(c.segmentWaiters[key]))
	for waiter := range c.segmentWaiters[key] {
		waiters = append(waiters, waiter)
	}
	c.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter <- status:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *clusterCoordinator) WaitSegment(ctx context.Context, info goshl.SegmentData) (<-chan goshl.SegmentStatus, error) {
	waiter := make(chan goshl.SegmentStatus, 1)
	key := clusterSegmentKey(info)
	c.mu.Lock()
	if c.segmentWaiters[key] == nil {
		c.segmentWaiters[key] = make(map[chan goshl.SegmentStatus]struct{})
	}
	c.segmentWaiters[key][waiter] = struct{}{}
	c.mu.Unlock()
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		delete(c.segmentWaiters[key], waiter)
		c.mu.Unlock()
	}()
	return waiter, nil
}

func (c *clusterCoordinator) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := "job:" + jobID
	if lock, found := c.locks[key]; found && time.Now().Before(lock.expires) {
		return "", c.activeJobs[jobID], goshl.ErrLockHeld
	}
	token := uuid.NewString()
	c.locks[key] = clusterLock{token: token, expires: time.Now().Add(ttl)}
	c.activeJobs[jobID] = runID
	return token, runID, nil
}

func (c *clusterCoordinator) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := "job:" + jobID
	lock, found := c.locks[key]
	if !found || lock.token != token || c.activeJobs[jobID] != runID {
		return errors.New("job lock ownership mismatch")
	}
	delete(c.locks, key)
	delete(c.activeJobs, jobID)
	c.releases = append(c.releases, key)
	return nil
}

func (c *clusterCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lock, found := c.locks[key]; found && time.Now().Before(lock.expires) {
		return "", goshl.ErrLockHeld
	}
	token := uuid.NewString()
	c.locks[key] = clusterLock{token: token, expires: time.Now().Add(ttl)}
	return token, nil
}

func (c *clusterCoordinator) ReleaseLock(ctx context.Context, key, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock, found := c.locks[key]
	if !found || lock.token != token {
		return errors.New("lock ownership mismatch")
	}
	delete(c.locks, key)
	c.releases = append(c.releases, key)
	return nil
}

func (c *clusterCoordinator) releaseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.releases)
}

type stubStorage struct {
	mu                 sync.Mutex
	metaData           []byte
	metaExists         bool
	segments           map[int][]byte
	segmentCalls       int
	subtitleData       []byte
	metaExistsCalls    int
	segmentExistsCalls int
	onSegmentExists    func()
	hideSegments       bool
}

func (s *stubStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	s.metaExistsCalls++
	return s.metaExists, nil
}

func (s *stubStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.metaData, nil
}

func (s *stubStorage) SetMetadata(ctx context.Context, sourceURL string, data []byte) error {
	s.metaData = data
	s.metaExists = true
	return nil
}

func (s *stubStorage) WriteSegment(ctx context.Context, info goshl.SegmentData, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.segments == nil {
		s.segments = make(map[int][]byte)
	}
	s.segments[info.Index] = data
	return nil
}

func (s *stubStorage) ReadSegment(ctx context.Context, info goshl.SegmentData) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segmentCalls++
	return s.segments[info.Index], nil
}

func (s *stubStorage) SegmentExists(ctx context.Context, info goshl.SegmentData) (bool, error) {
	s.mu.Lock()
	s.segmentExistsCalls++
	_, ok := s.segments[info.Index]
	hook := s.onSegmentExists
	hidden := s.hideSegments
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ok && !hidden, nil
}

func (s *stubStorage) setSegment(index int, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.segments == nil {
		s.segments = make(map[int][]byte)
	}
	s.segments[index] = data
}

func (s *stubStorage) WriteSprite(ctx context.Context, mediaID string, index int, data []byte) error {
	return nil
}

func (s *stubStorage) ReadSprite(ctx context.Context, mediaID string, index int) ([]byte, error) {
	return nil, nil
}

func (s *stubStorage) SpriteExists(ctx context.Context, mediaID string, index int) (bool, error) {
	return false, nil
}

func (s *stubStorage) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	s.subtitleData = data
	return nil
}

func (s *stubStorage) ReadSubtitleVTT(ctx context.Context, mediaID string, lang string) ([]byte, error) {
	return s.subtitleData, nil
}

func (s *stubStorage) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	return s.subtitleData != nil, nil
}

func (s *stubStorage) WritePreview(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}

func (s *stubStorage) ReadPreview(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}

func (s *stubStorage) PreviewExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}

func (s *stubStorage) WriteThumbnail(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}

func (s *stubStorage) ReadThumbnail(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}

func (s *stubStorage) ThumbnailExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}

type stubPathGen struct{}

func (stubPathGen) MasterPlaylist(sourceURL string) string { return "/master" }

func (stubPathGen) VariantPlaylist(sourceURL string, rendition string, streamType goshl.StreamType) string {
	return "/variant"
}

func (stubPathGen) Segment(sourceURL string, rendition string, streamType goshl.StreamType, index int) string {
	return "/segment"
}

func (stubPathGen) SpriteVTT(sourceURL string) string { return "/sprites.vtt" }

func (stubPathGen) Sprite(sourceURL string, index int) string {
	return "/sprite-" + strconv.Itoa(index) + ".jpg"
}

func (stubPathGen) SubtitlePlaylist(sourceURL string, trackID string) string { return "/sub.m3u8" }

func (stubPathGen) SubtitleVTT(sourceURL string, lang string) string { return "/sub.vtt" }

func (stubPathGen) Preview(sourceURL string) string { return "/preview.gif" }

func (stubPathGen) Thumbnail(sourceURL string) string { return "/thumbnail.jpg" }

type timelineStorage struct {
	*stubStorage
	lock      sync.Mutex
	metadata  map[string][]byte
	segments  map[goshl.SegmentData][]byte
	subtitles map[string][]byte
}

func newTimelineStorage() *timelineStorage {
	return &timelineStorage{
		stubStorage: &stubStorage{},
		metadata:    make(map[string][]byte),
		segments:    make(map[goshl.SegmentData][]byte),
		subtitles:   make(map[string][]byte),
	}
}

func (s *timelineStorage) MetadataExists(ctx context.Context, mediaKey string) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	_, ok := s.metadata[mediaKey]
	return ok, nil
}

func (s *timelineStorage) GetMetadata(ctx context.Context, mediaKey string) ([]byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.metadata[mediaKey], nil
}

func (s *timelineStorage) SetMetadata(ctx context.Context, mediaKey string, data []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.metadata[mediaKey] = data
	return nil
}

func (s *timelineStorage) WriteSegment(ctx context.Context, info goshl.SegmentData, data []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.segments[info] = append([]byte(nil), data...)
	return nil
}

func (s *timelineStorage) ReadSegment(ctx context.Context, info goshl.SegmentData) ([]byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	data, ok := s.segments[info]
	if !ok {
		return nil, fmt.Errorf("segment %d of %s is not stored", info.Index, info.Rendition)
	}
	return data, nil
}

func (s *timelineStorage) SegmentExists(ctx context.Context, info goshl.SegmentData) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	_, ok := s.segments[info]
	return ok, nil
}

func (s *timelineStorage) WriteSubtitleVTT(ctx context.Context, assetKey string, trackID string, data []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.subtitles[assetKey+"|"+trackID] = append([]byte(nil), data...)
	return nil
}

func (s *timelineStorage) ReadSubtitleVTT(ctx context.Context, assetKey string, trackID string) ([]byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.subtitles[assetKey+"|"+trackID], nil
}

func (s *timelineStorage) SubtitleVTTExists(ctx context.Context, assetKey string, trackID string) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	_, ok := s.subtitles[assetKey+"|"+trackID]
	return ok, nil
}

type timelineObserver struct {
	mu       sync.Mutex
	failures []string
}

func (o *timelineObserver) Observe(ctx context.Context, event goshl.Event) {
	if event.Type != goshl.EventJobFailed {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, fmt.Sprintf("%s %s %d-%d: %s", event.StreamType, event.Rendition, event.StartIndex, event.EndIndex, event.Error))
}

func (o *timelineObserver) failed() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.failures...)
}

const outputTimelineOffset = 10 * time.Second
