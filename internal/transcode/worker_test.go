package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
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
func (m *memoryStorage) WritePreview(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (m *memoryStorage) ReadPreview(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (m *memoryStorage) PreviewExists(ctx context.Context, sourceURL string) (bool, error) {
	return false, nil
}
func (m *memoryStorage) WriteThumbnail(ctx context.Context, sourceURL string, data []byte) error {
	return nil
}
func (m *memoryStorage) ReadThumbnail(ctx context.Context, sourceURL string) ([]byte, error) {
	return nil, nil
}
func (m *memoryStorage) ThumbnailExists(ctx context.Context, sourceURL string) (bool, error) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	if storage.writes[0].Index != 2 || storage.writes[0].MediaKey != "file:///source" {
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

func TestWorkerKill(t *testing.T) {
	tmp := t.TempDir()

	script := filepath.Join(tmp, "ffmpeg")
	sleepScript := `#!/bin/sh
sleep 10
`
	if err := os.WriteFile(script, []byte(sleepScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	storage := &memoryStorage{}
	w := NewWorker([]string{}, storage, "file:///source", "1080p", true, tmp, false)

	ctx := context.Background()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	w.Kill()

	time.Sleep(100 * time.Millisecond)
	state := w.State()
	if state != WorkerStateError && state != WorkerStateDone {
		t.Logf("worker state after kill: %v", state)
	}
}

func TestWorkerErrMethod(t *testing.T) {
	w := &Worker{}
	if w.Err() != nil {
		t.Fatalf("expected nil error for new worker")
	}
}

func TestWorkerNoSkipFirst(t *testing.T) {
	tmp := t.TempDir()

	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

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
	w := NewWorker([]string{"--emit", files[0], files[1]}, storage, "file:///source", "1080p", true, tmp, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	for {
		state := w.State()
		if state == WorkerStateDone || state == WorkerStateError {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("worker did not finish before context deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if w.State() != WorkerStateDone {
		t.Fatalf("worker did not finish, state %v err %v", w.State(), w.Err())
	}

	if len(storage.writes) != 2 {
		t.Fatalf("expected 2 segments written (no skip), got %d", len(storage.writes))
	}
}

func TestWorkerAudioSegments(t *testing.T) {
	tmp := t.TempDir()

	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	files := []string{"segment-00001.ts"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte("audiodata"), 0644); err != nil {
			t.Fatalf("prime file: %v", err)
		}
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	storage := &memoryStorage{}
	w := NewWorker([]string{"--emit", files[0]}, storage, "file:///source", "aac_stereo", false, tmp, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	for {
		state := w.State()
		if state == WorkerStateDone || state == WorkerStateError {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("worker did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(storage.writes) != 1 {
		t.Fatalf("expected 1 audio segment written, got %d", len(storage.writes))
	}
	if storage.writes[0].IsVideo {
		t.Fatalf("expected audio segment, got video")
	}
}

func TestWorkerFailsWhenFFmpegProducesNoSegments(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	w := NewWorker([]string{"--emit"}, &memoryStorage{}, "file:///source", "1080p", true, tmp, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	waitForWorker(t, ctx, w)

	if w.State() != WorkerStateError || w.Err() == nil || w.Err().Error() != "ffmpeg produced no segments" {
		t.Fatalf("expected missing segment error, state %v err %v", w.State(), w.Err())
	}
}

func TestWorkerRejectsEmptySegment(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	filename := "segment-00007.ts"
	if err := os.WriteFile(filepath.Join(tmp, filename), nil, 0644); err != nil {
		t.Fatalf("prime file: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	storage := &memoryStorage{}
	w := NewWorker([]string{"--emit", filename}, storage, "file:///source", "1080p", true, tmp, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	waitForWorker(t, ctx, w)

	if w.State() != WorkerStateError || w.Err() == nil || w.Err().Error() != "segment 7 is empty" {
		t.Fatalf("expected empty segment error, state %v err %v", w.State(), w.Err())
	}
	if len(storage.writes) != 0 {
		t.Fatalf("expected no empty segment writes, got %d", len(storage.writes))
	}
}

func TestWorkerRejectsMalformedSegmentListEntry(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	filename := "not-a-segment.ts"
	if err := os.WriteFile(filepath.Join(tmp, filename), []byte("data"), 0644); err != nil {
		t.Fatalf("prime file: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	w := NewWorker([]string{"--emit", filename}, &memoryStorage{}, "file:///source", "1080p", true, tmp, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	waitForWorker(t, ctx, w)
	if w.State() != WorkerStateError || w.Err() == nil || !strings.Contains(w.Err().Error(), "invalid segment filename") {
		t.Fatalf("expected malformed filename error, state %v err %v", w.State(), w.Err())
	}
}

func TestWorkerRejectsPartialExpectedBatch(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte(fakeFFmpegScript), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	filename := "segment-00001.ts"
	if err := os.WriteFile(filepath.Join(tmp, filename), []byte("data"), 0644); err != nil {
		t.Fatalf("prime file: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+origPath)

	w := NewWorker([]string{"--emit", filename}, &memoryStorage{}, "media-key", "1080p", true, tmp, false, []int{1, 2})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	waitForWorker(t, ctx, w)
	if w.State() != WorkerStateError || w.Err() == nil || !strings.Contains(w.Err().Error(), "expected segment 2") {
		t.Fatalf("expected partial batch error, state %v err %v", w.State(), w.Err())
	}
}

func TestWorkerStoresCompleteRealFFmpegBatch(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	tmp := t.TempDir()
	input := filepath.Join(tmp, "nonzero.ts")
	create := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-t", "4", "-c:v", "libx264", "-g", "25", "-keyint_min", "25", "-sc_threshold", "0",
		"-f", "mpegts", input,
	)
	if data, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, data)
	}
	outputDir := filepath.Join(tmp, "segments")
	if err := os.Mkdir(outputDir, 0755); err != nil {
		t.Fatalf("create segment directory: %v", err)
	}
	builder := ffmpeg.NewCommandBuilder(&domain.HWAccelConfig{
		Accelerator:  domain.AccelNone,
		EncodeFlags:  []string{"-c:v", "libx264"},
		KeyframeFlag: "-force_key_frames",
		ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
	})
	args := builder.Video(ffmpeg.VideoParams{
		InputURL:    input,
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 500_000},
		Segments: []domain.Segment{
			{Index: 0, Start: 0, End: 2, Duration: 2},
			{Index: 1, Start: 2, End: 4, Duration: 2},
		},
		OutputDir: outputDir,
	})
	storage := &memoryStorage{}
	worker := NewWorker(args, storage, "media-key", "180p", true, outputDir, false, []int{0, 1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	waitForWorker(t, ctx, worker)
	if worker.State() != WorkerStateDone {
		t.Fatalf("real FFmpeg worker failed: %v", worker.Err())
	}
	if len(storage.writes) != 2 || storage.writes[0].Index != 0 || storage.writes[1].Index != 1 {
		t.Fatalf("unexpected stored batch: %#v", storage.writes)
	}
}

func waitForWorker(t *testing.T, ctx context.Context, w *Worker) {
	t.Helper()
	for {
		state := w.State()
		if state == WorkerStateDone || state == WorkerStateError {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("worker did not finish before context deadline, state %v err %v", state, w.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
