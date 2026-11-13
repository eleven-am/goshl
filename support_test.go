package goshl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/google/uuid"
)

func jsonMetadata(metadata *domain.Metadata) ([]byte, error) {
	return json.Marshal(metadata)
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

func (s *stubStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.segments == nil {
		s.segments = make(map[int][]byte)
	}
	s.segments[info.Index] = data
	return nil
}

func (s *stubStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segmentCalls++
	return s.segments[info.Index], nil
}

func (s *stubStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
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

type stubCoordinator struct {
	mu            sync.Mutex
	enqueued      []domain.Job
	subCh         chan domain.JobDelivery
	waitCh        chan domain.SegmentStatus
	lockErr       error
	subErrByType  map[domain.StreamType]error
	lockCalls     []string
	lockTTLs      []time.Duration
	releaseCalls  []string
	onAcquireLock func(key string)
	onWaitSegment func(info domain.SegmentData)
}

func (c *stubCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enqueued = append(c.enqueued, job)
	return nil
}

func (c *stubCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType, profile string) (<-chan domain.JobDelivery, error) {
	if c.subErrByType != nil && c.subErrByType[streamType] != nil {
		return nil, c.subErrByType[streamType]
	}
	if c.subCh == nil {
		c.subCh = make(chan domain.JobDelivery)
		close(c.subCh)
	}
	return c.subCh, nil
}

func (c *stubCoordinator) Renew(ctx context.Context, receipt string, ttl time.Duration) error {
	return nil
}

func (c *stubCoordinator) Ack(ctx context.Context, receipt string) error { return nil }

func (c *stubCoordinator) SettleJob(ctx context.Context, receipt string, status domain.JobStatus) error {
	return nil
}

func (c *stubCoordinator) WaitJob(ctx context.Context, runID string) (<-chan domain.JobStatus, error) {
	return make(chan domain.JobStatus), nil
}

func (c *stubCoordinator) NotifySegment(ctx context.Context, info domain.SegmentData, status domain.SegmentStatus) error {
	return nil
}

func (c *stubCoordinator) WaitSegment(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error) {
	if c.onWaitSegment != nil {
		c.onWaitSegment(info)
	}
	return c.waitSegmentChan(), nil
}

func (c *stubCoordinator) waitSegmentChan() chan domain.SegmentStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waitCh == nil {
		c.waitCh = make(chan domain.SegmentStatus, 1)
	}
	return c.waitCh
}

func (c *stubCoordinator) sendWaitStatus(status domain.SegmentStatus) {
	c.waitSegmentChan() <- status
}

func (c *stubCoordinator) AcquireJob(ctx context.Context, jobID, runID string, ttl time.Duration) (string, string, error) {
	key := "job:" + jobID
	c.mu.Lock()
	c.lockCalls = append(c.lockCalls, key)
	c.lockTTLs = append(c.lockTTLs, ttl)
	c.mu.Unlock()
	if c.onAcquireLock != nil {
		c.onAcquireLock(key)
	}
	if c.lockErr != nil {
		return "", "active-run", c.lockErr
	}
	return uuid.New().String(), runID, nil
}

func (c *stubCoordinator) ReleaseJob(ctx context.Context, jobID, runID, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCalls = append(c.releaseCalls, "job:"+jobID)
	return nil
}

func (c *stubCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.mu.Lock()
	c.lockCalls = append(c.lockCalls, key)
	c.lockTTLs = append(c.lockTTLs, ttl)
	c.mu.Unlock()
	if c.onAcquireLock != nil {
		c.onAcquireLock(key)
	}
	if c.lockErr != nil {
		return "", c.lockErr
	}
	return uuid.New().String(), nil
}

func (c *stubCoordinator) ReleaseLock(ctx context.Context, key string, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCalls = append(c.releaseCalls, key)
	return nil
}

func (c *stubCoordinator) Close() {}

type stubPathGen struct{}

func testVideoMetadata(duration float64, keyframes []float64) *domain.Metadata {
	return &domain.Metadata{Duration: duration, Keyframes: keyframes, Video: domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, Bitrate: 5_000_000, PixelFormat: "yuv420p", Profile: "High", Level: 40, FrameRate: 24, StartsWithKeyframe: true}}
}

func (stubPathGen) MasterPlaylist(sourceURL string) string { return "/master" }

func (stubPathGen) VariantPlaylist(sourceURL string, rendition string, streamType domain.StreamType) string {
	return "/variant"
}

func (stubPathGen) Segment(sourceURL string, rendition string, streamType domain.StreamType, index int) string {
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

func installFakeFFmpeg(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg")
	content := `#!/bin/sh
if [ "$1" = "-hwaccels" ]; then
echo "Hardware acceleration methods:"; echo cuda; echo videotoolbox; exit 0; fi
if [ "$1" = "-encoders" ]; then
echo "h264_nvenc"; echo "h264_videotoolbox"; exit 0; fi
exit 0
`
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}
	orig := os.Getenv("PATH")
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+orig)
	return func() { _ = os.Setenv("PATH", orig) }
}
