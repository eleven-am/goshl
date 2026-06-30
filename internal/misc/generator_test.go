package misc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func spriteURL(index int) string {
	return fmt.Sprintf("http://sprites/%d.jpg", index)
}

func staticSpriteURL(url string) func(int) string {
	return func(int) string {
		return url
	}
}

type stubStorage struct {
	spriteVTTExists bool
	spriteExists    bool
	subtitleExists  bool
	previewExists   bool
	thumbnailExists bool

	existsErr error

	wroteSprites   int
	wroteSpriteVTT int
	wroteSubtitle  int
	wrotePreview   int
	wroteThumbnail int

	spriteVTTData []byte
	spriteData    map[int][]byte
	previewData   []byte
	thumbnailData []byte
}

func (s *stubStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}
func (s *stubStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SetMetadata(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (s *stubStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	return nil
}
func (s *stubStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	return false, nil
}

func (s *stubStorage) WriteSprite(ctx context.Context, mediaID string, index int, data []byte) error {
	s.wroteSprites++
	if s.spriteData == nil {
		s.spriteData = make(map[int][]byte)
	}
	s.spriteData[index] = data
	return nil
}
func (s *stubStorage) ReadSprite(ctx context.Context, mediaID string, index int) ([]byte, error) {
	if s.spriteData != nil {
		return s.spriteData[index], nil
	}
	return nil, nil
}
func (s *stubStorage) SpriteExists(ctx context.Context, mediaID string, index int) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	if s.spriteData != nil {
		_, ok := s.spriteData[index]
		return ok, nil
	}
	return s.spriteExists, nil
}

func (s *stubStorage) WriteSpriteVTT(ctx context.Context, mediaID string, data []byte) error {
	s.wroteSpriteVTT++
	s.spriteVTTData = data
	return nil
}
func (s *stubStorage) ReadSpriteVTT(ctx context.Context, mediaID string) ([]byte, error) {
	return s.spriteVTTData, nil
}
func (s *stubStorage) SpriteVTTExists(ctx context.Context, mediaID string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	return s.spriteVTTExists, nil
}

func (s *stubStorage) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	s.wroteSubtitle++
	return nil
}
func (s *stubStorage) ReadSubtitleVTT(ctx context.Context, mediaID string, lang string) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	return s.subtitleExists, nil
}
func (s *stubStorage) WritePreview(ctx context.Context, sourceURL string, data []byte) error {
	s.wrotePreview++
	s.previewData = data
	return nil
}
func (s *stubStorage) ReadPreview(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.previewData, nil
}
func (s *stubStorage) PreviewExists(ctx context.Context, sourceURL string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	return s.previewExists, nil
}
func (s *stubStorage) WriteThumbnail(ctx context.Context, sourceURL string, data []byte) error {
	s.wroteThumbnail++
	s.thumbnailData = data
	return nil
}
func (s *stubStorage) ReadThumbnail(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.thumbnailData, nil
}
func (s *stubStorage) ThumbnailExists(ctx context.Context, sourceURL string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	return s.thumbnailExists, nil
}

func TestGenerateVTTProducesContinuousEntries(t *testing.T) {
	g := &Generator{thumbWidth: 10, thumbHeight: 10, interval: 1, cols: 2, rows: 2}

	out := string(g.generateVTT(3.0, 2, spriteURL))

	if !strings.Contains(out, "WEBVTT") {
		t.Fatalf("missing header: %s", out)
	}
	if !strings.Contains(out, "00:00:00.000 --> 00:00:01.000") || !strings.Contains(out, "00:00:02.000 --> 00:00:03.000") {
		t.Fatalf("expected sequential cues, got %s", out)
	}
	if strings.Contains(out, "00:00:03.000 --> 00:00:04.000") {
		t.Fatalf("should not exceed duration: %s", out)
	}
}

func TestGetSpriteVTTUsesCacheWhenPresent(t *testing.T) {
	storage := &stubStorage{spriteVTTExists: true, spriteVTTData: []byte("cached")}
	g := NewGenerator(storage)

	data, err := g.GetSpriteVTT(context.Background(), "file:///media", 30, spriteURL)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(data) != "cached" {
		t.Fatalf("expected cached data, got %s", data)
	}
	if storage.wroteSpriteVTT != 0 || storage.wroteSprites != 0 {
		t.Fatalf("should not generate sprites when cached")
	}
}

func TestGetSpriteVTTPropagatesExistenceError(t *testing.T) {
	storage := &stubStorage{existsErr: errors.New("boom")}
	g := NewGenerator(storage)

	_, err := g.GetSpriteVTT(context.Background(), "file:///media", 10, staticSpriteURL("pattern"))
	if err == nil || !strings.Contains(err.Error(), "check sprite vtt") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestFormatVTTTime(t *testing.T) {
	if got := formatVTTTime(3661.789); got != "01:01:01.789" {
		t.Fatalf("unexpected time format: %s", got)
	}
}

func createTestSpriteImage(width, height int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

func TestGetPreviewReturnsCachedData(t *testing.T) {
	storage := &stubStorage{previewExists: true, previewData: []byte("cached-gif")}
	g := NewGenerator(storage)

	data, err := g.GetPreview(context.Background(), "file:///media", 10, spriteURL, domain.PreviewConfig{
		MaxFrames:     10,
		Frameduration: 100,
		Width:         100,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(data) != "cached-gif" {
		t.Fatalf("expected cached data, got %s", data)
	}
	if storage.wrotePreview != 0 {
		t.Fatalf("should not generate preview when cached")
	}
}

func TestGetPreviewPropagatesExistenceError(t *testing.T) {
	storage := &stubStorage{existsErr: errors.New("boom")}
	g := NewGenerator(storage)

	_, err := g.GetPreview(context.Background(), "file:///media", 10, staticSpriteURL("pattern"), domain.PreviewConfig{})
	if err == nil || !strings.Contains(err.Error(), "check preview") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestGeneratePreviewFromSprites(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	spriteImg := createTestSpriteImage(20, 20)
	storage := &stubStorage{
		spriteData:      map[int][]byte{0: spriteImg},
		spriteVTTExists: true,
		spriteVTTData:   []byte("WEBVTT"),
	}
	g.storage = storage

	config := domain.PreviewConfig{
		MaxFrames:     4,
		Frameduration: 100,
		Width:         20,
	}

	preview, err := g.generatePreview(context.Background(), "file:///media", 4, config)
	if err != nil {
		t.Fatalf("generatePreview error: %v", err)
	}

	if len(preview) == 0 {
		t.Fatalf("expected non-empty preview data")
	}

	if !bytes.HasPrefix(preview, []byte("GIF")) {
		t.Fatalf("expected GIF format, got prefix: %v", preview[:min(10, len(preview))])
	}
}

func TestGeneratePreviewSamplesFrames(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        3,
		rows:        3,
		storage:     nil,
	}

	spriteImg := createTestSpriteImage(30, 30)
	storage := &stubStorage{
		spriteData:      map[int][]byte{0: spriteImg},
		spriteVTTExists: true,
		spriteVTTData:   []byte("WEBVTT"),
	}
	g.storage = storage

	config := domain.PreviewConfig{
		MaxFrames:     3,
		Frameduration: 100,
		Width:         10,
	}

	preview, err := g.generatePreview(context.Background(), "file:///media", 9, config)
	if err != nil {
		t.Fatalf("generatePreview error: %v", err)
	}

	if len(preview) == 0 {
		t.Fatalf("expected non-empty preview data")
	}
}

func TestGeneratePreviewNoFramesError(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	storage := &stubStorage{
		spriteData: map[int][]byte{},
	}
	g.storage = storage

	config := domain.PreviewConfig{
		MaxFrames:     4,
		Frameduration: 100,
		Width:         20,
	}

	_, err := g.generatePreview(context.Background(), "file:///media", 0, config)
	if err == nil || !strings.Contains(err.Error(), "no frames") {
		t.Fatalf("expected no frames error, got %v", err)
	}
}

func TestGetSpriteExistenceError(t *testing.T) {
	storage := &stubStorage{existsErr: errors.New("sprite err")}
	g := NewGenerator(storage)

	_, err := g.GetSprite(context.Background(), "file:///media", 10, staticSpriteURL("pattern"), 0)
	if err == nil || !strings.Contains(err.Error(), "check sprite") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestGetSpriteReturnsCachedData(t *testing.T) {
	spriteImg := createTestSpriteImage(20, 20)
	storage := &stubStorage{
		spriteData:      map[int][]byte{0: spriteImg},
		spriteVTTExists: true,
		spriteVTTData:   []byte("WEBVTT"),
	}
	g := NewGenerator(storage)

	data, err := g.GetSprite(context.Background(), "file:///media", 10, spriteURL, 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("expected non-empty sprite data")
	}
}

func TestGeneratePreviewMultipleSprites(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	spriteImg1 := createTestSpriteImage(20, 20)
	spriteImg2 := createTestSpriteImage(20, 20)
	storage := &stubStorage{
		spriteData: map[int][]byte{0: spriteImg1, 1: spriteImg2},
	}
	g.storage = storage

	config := domain.PreviewConfig{
		MaxFrames:     8,
		Frameduration: 100,
		Width:         20,
	}

	preview, err := g.generatePreview(context.Background(), "file:///media", 8, config)
	if err != nil {
		t.Fatalf("generatePreview error: %v", err)
	}

	if len(preview) == 0 {
		t.Fatalf("expected non-empty preview data")
	}
}

func TestGeneratePreviewReadSpriteError(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	storage := &stubStorage{
		spriteData: map[int][]byte{},
	}
	g.storage = storage

	config := domain.PreviewConfig{
		MaxFrames:     4,
		Frameduration: 100,
		Width:         20,
	}

	_, err := g.generatePreview(context.Background(), "file:///media", 4, config)
	if err == nil {
		t.Fatalf("expected error for missing sprite")
	}
}

func TestGetThumbnailReturnsCachedData(t *testing.T) {
	storage := &stubStorage{
		thumbnailExists: true,
		thumbnailData:   []byte("cached-thumbnail"),
	}
	g := NewGenerator(storage)

	data, err := g.GetThumbnail(context.Background(), "file:///media", 60, spriteURL, domain.ThumbnailConfig{Position: 0.1, Width: 480})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "cached-thumbnail" {
		t.Fatalf("expected cached data, got %s", data)
	}
}

func TestGetThumbnailExistsCheckError(t *testing.T) {
	storage := &stubStorage{
		existsErr: errors.New("exists error"),
	}
	g := NewGenerator(storage)

	_, err := g.GetThumbnail(context.Background(), "file:///media", 60, spriteURL, domain.ThumbnailConfig{Position: 0.1, Width: 480})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestGenerateThumbnailSuccess(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	spriteImg := createTestSpriteImage(20, 20)
	storage := &stubStorage{
		spriteData: map[int][]byte{0: spriteImg},
	}
	g.storage = storage

	config := domain.ThumbnailConfig{
		Position: 0.1,
		Width:    20,
	}

	thumbnail, err := g.generateThumbnail(context.Background(), "file:///media", 4, config)
	if err != nil {
		t.Fatalf("generateThumbnail error: %v", err)
	}

	if len(thumbnail) == 0 {
		t.Fatalf("expected non-empty thumbnail data")
	}
}

func TestGenerateThumbnailAllowsZeroPosition(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	spriteImg := createTestSpriteImage(20, 20)
	storage := &stubStorage{
		spriteData: map[int][]byte{0: spriteImg},
	}
	g.storage = storage

	thumbnail, err := g.generateThumbnail(context.Background(), "file:///media", 4, domain.ThumbnailConfig{
		Position: 0,
		Width:    20,
	})
	if err != nil {
		t.Fatalf("generateThumbnail error: %v", err)
	}
	if len(thumbnail) == 0 {
		t.Fatalf("expected non-empty thumbnail data")
	}
}

func TestSpriteFilterUsesHardwareDownloadForGPUFrames(t *testing.T) {
	g := NewGenerator(&stubStorage{}, &domain.HWAccelConfig{
		Accelerator: domain.AccelCUDA,
		ScaleFilter: "scale_cuda=%d:%d:format=nv12",
	})

	filter := g.spriteFilter()
	for _, want := range []string{"scale_cuda=160:90:format=nv12", "hwdownload", "tile=10x10"} {
		if !strings.Contains(filter, want) {
			t.Fatalf("expected %q in filter %q", want, filter)
		}
	}
}

func TestGenerateThumbnailPositionClamping(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	spriteImg := createTestSpriteImage(20, 20)
	storage := &stubStorage{
		spriteData: map[int][]byte{0: spriteImg},
	}
	g.storage = storage

	config := domain.ThumbnailConfig{
		Position: 2.0,
		Width:    20,
	}

	thumbnail, err := g.generateThumbnail(context.Background(), "file:///media", 4, config)
	if err != nil {
		t.Fatalf("generateThumbnail error: %v", err)
	}

	if len(thumbnail) == 0 {
		t.Fatalf("expected non-empty thumbnail data")
	}
}

func TestGenerateThumbnailReadSpriteError(t *testing.T) {
	g := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     nil,
	}

	storage := &stubStorage{
		spriteData: map[int][]byte{},
	}
	g.storage = storage

	config := domain.ThumbnailConfig{
		Position: 0.1,
		Width:    20,
	}

	_, err := g.generateThumbnail(context.Background(), "file:///media", 4, config)
	if err == nil {
		t.Fatalf("expected error for missing sprite")
	}
}
