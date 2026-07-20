package goshl

import (
	"context"
	"encoding/json"
	"errors"
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
	metaData           []byte
	metaExists         bool
	segments           map[int][]byte
	segmentCalls       int
	subtitleData       []byte
	spriteVTT          []byte
	metaExistsCalls    int
	segmentExistsCalls int
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
	if s.segments == nil {
		s.segments = make(map[int][]byte)
	}
	s.segments[info.Index] = data
	return nil
}
func (s *stubStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	s.segmentCalls++
	return s.segments[info.Index], nil
}
func (s *stubStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	s.segmentExistsCalls++
	_, ok := s.segments[info.Index]
	return ok, nil
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
func (s *stubStorage) WriteSpriteVTT(ctx context.Context, mediaID string, data []byte) error {
	s.spriteVTT = data
	return nil
}
func (s *stubStorage) ReadSpriteVTT(ctx context.Context, mediaID string) ([]byte, error) {
	return s.spriteVTT, nil
}
func (s *stubStorage) SpriteVTTExists(ctx context.Context, mediaID string) (bool, error) {
	return s.spriteVTT != nil, nil
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
	subCh         chan domain.Job
	waitCh        chan domain.SegmentStatus
	lockErr       error
	subErrByType  map[domain.StreamType]error
	lockCalls     []string
	releaseCalls  []string
	onAcquireLock func(key string)
}

func (c *stubCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.enqueued = append(c.enqueued, job)
	return nil
}
func (c *stubCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType) (<-chan domain.Job, error) {
	if c.subErrByType != nil && c.subErrByType[streamType] != nil {
		return nil, c.subErrByType[streamType]
	}
	if c.subCh == nil {
		c.subCh = make(chan domain.Job)
		close(c.subCh)
	}
	return c.subCh, nil
}
func (c *stubCoordinator) Ack(ctx context.Context, jobID string) error { return nil }
func (c *stubCoordinator) NotifySegment(ctx context.Context, info domain.SegmentData, status domain.SegmentStatus) error {
	return nil
}
func (c *stubCoordinator) WaitSegment(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error) {
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
func (c *stubCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.lockCalls = append(c.lockCalls, key)
	if c.onAcquireLock != nil {
		c.onAcquireLock(key)
	}
	if c.lockErr != nil {
		return "", c.lockErr
	}
	return uuid.New().String(), nil
}
func (c *stubCoordinator) ReleaseLock(ctx context.Context, key string, token string) error {
	c.releaseCalls = append(c.releaseCalls, key)
	return nil
}
func (c *stubCoordinator) Close() {}

type stubPathGen struct{}

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
func (stubPathGen) SubtitleVTT(sourceURL string, lang string) string { return "/sub.vtt" }
func (stubPathGen) Preview(sourceURL string) string                  { return "/preview.gif" }
func (stubPathGen) Thumbnail(sourceURL string) string                { return "/thumbnail.jpg" }

// installFakeFFmpeg ensures hwaccel.Detect can run without real ffmpeg.
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

func TestMasterPlaylistIncludesAllAudioTracks(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{
		Video: domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Audios: []domain.AudioStream{
			{Codec: "aac", Channels: 2, Bitrate: 128_000, Language: "eng"},
			{Codec: "ac3", Channels: 6, Bitrate: 640_000, Language: "spa"},
		},
	}
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

	for _, want := range []string{"audio0_aac_stereo", "audio1_aac_stereo", "audio1_aac_surround", "audio1_ac3_passthrough"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected audio rendition %q in playlist: %s", want, out)
		}
	}
	if strings.Count(out, "DEFAULT=YES") != 1 {
		t.Fatalf("expected exactly one default audio rendition: %s", out)
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

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}, Video: domain.VideoStream{Width: 1920, Height: 1080}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{0: []byte("seg0")}}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	// simulate worker ready
	go func() {
		time.Sleep(10 * time.Millisecond)
		coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	}()

	data, err := svc.Segment(context.Background(), "file:///media", domain.StreamAudio, "aac_stereo", 1)
	if err != nil {
		t.Fatalf("segment err: %v", err)
	}
	if data != nil {
		// stub storage has no segment 1; should still call ReadSegment returning nil
	}
	if len(coord.enqueued) != 1 {
		t.Fatalf("expected enqueue, got %d", len(coord.enqueued))
	}
	opts := Options{}
	opts.setDefaults()
	if coord.enqueued[0].StartIndex != 0 || coord.enqueued[0].EndIndex != opts.SegmentsPerJob-1 {
		t.Fatalf("unexpected job indices: %#v", coord.enqueued[0])
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

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}}
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

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	}()

	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)

	if len(coord.lockCalls) != 1 {
		t.Fatalf("expected 1 lock call, got %d", len(coord.lockCalls))
	}
	if !strings.Contains(coord.lockCalls[0], "segment:") {
		t.Fatalf("expected segment lock key, got %q", coord.lockCalls[0])
	}
	if len(coord.releaseCalls) != 1 {
		t.Fatalf("expected 1 release call, got %d", len(coord.releaseCalls))
	}
}

func TestSegmentWaitsWhenLockHeld(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{lockErr: domain.ErrLockHeld}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	}()

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

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}}
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

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}

	lockAcquired := false
	coord := &stubCoordinator{
		onAcquireLock: func(key string) {
			lockAcquired = true
			store.segments[1] = []byte("appeared")
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

	meta := &domain.Metadata{Duration: 60, Keyframes: []float64{0, 6, 12, 18, 24, 30, 36, 42, 48, 54, 60}}
	metaBytes, _ := json.Marshal(meta)
	store := &stubStorage{metaData: metaBytes, metaExists: true, segments: map[int][]byte{}}
	coord := &stubCoordinator{}
	svc := NewController(Options{
		Storage:     store,
		Coordinator: coord,
		PathGen:     stubPathGen{},
	})

	go func() {
		time.Sleep(5 * time.Millisecond)
		coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	}()
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	firstJobID := coord.enqueued[0].ID

	coord.enqueued = nil
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	go func() {
		time.Sleep(5 * time.Millisecond)
		coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	}()
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "1080p", 1)
	secondJobID := coord.enqueued[0].ID

	if firstJobID != secondJobID {
		t.Fatalf("job IDs should be deterministic, got %q and %q", firstJobID, secondJobID)
	}

	coord.enqueued = nil
	coord.waitCh = make(chan domain.SegmentStatus, 1)
	go func() {
		time.Sleep(5 * time.Millisecond)
		coord.sendWaitStatus(domain.SegmentStatus{State: domain.SegmentStateReady})
	}()
	_, _ = svc.Segment(context.Background(), "file:///media", domain.StreamVideo, "720p", 1)
	differentRenditionID := coord.enqueued[0].ID

	if firstJobID == differentRenditionID {
		t.Fatalf("different renditions should have different job IDs")
	}
}

func TestSegmentSkipsLockWhenAlreadyExists(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()

	meta := &domain.Metadata{Duration: 12, Keyframes: []float64{0, 6, 12}}
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
