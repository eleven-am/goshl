package transcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type memoryStorage struct {
	writes []domain.SegmentData
}

func (m *memoryStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}
func (m *memoryStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (m *memoryStorage) SetMetadata(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (m *memoryStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	m.writes = append(m.writes, info)
	return nil
}
func (m *memoryStorage) ReadSegment(ctx context.Context, info domain.SegmentData) ([]byte, error) {
	return nil, nil
}
func (m *memoryStorage) SegmentExists(ctx context.Context, info domain.SegmentData) (bool, error) {
	return false, nil
}
func (m *memoryStorage) WriteSprite(ctx context.Context, mediaID string, index int, data []byte) error {
	return nil
}
func (m *memoryStorage) ReadSprite(ctx context.Context, mediaID string, index int) ([]byte, error) {
	return nil, nil
}
func (m *memoryStorage) SpriteExists(ctx context.Context, mediaID string, index int) (bool, error) {
	return false, nil
}
func (m *memoryStorage) WriteSpriteVTT(ctx context.Context, mediaID string, data []byte) error {
	return nil
}
func (m *memoryStorage) ReadSpriteVTT(ctx context.Context, mediaID string) ([]byte, error) {
	return nil, nil
}
func (m *memoryStorage) SpriteVTTExists(ctx context.Context, mediaID string) (bool, error) {
	return false, nil
}
func (m *memoryStorage) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	return nil
}
func (m *memoryStorage) ReadSubtitleVTT(ctx context.Context, mediaID string, lang string) ([]byte, error) {
	return nil, nil
}
func (m *memoryStorage) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	return false, nil
}

func TestWorkerUploadsSegmentsAndSkipsFirstWhenConfigured(t *testing.T) {
	tmp := t.TempDir()

	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Pre-create files the fake ffmpeg will reference.
	files := []string{"segment-00001.ts", "segment-00002.ts"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte("data"), 0644); err != nil {
			t.Fatalf("prime file: %v", err)
		}
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	storage := &memoryStorage{}
	w := NewWorker([]string{"--emit", files[0], files[1]}, storage, "file:///source", "1080p", true, tmp, true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// wait for completion bounded by context deadline
	for {
		state := w.State()
		if state == WorkerStateDone || state == WorkerStateError {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("worker did not finish before context deadline, state %v err %v", state, w.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if w.State() != WorkerStateDone {
		t.Fatalf("worker did not finish, state %v err %v", w.State(), w.Err())
	}

	if len(storage.writes) != 1 {
		t.Fatalf("expected only one segment written (skipping first), got %d", len(storage.writes))
	}
	if storage.writes[0].Index != 2 || storage.writes[0].SourceURL != "file:///source" {
		t.Fatalf("unexpected written segment info: %#v", storage.writes[0])
	}
}

const fakeFFmpegScript = `#!/bin/sh
if [ "$1" = "--emit" ]; then
  shift
  for f in "$@"; do
    echo "$f"
  done
  exit 0
fi
echo "unexpected args: $@" >&2
exit 1
`
