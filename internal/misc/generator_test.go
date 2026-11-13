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
	spriteExists    bool
	subtitleExists  bool
	previewExists   bool
	thumbnailExists bool

	existsErr error

	wroteSprites   int
	wroteSubtitle  int
	wrotePreview   int
	wroteThumbnail int

	spriteData    map[int][]byte
	defaultSprite []byte
	readSprites   int
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
	s.readSprites++
	if s.spriteData != nil {
		if data, ok := s.spriteData[index]; ok {
			return data, nil
		}
	}
	return s.defaultSprite, nil
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

func TestSpriteVTTHasNoTimestampMapAndRoundsTimes(t *testing.T) {
	g := &Generator{thumbWidth: 10, thumbHeight: 10, interval: 0.7, cols: 2, rows: 2}

	out := string(g.generateVTT(2.1, 1, spriteURL))

	if strings.Contains(out, "X-TIMESTAMP-MAP") {
		t.Fatalf("sprite vtt carries a timestamp map: %s", out)
	}
	for _, want := range []string{"00:00:00.700 --> 00:00:01.400", "00:00:01.400 --> 00:00:02.100"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

func TestGetSpriteVTTRendersFromExistingSprites(t *testing.T) {
	storage := &stubStorage{spriteData: map[int][]byte{0: []byte("img")}}
	g := NewGenerator(storage)

	data, err := g.GetSpriteVTT(context.Background(), "file:///media", "sprite-key", 30, staticSpriteURL("sheet.jpg"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if strings.Count(string(data), "sheet.jpg#xywh=") != 6 {
		t.Fatalf("expected a freshly rendered vtt, got %s", data)
	}
	if storage.wroteSprites != 0 {
		t.Fatalf("should not generate sprites when they exist")
	}
}

func TestGetSpriteVTTPropagatesExistenceError(t *testing.T) {
	storage := &stubStorage{existsErr: errors.New("boom")}
	g := NewGenerator(storage)

	_, err := g.GetSpriteVTT(context.Background(), "file:///media", "sprite-key", 10, staticSpriteURL("pattern"))
	if err == nil || !strings.Contains(err.Error(), "check sprite") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestSpriteIdentityFollowsGeneratorConfig(t *testing.T) {
	g := NewGenerator(&stubStorage{})
	if got := g.SpriteIdentity(); got != "160x90:5:10x10" {
		t.Fatalf("default sprite identity must stay compatible with existing caches, got %q", got)
	}

	variants := map[string]func(*Generator){
		"width":    func(g *Generator) { g.thumbWidth = 320 },
		"height":   func(g *Generator) { g.thumbHeight = 180 },
		"interval": func(g *Generator) { g.interval = 2.5 },
		"cols":     func(g *Generator) { g.cols = 5 },
		"rows":     func(g *Generator) { g.rows = 4 },
	}
	seen := map[string]string{g.SpriteIdentity(): "default"}
	for name, mutate := range variants {
		variant := NewGenerator(&stubStorage{})
		mutate(variant)
		identity := variant.SpriteIdentity()
		if previous, dup := seen[identity]; dup {
			t.Fatalf("%s collides with %s: %q", name, previous, identity)
		}
		seen[identity] = name
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

	data, err := g.GetPreview(context.Background(), "file:///media", "sprite-key", "preview-key", 10, domain.PreviewConfig{
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

	_, err := g.GetPreview(context.Background(), "file:///media", "sprite-key", "preview-key", 10, domain.PreviewConfig{})
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
		spriteData: map[int][]byte{0: spriteImg},
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
		spriteData: map[int][]byte{0: spriteImg},
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

	_, err := g.GetSprite(context.Background(), "file:///media", "sprite-key", 10, 0)
	if err == nil || !strings.Contains(err.Error(), "check sprite") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestGetSpriteReturnsCachedData(t *testing.T) {
	spriteImg := createTestSpriteImage(20, 20)
	storage := &stubStorage{
		spriteData: map[int][]byte{0: spriteImg},
	}
	g := NewGenerator(storage)

	data, err := g.GetSprite(context.Background(), "file:///media", "file:///media", 10, 0)
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

func TestGeneratePreviewReadsAtMostRequestedFramesForLongMedia(t *testing.T) {
	storage := &stubStorage{defaultSprite: createTestSpriteImage(20, 20)}
	generator := &Generator{
		thumbWidth:  10,
		thumbHeight: 10,
		interval:    1,
		cols:        2,
		rows:        2,
		storage:     storage,
	}
	config := domain.PreviewConfig{MaxFrames: 10, Frameduration: 100, Width: 20}
	if _, err := generator.generatePreview(context.Background(), "media-key", 10_000, config); err != nil {
		t.Fatalf("generate sparse preview: %v", err)
	}
	if storage.readSprites > config.MaxFrames {
		t.Fatalf("read %d sprites for %d requested frames", storage.readSprites, config.MaxFrames)
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

	data, err := g.GetThumbnail(context.Background(), "file:///media", "thumbnail-key", 60, domain.ThumbnailConfig{Position: 0.1, Width: 480})
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

	_, err := g.GetThumbnail(context.Background(), "file:///media", "thumbnail-key", 60, domain.ThumbnailConfig{Position: 0.1, Width: 480})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestThumbnailArgsSeekDirectlyToConfiguredPosition(t *testing.T) {
	g := NewGenerator(&stubStorage{})
	args, err := g.thumbnailArgs("file:///media", 100, domain.ThumbnailConfig{Position: 0.25, Width: 480})
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-ss 25.000000", "-frames:v 1", "scale=480:-2", "pipe:1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in %q", want, joined)
		}
	}
}

func TestSpriteFilterUsesHardwareDownloadForGPUFrames(t *testing.T) {
	g := NewGenerator(&stubStorage{}, &domain.HWAccelConfig{
		Accelerator:  domain.AccelCUDA,
		ScaleFilter:  "scale_cuda=%d:%d:format=nv12",
		DeviceFrames: true,
	})

	filter := g.spriteFilter(100)
	for _, want := range []string{"scale_cuda=160:90:format=nv12", "hwdownload", "tile=10x10"} {
		if !strings.Contains(filter, want) {
			t.Fatalf("expected %q in filter %q", want, filter)
		}
	}
}

func TestSpriteArgsLimitCUDAThreads(t *testing.T) {
	g := NewGenerator(&stubStorage{}, &domain.HWAccelConfig{
		Accelerator:  domain.AccelCUDA,
		DecodeFlags:  []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"},
		ScaleFilter:  "scale_cuda=%d:%d:format=nv12",
		DeviceFrames: true,
	})

	args, err := g.spriteArgs("input.mp4", "sprite-%d.jpg", 100)
	if err != nil {
		t.Fatalf("args: %v", err)
	}

	threadsIndex := indexOf(args, "-threads")
	inputIndex := indexOf(args, "-i")
	if threadsIndex == -1 || threadsIndex+1 >= len(args) || args[threadsIndex+1] != "1" {
		t.Fatalf("expected cuda sprite args to include -threads 1, got %v", args)
	}
	if inputIndex == -1 || threadsIndex > inputIndex {
		t.Fatalf("expected -threads before input, got %v", args)
	}
}

func TestSpriteArgsDoesNotLimitCPUThreads(t *testing.T) {
	g := NewGenerator(&stubStorage{})

	args, err := g.spriteArgs("input.mp4", "sprite-%d.jpg", 100)
	if err != nil {
		t.Fatalf("args: %v", err)
	}

	if indexOf(args, "-threads") != -1 {
		t.Fatalf("expected cpu sprite args not to include -threads, got %v", args)
	}
}

func indexOf(values []string, value string) int {
	for i, candidate := range values {
		if candidate == value {
			return i
		}
	}
	return -1
}
