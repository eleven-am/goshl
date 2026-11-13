package goshl

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/google/uuid"
)

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

type invalidatingStorage struct {
	*stubStorage
	keys []string
}

func (s *invalidatingStorage) DeleteMedia(ctx context.Context, mediaKey string) error {
	s.keys = append(s.keys, mediaKey)
	return nil
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
func (stubPathGen) SubtitleVTT(sourceURL string, lang string) string         { return "/sub.vtt" }
func (stubPathGen) Preview(sourceURL string) string                          { return "/preview.gif" }
func (stubPathGen) Thumbnail(sourceURL string) string                        { return "/thumbnail.jpg" }

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

func TestMasterPlaylistUsesCachedMetadata(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Video: domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000}, Audios: []domain.AudioStream{{Codec: "ac3", Channels: 6, Bitrate: 640_000}}}
	metaBytes, _ := json.Marshal(meta)
	svc := NewController(Options{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	out, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master playlist err: %v", err)
	}
	if out == "" || !strings.Contains(out, "#EXT-X-STREAM-INF") {
		t.Fatalf("expected playlist content, got %q", out)
	}
}

func masterWithSubtitles(t *testing.T, omit bool) string {
	t.Helper()
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{
		Video:  domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Audios: []domain.AudioStream{{Codec: "aac", Channels: 2, Bitrate: 128_000, Language: "eng"}},
		Subtitles: []domain.SubtitleStream{
			{Index: 2, ID: "t2", Codec: "subrip", Language: "eng", Default: true},
			{Index: 3, ID: "t3", Codec: "subrip", Language: "fra"},
		},
	}
	metaBytes, _ := json.Marshal(meta)
	svc := NewController(Options{
		Storage:                &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator:            &stubCoordinator{},
		PathGen:                stubPathGen{},
		OmitSubtitleRenditions: omit,
	})

	out, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master playlist err: %v", err)
	}
	return out
}

func TestMasterOmitsSubtitleRenditionsWhenConfigured(t *testing.T) {
	out := masterWithSubtitles(t, true)
	if !strings.Contains(out, "#EXT-X-STREAM-INF") {
		t.Fatalf("expected variants in playlist: %s", out)
	}
	for _, unwanted := range []string{"TYPE=SUBTITLES", "SUBTITLES="} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("playlist must not contain %q: %s", unwanted, out)
		}
	}
}

func TestMasterAdvertisesSubtitleRenditionsByDefault(t *testing.T) {
	out := masterWithSubtitles(t, false)
	if got := strings.Count(out, "TYPE=SUBTITLES"); got != 2 {
		t.Fatalf("expected 2 subtitle renditions, got %d: %s", got, out)
	}
	if !strings.Contains(out, "SUBTITLES=\"subtitles\"") {
		t.Fatalf("expected variants to reference the subtitle group: %s", out)
	}
}

func TestSegmentReturnsCachedData(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 10, Keyframes: []float64{0, 6, 10}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{3: []byte("ok")}}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)
	if err != nil {
		t.Fatalf("segment err: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("unexpected data %q", data)
	}
}

func TestSegmentEnqueuesWhenMissingAndUsesPubSubReady(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	meta.Audios = []domain.AudioStream{{Index: 1, Codec: "aac", Channels: 2, Bitrate: 128_000, Default: true}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{0: []byte("seg0")}}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamAudio, "aac_stereo", 1)
	if err != nil {
		t.Fatalf("segment err: %v", err)
	}
	if data != nil {
		t.Fatalf("stub storage has no segment 1, got %q", data)
	}
	if len(coord.enqueued) != 1 {
		t.Fatalf("expected enqueue, got %d", len(coord.enqueued))
	}
	if coord.enqueued[0].StartIndex != 1 || coord.enqueued[0].EndIndex != 1 {
		t.Fatalf("unexpected job indices: %#v", coord.enqueued[0])
	}
	if coord.enqueued[0].VideoStreamIndex != meta.Video.Index {
		t.Fatalf("enqueued job must carry the video stream index %d, got %d", meta.Video.Index, coord.enqueued[0].VideoStreamIndex)
	}
	if overlap := coord.enqueued[0].Overlap; overlap == nil || overlap.Index != 0 || coord.enqueued[0].Trailing != nil {
		t.Fatalf("enqueued audio job must carry segment 0 as overlap and no trailing segment: %#v / %#v", overlap, coord.enqueued[0].Trailing)
	}
}

func TestSubtitleVTTReturnsErrorWhenLanguageMissing(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Subtitles: []domain.SubtitleStream{{Language: "es"}}}
	metaBytes, _ := json.Marshal(meta)
	svc := NewController(Options{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	if _, err := svc.SubtitleVTT(context.Background(), "file:///media", "en"); err == nil {
		t.Fatalf("expected error for missing language")
	}
}

func TestVariantPlaylistReturnsPlaylist(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	svc := NewController(Options{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	out, err := svc.VariantPlaylist(context.Background(), "file:///media", domain.StreamVideo, "1080p")
	if err != nil {
		t.Fatalf("variant playlist err: %v", err)
	}
	if !strings.Contains(out, "#EXTM3U") || !strings.Contains(out, "#EXTINF") {
		t.Fatalf("expected valid playlist, got %q", out)
	}
}

func TestStartAndStop(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	svc := NewController(Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start err: %v", err)
	}
	svc.Stop()
}

func TestStartStopsVideoPoolWhenAudioPoolFails(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{
		subErrByType: map[domain.StreamType]error{
			domain.StreamAudio: errors.New("audio subscribe failed"),
		},
	}
	svc := NewController(Options{
		Storage:     &stubStorage{},
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	if err := svc.Start(context.Background()); err == nil {
		t.Fatalf("expected audio pool start failure")
	}

	delete(coord.subErrByType, domain.StreamAudio)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("expected controller to restart after partial start failure: %v", err)
	}
	svc.Stop()
}

func TestSpriteURLUsesPathGeneratorForEachIndex(t *testing.T) {
	svc := NewController(Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})

	if got := svc.spriteURL("file:///media")(12); got != "/sprite-12.jpg" {
		t.Fatalf("expected direct path generator result, got %q", got)
	}
}

func TestPreviewReturnsDataWithCustomConfig(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 10}
	metaBytes, _ := json.Marshal(meta)
	svc := NewController(Options{
		Storage:     &stubStorage{metaData: metaBytes, metaExists: true},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
		DefaultPreviewConfig: PreviewConfig{
			MaxFrames:     20,
			Frameduration: 200,
			Width:         320,
		},
	})

	_, err := svc.Preview(context.Background(), "file:///media", &PreviewConfig{
		MaxFrames: 10,
		Width:     240,
	})
	if err == nil {
		t.Logf("preview with custom config executed (error expected due to missing sprites)")
	}
}

func TestOptionsSetDefaults(t *testing.T) {
	opts := Options{}
	opts.setDefaults()

	if opts.SegmentTimeout != 30*time.Second {
		t.Errorf("expected SegmentTimeout 30s, got %v", opts.SegmentTimeout)
	}
	if opts.TargetDuration != 6.0 {
		t.Errorf("expected TargetDuration 6.0, got %v", opts.TargetDuration)
	}
	if opts.SegmentsPerJob != 4 {
		t.Errorf("expected SegmentsPerJob 4, got %v", opts.SegmentsPerJob)
	}
	if opts.VideoPoolSize != 2 {
		t.Errorf("expected VideoPoolSize 2, got %v", opts.VideoPoolSize)
	}
	if opts.AudioPoolSize != 4 {
		t.Errorf("expected AudioPoolSize 4, got %v", opts.AudioPoolSize)
	}
	if opts.DefaultPreviewConfig.MaxFrames != 40 {
		t.Errorf("expected MaxFrames 40, got %v", opts.DefaultPreviewConfig.MaxFrames)
	}
	if opts.DefaultPreviewConfig.Frameduration != 250 {
		t.Errorf("expected Frameduration 250, got %v", opts.DefaultPreviewConfig.Frameduration)
	}
	if opts.DefaultPreviewConfig.Width != 480 {
		t.Errorf("expected Width 480, got %v", opts.DefaultPreviewConfig.Width)
	}
}

func TestOptionsValidatePanicsOnMissingStorage(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for missing Storage")
		}
	}()
	opts := Options{Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}}
	opts.validate()
}

func TestOptionsValidatePanicsOnMissingCoordinator(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for missing Coordinator")
		}
	}()
	opts := Options{Storage: &stubStorage{}, PathGen: stubPathGen{}}
	opts.validate()
}

func TestOptionsValidatePanicsOnMissingPathGen(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for missing PathGen")
		}
	}()
	opts := Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}}
	opts.validate()
}

func TestSegmentAcquiresAndReleasesLock(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)

	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected 1 lock call, got %d", len(coord.lockCalls))
	}
	if !strings.HasPrefix(coord.lockCalls[0], "job:") {
		t.Fatalf("expected canonical job lock key, got %q", coord.lockCalls[0])
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("lock must be held while the job runs, got %d releases", len(coord.releaseCalls))
	}
	svc.finishFlightRun(coord.enqueued[0].ID, coord.enqueued[0].RunID)
	if len(coord.releaseCalls) != 1 {
		t.Fatalf("expected 1 release call after job finished, got %d", len(coord.releaseCalls))
	}
}

func TestSegmentWaitsWhenLockHeld(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)

	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected 1 lock attempt, got %d", len(coord.lockCalls))
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("expected 0 release calls when lock not acquired, got %d", len(coord.releaseCalls))
	}
	if len(coord.enqueued) != 0 {
		t.Fatalf("expected no enqueue when lock held, got %d", len(coord.enqueued))
	}
}

func TestSegmentReturnsCoordinatorLockFailure(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: errors.New("coordinator down")}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	_, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err == nil || !strings.Contains(err.Error(), "acquire segment lock") {
		t.Fatalf("expected lock failure, got %v", err)
	}
}

func TestMetadataReturnsCoordinatorLockFailure(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	store := &stubStorage{metaExists: false}
	coord := &stubCoordinator{lockErr: errors.New("coordinator down")}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	_, err := svc.Metadata(context.Background(), "file:///media")
	if err == nil || !strings.Contains(err.Error(), "acquire metadata lock") {
		t.Fatalf("expected lock failure, got %v", err)
	}
}

func TestControllerUsesExplicitHardwareMode(t *testing.T) {
	svc := NewController(Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
		HWAccelMode: AccelVideoToolbox,
	})

	cfg := svc.HWAccelConfig()
	if cfg.Accelerator != AccelVideoToolbox || cfg.Requested != AccelVideoToolbox {
		t.Fatalf("unexpected hw config: %#v", cfg)
	}
}

func TestSegmentDoubleCheckReturnsExistingAfterLock(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}

	lockAcquired := false
	coord := &stubCoordinator{
		onAcquireLock: func(key string) {
			lockAcquired = true
			store.setSegment(1, []byte("appeared"))
		},
	}

	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !lockAcquired {
		t.Fatalf("lock should have been acquired")
	}
	if string(data) != "appeared" {
		t.Fatalf("expected segment data from double-check, got %q", data)
	}
	if len(coord.enqueued) != 0 {
		t.Fatalf("should not enqueue when segment exists after double-check, got %d", len(coord.enqueued))
	}
	if store.segmentExistsCalls != 2 {
		t.Fatalf("expected 2 segment exists calls (initial + double-check), got %d", store.segmentExistsCalls)
	}
}

func TestMetadataAcquiresLockWhenNotCached(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	store := &stubStorage{metaExists: false}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	_, _ = svc.Metadata(context.Background(), "file:///nonexistent")

	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected 1 lock call for metadata, got %d", len(coord.lockCalls))
	}
	if !strings.Contains(coord.lockCalls[0], "metadata:") {
		t.Fatalf("expected metadata lock key, got %q", coord.lockCalls[0])
	}
}

func TestMetadataSkipsLockWhenCached(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 10}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	_, err := svc.Metadata(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(coord.lockCalls) != 0 {
		t.Fatalf("expected no lock calls when metadata cached, got %d", len(coord.lockCalls))
	}
}

func TestDeterministicJobIDs(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(60, []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54, 60})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	firstJobID := coord.enqueued[0].ID
	firstJobRunID := coord.enqueued[0].RunID
	svc.finishFlightRun(firstJobID, firstJobRunID)

	coord.enqueued = nil
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	secondJobID := coord.enqueued[0].ID

	if firstJobID != secondJobID {
		t.Fatalf("job IDs should be deterministic, got %q and %q", firstJobID, secondJobID)
	}

	coord.enqueued = nil
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "720p", 1)
	differentRenditionID := coord.enqueued[0].ID

	if firstJobID == differentRenditionID {
		t.Fatalf("different renditions should have different job IDs")
	}
}

func TestSegmentSkipsLockWhenAlreadyExists(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{3: []byte("cached")}}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "cached" {
		t.Fatalf("expected cached data, got %q", data)
	}

	if len(coord.lockCalls) != 0 {
		t.Fatalf("expected no lock calls when segment exists, got %d", len(coord.lockCalls))
	}
}

func TestSegmentRechecksStorageAfterWaitRegistration(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}, Video: domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, PixelFormat: "yuv420p", Profile: "High", Level: 40}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld}
	coord.onWaitSegment = func(info domain.SegmentData) {
		store.setSegment(info.Index, []byte("ready-before-subscription"))
	}
	svc := NewController(Options{Storage: store, Coordinator: coord, PathGen: stubPathGen{}, SegmentTimeout: 20 * time.Millisecond})

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("expected storage recheck to recover missed notification: %v", err)
	}
	if string(data) != "ready-before-subscription" {
		t.Fatalf("unexpected segment %q", data)
	}
}

func TestSegmentPollsDurableStorageWhenNotificationIsLost(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld}
	svc := NewController(Options{Storage: store, Coordinator: coord, PathGen: stubPathGen{}, SegmentTimeout: time.Second})

	go func() {
		time.Sleep(150 * time.Millisecond)
		store.setSegment(1, []byte("durable-without-notification"))
	}()

	data, err := svc.Segment(context.Background(), "file:///media", StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("expected durable polling to recover notification loss: %v", err)
	}
	if string(data) != "durable-without-notification" {
		t.Fatalf("unexpected segment %q", data)
	}
}

func TestSegmentContinuesPollingAfterNotificationChannelCloses(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := testVideoMetadata(12, []float64{0, 6, 12})
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	waitCh := make(chan domain.SegmentStatus)
	close(waitCh)
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld, waitCh: waitCh}
	svc := NewController(Options{Storage: store, Coordinator: coord, PathGen: stubPathGen{}, SegmentTimeout: time.Second})

	go func() {
		time.Sleep(150 * time.Millisecond)
		store.setSegment(1, []byte("ready-after-close"))
	}()

	data, err := svc.Segment(context.Background(), "file:///media", StreamVideo, "1080p", 1)
	if err != nil {
		t.Fatalf("expected polling after notification close: %v", err)
	}
	if string(data) != "ready-after-close" {
		t.Fatalf("unexpected segment %q", data)
	}
}

func seekTestController(t *testing.T, store *stubStorage, coord *stubCoordinator, opts Options) *Controller {
	t.Helper()
	meta := &domain.Metadata{Duration: 60, Keyframes: []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54, 60}, Video: domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, PixelFormat: "yuv420p", Profile: "High", Level: 40}}
	metaBytes, _ := json.Marshal(meta)
	store.metaData = metaBytes
	store.metaExists = true
	if store.segments == nil {
		store.segments = map[int][]byte{}
	}
	opts.Storage = store
	opts.Coordinator = coord
	opts.PathGen = stubPathGen{}
	return NewController(opts)
}

func TestSegmentSequentialRequestsInsideOneJobEnqueueOnce(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Options{})

	for index := 0; index < 4; index++ {
		coord.waitCh = make(chan domain.SegmentStatus, 1)
		coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}
		_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", index)
	}

	if len(coord.enqueued) != 1 {
		t.Fatalf("expected exactly 1 enqueue for 4 requests in one job range, got %d", len(coord.enqueued))
	}
	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected exactly 1 lock acquisition, got %d", len(coord.lockCalls))
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("lock must be held until the job finishes, got %d releases", len(coord.releaseCalls))
	}
	svc.finishFlightRun(coord.enqueued[0].ID, coord.enqueued[0].RunID)
	if len(coord.releaseCalls) != 1 {
		t.Fatalf("expected the lock released when the job finished, got %d", len(coord.releaseCalls))
	}
}

func TestSegmentEnqueuesAgainAfterJobFinished(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Options{})

	for round := 0; round < 2; round++ {
		coord.waitCh = make(chan domain.SegmentStatus, 1)
		coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}
		_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
		svc.finishFlightRun(coord.enqueued[round].ID, coord.enqueued[round].RunID)
	}

	if len(coord.enqueued) != 2 {
		t.Fatalf("expected a fresh enqueue after the first job finished, got %d", len(coord.enqueued))
	}
}

func TestSegmentAnchorsJobAtRequestedIndex(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Options{})
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)

	if len(coord.enqueued) != 1 {
		t.Fatalf("expected 1 enqueue, got %d", len(coord.enqueued))
	}
	job := coord.enqueued[0]
	if job.StartIndex != 3 || job.EndIndex != 6 || job.RequestedIndex != 3 {
		t.Fatalf("job must start at the requested segment, got %d-%d requested %d", job.StartIndex, job.EndIndex, job.RequestedIndex)
	}
	if job.Segments[0].Index != 3 || len(job.ExpectedIndices) != 4 {
		t.Fatalf("unexpected job segments: %#v", job.ExpectedIndices)
	}
}

func TestSegmentJobStopsBeforeExistingSegments(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	store := &stubStorage{segments: map[int][]byte{5: []byte("cached")}}
	svc := seekTestController(t, store, coord, Options{})
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 3)

	job := coord.enqueued[0]
	if job.StartIndex != 3 || job.EndIndex != 4 {
		t.Fatalf("job must not overlap existing segment 5, got %d-%d", job.StartIndex, job.EndIndex)
	}
}

func TestSegmentJobStopsBeforeNeighbouringFlight(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	coord := &stubCoordinator{}
	svc := seekTestController(t, &stubStorage{}, coord, Options{})

	for _, index := range []int{5, 3} {
		coord.waitCh = make(chan domain.SegmentStatus, 1)
		coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}
		_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", index)
	}

	if len(coord.enqueued) != 2 {
		t.Fatalf("expected 2 enqueues, got %d", len(coord.enqueued))
	}
	if coord.enqueued[0].StartIndex != 5 || coord.enqueued[0].EndIndex != 8 {
		t.Fatalf("unexpected first job: %d-%d", coord.enqueued[0].StartIndex, coord.enqueued[0].EndIndex)
	}
	if coord.enqueued[1].StartIndex != 3 || coord.enqueued[1].EndIndex != 4 {
		t.Fatalf("second job must stop before the in-flight job, got %d-%d", coord.enqueued[1].StartIndex, coord.enqueued[1].EndIndex)
	}
}

func waitForEnqueued(t *testing.T, coord *stubCoordinator, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		coord.mu.Lock()
		got := len(coord.enqueued)
		coord.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d enqueued jobs, got %d", want, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type joinRecorder struct {
	joined chan struct{}
	ready  chan domain.Event
}

func newJoinRecorder() *joinRecorder {
	return &joinRecorder{joined: make(chan struct{}, 8), ready: make(chan domain.Event, 8)}
}

func (r *joinRecorder) observer() domain.Observer {
	return domain.ObserverFunc(func(_ context.Context, event domain.Event) {
		switch event.Type {
		case domain.EventJobJoined:
			r.joined <- struct{}{}
		case domain.EventSegmentReady:
			r.ready <- event
		}
	})
}

type segmentResult struct {
	data []byte
	err  error
}

func startSegment(ctx context.Context, svc *Controller, index int) <-chan segmentResult {
	done := make(chan segmentResult, 1)
	go func() {
		data, err := svc.Segment(ctx, "file:///media", domain.StreamVideo, "1080p", index)
		done <- segmentResult{data, err}
	}()
	return done
}

func cancelFirstWaiter(t *testing.T, svc *Controller, coord *stubCoordinator) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := startSegment(ctx, svc, 2)
	waitForEnqueued(t, coord, 1)
	cancel()
	if res := <-done; !errors.Is(res.err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", res.err)
	}
}

func awaitJoined(t *testing.T, rec *joinRecorder, done <-chan segmentResult) {
	t.Helper()
	select {
	case <-rec.joined:
	case res := <-done:
		t.Fatalf("request returned without joining the cluster job: data=%q err=%v", res.data, res.err)
	case <-time.After(3 * time.Second):
		t.Fatal("request never joined the cluster job")
	}
}

func TestSegmentKeepsClusterJobAfterLocalWaiterLeaves(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	store := &stubStorage{}
	coord := &stubCoordinator{}
	rec := newJoinRecorder()
	svc := seekTestController(t, store, coord, Options{Observer: rec.observer()})

	cancelFirstWaiter(t, svc, coord)

	coord.mu.Lock()
	enqueued := len(coord.enqueued)
	released := len(coord.releaseCalls)
	coord.mu.Unlock()
	if enqueued != 1 {
		t.Fatalf("local waiter departure must leave the single cluster job in place, got %d", enqueued)
	}
	if released != 0 {
		t.Fatalf("local waiter departure must not release the cluster job lock, got %d releases", released)
	}

	followUp := startSegment(context.Background(), svc, 2)
	awaitJoined(t, rec, followUp)
	store.setSegment(2, []byte("segment-2"))
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	select {
	case res := <-followUp:
		if res.err != nil || string(res.data) != "segment-2" {
			t.Fatalf("follow-up request: data=%q err=%v", res.data, res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow-up request was never served")
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if len(coord.enqueued) != 1 || len(coord.releaseCalls) != 0 {
		t.Fatalf("follow-up must reuse the job: enqueued=%d releases=%v", len(coord.enqueued), coord.releaseCalls)
	}
}

func TestSegmentRejoinKeepsExistingClusterJob(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	store := &stubStorage{}
	coord := &stubCoordinator{}
	rec := newJoinRecorder()
	svc := seekTestController(t, store, coord, Options{Observer: rec.observer()})

	cancelFirstWaiter(t, svc, coord)

	second := startSegment(context.Background(), svc, 3)
	awaitJoined(t, rec, second)
	store.setSegment(3, []byte("segment-3"))
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	select {
	case res := <-second:
		if res.err != nil {
			t.Fatalf("rejoined request failed: %v", res.err)
		}
		if string(res.data) != "segment-3" {
			t.Fatalf("expected rejoined request to be served segment 3, got %q", res.data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rejoined request was never served")
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if len(coord.enqueued) != 1 {
		t.Fatalf("rejoin must not enqueue again, got %d", len(coord.enqueued))
	}
	if len(coord.releaseCalls) != 0 {
		t.Fatalf("rejoin must not release the cluster job lock, got %v", coord.releaseCalls)
	}
}

func TestSegmentParkedWaiterIsServedByReadyNotification(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	polled := make(chan struct{}, 64)
	store := &stubStorage{hideSegments: true, onSegmentExists: func() {
		select {
		case polled <- struct{}{}:
		default:
		}
	}}
	coord := &stubCoordinator{}
	rec := newJoinRecorder()
	svc := seekTestController(t, store, coord, Options{Observer: rec.observer(), SegmentTimeout: 10 * time.Second})

	done := startSegment(context.Background(), svc, 1)
	waitForEnqueued(t, coord, 1)
	for len(polled) > 0 {
		<-polled
	}
	select {
	case <-polled:
	case res := <-done:
		t.Fatalf("waiter returned before any notification: data=%q err=%v", res.data, res.err)
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never polled storage, so it is not parked")
	}

	store.setSegment(1, []byte("late-segment"))
	coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})

	select {
	case res := <-done:
		if res.err != nil || string(res.data) != "late-segment" {
			t.Fatalf("parked waiter: data=%q err=%v", res.data, res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parked waiter was not served by the ready notification")
	}
	select {
	case event := <-rec.ready:
		if event.Index != 1 || event.Bytes != int64(len("late-segment")) {
			t.Fatalf("unexpected ready event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected EventSegmentReady for the notified segment")
	}
}

func TestNewControllerWithContextRejectsInvalidOptions(t *testing.T) {
	_, err := NewControllerWithContext(context.Background(), Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, SegmentsPerJob: -1})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected invalid options error, got %v", err)
	}
}

func TestSegmentRejectsInvalidRequestBeforeQueueing(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}, Video: domain.VideoStream{Width: 1920, Height: 1080}}
	metaBytes, _ := json.Marshal(meta)
	coord := &stubCoordinator{}
	svc := NewController(Options{Storage: &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}, Coordinator: coord, PathGen: stubPathGen{}})

	if _, err := svc.Segment(context.Background(), "file:///media", StreamType("bogus"), "1080p", 0); !errors.Is(err, ErrInvalidStreamType) {
		t.Fatalf("expected invalid stream type, got %v", err)
	}
	if _, err := svc.Segment(context.Background(), "file:///media", StreamVideo, "1080p", -1); !errors.Is(err, ErrSegmentOutOfRange) {
		t.Fatalf("expected segment range error, got %v", err)
	}
	if len(coord.enqueued) != 0 {
		t.Fatalf("invalid requests must not enqueue work: %#v", coord.enqueued)
	}
}

func TestMediaKeyChangesWithSourceRevisionAndPipeline(t *testing.T) {
	base := Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, SourceRevision: func(context.Context, string) (string, error) { return "revision-a", nil }}
	first := NewController(base)
	firstKey, err := first.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve first key: %v", err)
	}

	base.SourceRevision = func(context.Context, string) (string, error) { return "revision-b", nil }
	second := NewController(base)
	secondKey, err := second.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve second key: %v", err)
	}
	if firstKey == secondKey {
		t.Fatalf("source revisions must not share a media key")
	}

	base.SourceRevision = func(context.Context, string) (string, error) { return "revision-a", nil }
	base.TargetDuration = 4
	third := NewController(base)
	thirdKey, err := third.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve third key: %v", err)
	}
	if firstKey == thirdKey {
		t.Fatalf("pipeline changes must not share a media key")
	}
}

func TestLocalSourcePathHandlesFilesWithoutParsingRemoteURLs(t *testing.T) {
	for input, want := range map[string]string{
		"/media/video.mkv":             "/media/video.mkv",
		"file:///media/My%20Video.mkv": "/media/My Video.mkv",
		"file://localhost/media/a.mkv": "/media/a.mkv",
	} {
		got, local := localSourcePath(input)
		if !local || got != want {
			t.Fatalf("localSourcePath(%q) = %q, %v; want %q, true", input, got, local, want)
		}
	}
	if _, local := localSourcePath("https://[::1/video?token=secret"); local {
		t.Fatalf("remote URL must not be treated as a local file")
	}
}

func TestInvalidateUsesResolvedMediaKey(t *testing.T) {
	storage := &invalidatingStorage{stubStorage: &stubStorage{}}
	controller := NewController(Options{
		Storage:        storage,
		Coordinator:    &stubCoordinator{},
		PathGen:        stubPathGen{},
		SourceRevision: func(context.Context, string) (string, error) { return "revision", nil },
	})
	want, err := controller.MediaKey(context.Background(), "https://media.example/video")
	if err != nil {
		t.Fatalf("resolve media key: %v", err)
	}
	if err := controller.Invalidate(context.Background(), "https://media.example/video"); err != nil {
		t.Fatalf("invalidate media: %v", err)
	}
	if len(storage.keys) != 1 || storage.keys[0] != want {
		t.Fatalf("invalidated keys %#v, want %q", storage.keys, want)
	}
}

func TestInvalidateReportsUnsupportedStorage(t *testing.T) {
	controller := NewController(Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	if err := controller.Invalidate(context.Background(), "source"); !errors.Is(err, ErrCacheInvalidationUnsupported) {
		t.Fatalf("expected unsupported invalidation error, got %v", err)
	}
}

func TestGeneratedAssetRequestsRejectInvalidConfigurationBeforeProbe(t *testing.T) {
	storage := &stubStorage{}
	controller := NewController(Options{Storage: storage, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	if _, err := controller.Preview(context.Background(), "source", &PreviewConfig{Width: -1}); !errors.Is(err, ErrInvalidPreviewConfig) {
		t.Fatalf("expected invalid preview configuration, got %v", err)
	}
	if _, err := controller.Thumbnail(context.Background(), "source", &ThumbnailConfig{Position: math.NaN()}); !errors.Is(err, ErrInvalidThumbnailConfig) {
		t.Fatalf("expected invalid thumbnail configuration, got %v", err)
	}
	if storage.metaExistsCalls != 0 {
		t.Fatalf("invalid asset requests must not probe storage")
	}
}

func TestAssetIdentityIncludesRequestedConfiguration(t *testing.T) {
	first := assetIdentity("media", "preview", PreviewConfig{MaxFrames: 20, Frameduration: 200, Width: 320})
	second := assetIdentity("media", "preview", PreviewConfig{MaxFrames: 40, Frameduration: 200, Width: 320})
	if first == second {
		t.Fatalf("preview configurations must not share an artifact key")
	}
}

func TestPipelineVersionChangesWithTimelineOffset(t *testing.T) {
	config := &domain.HWAccelConfig{Accelerator: domain.AccelNone}
	base := pipelineIdentity(Options{TargetDuration: 6}, config, 10*time.Second, 1)
	shifted := pipelineIdentity(Options{TargetDuration: 6}, config, 1400*time.Millisecond, 1)
	if base == shifted {
		t.Fatalf("a different output timeline offset must change the pipeline version")
	}
	if pipelineVersion(Options{TargetDuration: 6}, config) != pipelineIdentity(Options{TargetDuration: 6}, config, domain.OutputTimelineOffset, domain.JobContractVersion) {
		t.Fatalf("the pipeline version must be derived from the domain timeline offset and job contract version")
	}
}

func TestPipelineVersionChangesWithJobContractVersion(t *testing.T) {
	config := &domain.HWAccelConfig{Accelerator: domain.AccelNone}
	base := pipelineIdentity(Options{TargetDuration: 6}, config, 10*time.Second, 1)
	bumped := pipelineIdentity(Options{TargetDuration: 6}, config, 10*time.Second, 2)
	if base == bumped {
		t.Fatalf("a different job contract version must change the pipeline version")
	}
}

func TestBuildJobOverlapAndTrailingRules(t *testing.T) {
	controller := &Controller{opts: Options{JobTimeout: time.Minute}, pipelineVersion: "test"}
	segments := []domain.Segment{
		{Index: 0, Start: 0, End: 6},
		{Index: 1, Start: 6, End: 12},
		{Index: 2, Start: 12, End: 18},
		{Index: 3, Start: 18, End: 24},
	}
	direct := domain.VideoRendition{Name: "source", Method: domain.DirectStream}
	transcoded := domain.VideoRendition{Name: "720p", Method: domain.Transcode}
	stereo := domain.AudioRendition{Name: "aac_stereo", Method: domain.Transcode}
	passthrough := domain.AudioRendition{Name: "ac3_passthrough", Method: domain.DirectStream}
	deadline := time.Now().Add(time.Minute)
	meta := testVideoMetadata(24, []float64{0, 6, 12, 18})
	build := func(stream StreamType, video *domain.VideoRendition, audio *domain.AudioRendition, start, end int) domain.Job {
		name := ""
		if video != nil {
			name = video.Name
		} else {
			name = audio.Name
		}
		return controller.buildJob("run", deadline, "source", "key", stream, name, start, start, end, segments, video, audio, meta)
	}

	middle := build(StreamVideo, &direct, nil, 1, 2)
	if middle.Overlap == nil || *middle.Overlap != segments[0] {
		t.Fatalf("copied video must carry the preceding segment as overlap, got %+v", middle.Overlap)
	}
	if middle.Trailing == nil || *middle.Trailing != segments[3] {
		t.Fatalf("copied video must carry the following segment as trailing, got %+v", middle.Trailing)
	}
	first := build(StreamVideo, &direct, nil, 0, 1)
	if first.Overlap != nil || first.Trailing == nil || *first.Trailing != segments[2] {
		t.Fatalf("copied video from segment 0 has no overlap but a trailing segment, got %+v / %+v", first.Overlap, first.Trailing)
	}
	last := build(StreamVideo, &direct, nil, 2, 3)
	if last.Overlap == nil || last.Trailing != nil {
		t.Fatalf("copied video ending at the last segment has no trailing segment, got %+v / %+v", last.Overlap, last.Trailing)
	}

	encoded := build(StreamVideo, &transcoded, nil, 1, 2)
	if encoded.Overlap != nil || encoded.Trailing != nil {
		t.Fatalf("transcoded video must not process discarded neighbours, got %+v / %+v", encoded.Overlap, encoded.Trailing)
	}

	for _, audio := range []domain.AudioRendition{stereo, passthrough} {
		audio := audio
		job := build(StreamAudio, nil, &audio, 1, 2)
		if job.Overlap == nil || *job.Overlap != segments[0] {
			t.Fatalf("%s must carry the preceding segment as overlap, got %+v", audio.Name, job.Overlap)
		}
		if job.Trailing != nil {
			t.Fatalf("%s must not carry a trailing segment, got %+v", audio.Name, job.Trailing)
		}
		if start := build(StreamAudio, nil, &audio, 0, 1); start.Overlap != nil || start.Trailing != nil {
			t.Fatalf("%s from segment 0 has no neighbours, got %+v / %+v", audio.Name, start.Overlap, start.Trailing)
		}
	}
}

func TestBuildJobCarriesOnlyVideoStreamIndex(t *testing.T) {
	controller := &Controller{opts: Options{JobTimeout: time.Minute}, pipelineVersion: "test"}
	segments := []domain.Segment{{Index: 0, Start: 0, End: 6}}
	video := domain.VideoRendition{Name: "720p", Method: domain.Transcode}
	meta := testVideoMetadata(6, []float64{0, 1.5, 3, 4.5})
	meta.Video.Index = 3
	job := controller.buildJob("run", time.Now().Add(time.Minute), "source", "key", StreamVideo, video.Name, 0, 0, 0, segments, &video, nil, meta)
	if job.VideoStreamIndex != 3 {
		t.Fatalf("job must carry the video stream index 3, got %d", job.VideoStreamIndex)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	for _, field := range []string{"Keyframes", "Metadata", "Subtitles"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("job message must not carry probe metadata (%s): %s", field, encoded)
		}
	}
}

func TestHWAccelConfigReturnsIndependentSlices(t *testing.T) {
	svc := NewController(Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	first := svc.HWAccelConfig()
	first.EncodeFlags[0] = "mutated"
	second := svc.HWAccelConfig()
	if second.EncodeFlags[0] == "mutated" {
		t.Fatalf("returned hardware configuration mutated the controller")
	}
}
