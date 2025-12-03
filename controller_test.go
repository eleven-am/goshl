package goshl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type stubStorage struct {
	metaData     []byte
	metaExists   bool
	segments     map[int][]byte
	segmentCalls int
	subtitleData []byte
	spriteVTT    []byte
}

func (s *stubStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
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

type stubCoordinator struct {
	enqueued []domain.Job
	subCh    chan domain.Job
	waitCh   chan domain.SegmentStatus
}

func (c *stubCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	c.enqueued = append(c.enqueued, job)
	return nil
}
func (c *stubCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType) (<-chan domain.Job, error) {
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
	if c.waitCh == nil {
		c.waitCh = make(chan domain.SegmentStatus, 1)
	}
	return c.waitCh, nil
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
func (stubPathGen) SpriteVTT(sourceURL string) string                { return "/sprites.vtt" }
func (stubPathGen) Sprite(sourceURL string, index int) string        { return "/sprite" }
func (stubPathGen) SubtitleVTT(sourceURL string, lang string) string { return "/sub.vtt" }

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
		coord.waitCh <- domain.SegmentStatus{State: domain.SegmentStateReady}
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
