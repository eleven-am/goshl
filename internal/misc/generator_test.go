package misc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

type stubStorage struct {
	spriteVTTExists bool
	spriteExists    bool
	subtitleExists  bool

	existsErr error

	wroteSprites   int
	wroteSpriteVTT int
	wroteSubtitle  int

	spriteVTTData []byte
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
	return nil
}
func (s *stubStorage) ReadSprite(ctx context.Context, mediaID string, index int) ([]byte, error) {
	return nil, nil
}
func (s *stubStorage) SpriteExists(ctx context.Context, mediaID string, index int) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
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

func TestGenerateVTTProducesContinuousEntries(t *testing.T) {
	g := &Generator{thumbWidth: 10, thumbHeight: 10, interval: 1, cols: 2, rows: 2}

	out := string(g.generateVTT(3.0, 2, "http://sprites/%d.jpg"))

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

	data, err := g.GetSpriteVTT(context.Background(), "file:///media", 30, "http://sprites/%d.jpg")
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

	_, err := g.GetSpriteVTT(context.Background(), "file:///media", 10, "pattern")
	if err == nil || !strings.Contains(err.Error(), "check sprite vtt") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestFormatVTTTime(t *testing.T) {
	if got := formatVTTTime(3661.789); got != "01:01:01.789" {
		t.Fatalf("unexpected time format: %s", got)
	}
}
