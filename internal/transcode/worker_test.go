package transcode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestWorkerDropsOverlapSegmentAndUploadsRemaining(t *testing.T) {
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
	w := NewWorker(WorkerSpec{Args: []string{"--emit", files[0], files[1]}, Storage: storage, MediaKey: "file:///source", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{2}, Overlap: segmentNumber(1)})

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
			t.Fatalf("worker did not finish before context deadline, state %v err %v", state, w.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if w.State() != WorkerStateDone {
		t.Fatalf("worker did not finish, state %v err %v", w.State(), w.Err())
	}

	if len(storage.writes) != 1 {
		t.Fatalf("expected only the non-overlap segment written, got %d", len(storage.writes))
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

func segmentNumber(index int) *int {
	return &index
}

func installFakeFFmpeg(t *testing.T, script string) string {
	t.Helper()
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "ffmpeg"), []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tmp
}

func awaitWorker(t *testing.T, w *Worker, limit time.Duration) {
	t.Helper()
	select {
	case <-w.Done():
	case <-time.After(limit):
		t.Fatalf("worker did not finish within %v, state %v err %v", limit, w.State(), w.Err())
	}
}

func TestWorkerStartRejectsEmptyExpectedIndices(t *testing.T) {
	tmp := installFakeFFmpeg(t, fakeFFmpegScript)
	w := NewWorker(WorkerSpec{Args: []string{"--emit", "segment-00001.ts"}, Storage: &memoryStorage{}, MediaKey: "media", Rendition: "1080p", IsVideo: true, TmpDir: tmp})

	if err := w.Start(context.Background()); !errors.Is(err, ErrNoExpectedSegments) {
		t.Fatalf("worker without expected indices must refuse to start, got %v", err)
	}
	if w.State() != WorkerStateIdle {
		t.Fatalf("refused worker must stay idle, got %v", w.State())
	}
}

func TestWorkerKill(t *testing.T) {
	tmp := installFakeFFmpeg(t, "#!/bin/sh\nexec sleep 10\n")
	w := NewWorker(WorkerSpec{Args: []string{}, Storage: &memoryStorage{}, MediaKey: "file:///source", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{1}})

	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if w.State() != WorkerStateRunning {
		t.Fatalf("worker must be running before the kill, state %v", w.State())
	}
	w.Kill()
	awaitWorker(t, w, 2*time.Second)

	if w.State() != WorkerStateError || !errors.Is(w.Err(), context.Canceled) {
		t.Fatalf("killed worker must report cancellation, state %v err %v", w.State(), w.Err())
	}
}

func TestWorkerDropsOverlapAndTrailingByIndex(t *testing.T) {
	tmp := installFakeFFmpeg(t, fakeFFmpegScript)
	names := []string{"segment-00005.ts", "segment-00002.ts", "segment-00003.ts", "segment-00004.ts"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("data"), 0644); err != nil {
			t.Fatalf("prime file: %v", err)
		}
	}
	storage := &memoryStorage{}
	w := NewWorker(WorkerSpec{Args: append([]string{"--emit"}, names...), Storage: storage, MediaKey: "media", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{3, 4}, Overlap: segmentNumber(2), Trailing: segmentNumber(5)})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	awaitWorker(t, w, 10*time.Second)

	if w.State() != WorkerStateDone {
		t.Fatalf("worker failed: %v", w.Err())
	}
	if len(storage.writes) != 2 || storage.writes[0].Index != 3 || storage.writes[1].Index != 4 {
		t.Fatalf("only segments 3 and 4 may be stored, stored %#v", storage.writes)
	}
	for _, name := range []string{"segment-00005.ts", "segment-00002.ts"} {
		if _, err := os.Stat(filepath.Join(tmp, name)); !os.IsNotExist(err) {
			t.Fatalf("neighbour file %s must be deleted, stat error %v", name, err)
		}
	}
}

func TestWorkerAcceptsMissingNeighbourFilesAndRejectsOtherIndexes(t *testing.T) {
	tmp := installFakeFFmpeg(t, fakeFFmpegScript)
	for _, name := range []string{"segment-00003.ts", "segment-00006.ts"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("data"), 0644); err != nil {
			t.Fatalf("prime file: %v", err)
		}
	}
	complete := NewWorker(WorkerSpec{Args: []string{"--emit", "segment-00003.ts"}, Storage: &memoryStorage{}, MediaKey: "media", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{3}, Overlap: segmentNumber(2), Trailing: segmentNumber(4)})
	if err := complete.Start(context.Background()); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	awaitWorker(t, complete, 10*time.Second)
	if complete.State() != WorkerStateDone {
		t.Fatalf("a job whose neighbour files were never produced must still complete: %v", complete.Err())
	}

	if err := os.WriteFile(filepath.Join(tmp, "segment-00003.ts"), []byte("data"), 0644); err != nil {
		t.Fatalf("prime file: %v", err)
	}
	stray := NewWorker(WorkerSpec{Args: []string{"--emit", "segment-00003.ts", "segment-00006.ts"}, Storage: &memoryStorage{}, MediaKey: "media", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{3}, Overlap: segmentNumber(2), Trailing: segmentNumber(4)})
	if err := stray.Start(context.Background()); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	awaitWorker(t, stray, 10*time.Second)
	if stray.State() != WorkerStateError || stray.Err() == nil || !strings.Contains(stray.Err().Error(), "unexpected segment 6") {
		t.Fatalf("an index outside the job must fail it, state %v err %v", stray.State(), stray.Err())
	}
}

type cancellingStorage struct {
	memoryStorage
	cancel context.CancelFunc
	last   int
}

func (s *cancellingStorage) WriteSegment(ctx context.Context, info domain.SegmentData, data []byte) error {
	s.writes = append(s.writes, info)
	if info.Index == s.last {
		time.Sleep(300 * time.Millisecond)
		s.cancel()
	}
	return nil
}

func TestWorkerCompletesWhenCancelledAfterAllSegmentsUploaded(t *testing.T) {
	tmp := installFakeFFmpeg(t, fakeFFmpegScript)
	names := []string{"segment-00001.ts", "segment-00002.ts"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("data"), 0644); err != nil {
			t.Fatalf("prime file: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := &cancellingStorage{cancel: cancel, last: 2}
	w := NewWorker(WorkerSpec{Args: append([]string{"--emit"}, names...), Storage: storage, MediaKey: "media", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{1, 2}})
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	awaitWorker(t, w, 10*time.Second)
	if w.State() != WorkerStateDone {
		t.Fatalf("all segments were uploaded and ffmpeg exited 0, yet state %v err %v", w.State(), w.Err())
	}
}

func TestWorkerReportsErrorOnlyAfterProcessExit(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		tmp := t.TempDir()
		pidFile := filepath.Join(tmp, "pid")
		script := "#!/bin/sh\necho $$ > " + pidFile + "\necho not-a-segment.ts\nexec sleep 10\n"
		if err := os.WriteFile(filepath.Join(tmp, "ffmpeg"), []byte(script), 0755); err != nil {
			t.Fatalf("write script: %v", err)
		}
		t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
		w := NewWorker(WorkerSpec{Storage: &memoryStorage{}, MediaKey: "media", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{1}})
		if err := w.Start(context.Background()); err != nil {
			t.Fatalf("start failed: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for w.State() != WorkerStateError {
			if time.Now().After(deadline) {
				t.Fatalf("worker never reported the malformed entry, state %v", w.State())
			}
		}
		data, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatalf("read pid: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatalf("parse pid: %v", err)
		}
		if syscall.Kill(pid, 0) == nil {
			t.Fatalf("attempt %d: worker reported an error while ffmpeg (pid %d) was still running", attempt, pid)
		}
		select {
		case <-w.Done():
		default:
			t.Fatalf("attempt %d: worker state is final but Done is not closed", attempt)
		}
		if w.Err() == nil || !strings.Contains(w.Err().Error(), "invalid segment filename") {
			t.Fatalf("expected the malformed entry error, got %v", w.Err())
		}
	}
}

func TestWorkerErrMethod(t *testing.T) {
	w := &Worker{}
	if w.Err() != nil {
		t.Fatalf("expected nil error for new worker")
	}
}

func TestWorkerUploadsAllExpectedSegmentsWithoutNeighbours(t *testing.T) {
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
	w := NewWorker(WorkerSpec{Args: []string{"--emit", files[0], files[1]}, Storage: storage, MediaKey: "file:///source", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{1, 2}})

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
		t.Fatalf("expected both expected segments written, got %d", len(storage.writes))
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
	w := NewWorker(WorkerSpec{Args: []string{"--emit", files[0]}, Storage: storage, MediaKey: "file:///source", Rendition: "aac_stereo", IsVideo: false, TmpDir: tmp, Expected: []int{1}})

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

	w := NewWorker(WorkerSpec{Args: []string{"--emit"}, Storage: &memoryStorage{}, MediaKey: "file:///source", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{1}})
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
	w := NewWorker(WorkerSpec{Args: []string{"--emit", filename}, Storage: storage, MediaKey: "file:///source", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{7}})
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

	w := NewWorker(WorkerSpec{Args: []string{"--emit", filename}, Storage: &memoryStorage{}, MediaKey: "file:///source", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{1}})
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

	w := NewWorker(WorkerSpec{Args: []string{"--emit", filename}, Storage: &memoryStorage{}, MediaKey: "media-key", Rendition: "1080p", IsVideo: true, TmpDir: tmp, Expected: []int{1, 2}})
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
	args, err := builder.Video(ffmpeg.VideoParams{
		InputURL:    input,
		StreamIndex: 0,
		Rendition:   domain.VideoRendition{Method: domain.Transcode, Width: 320, Height: 180, Bitrate: 500_000, PeakBitrate: 750000, BufferSize: 1500000, Level: 30},
		Segments: []domain.Segment{
			{Index: 0, Start: 0, End: 2, Duration: 2},
			{Index: 1, Start: 2, End: 4, Duration: 2},
		},
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	storage := &memoryStorage{}
	worker := NewWorker(WorkerSpec{Args: args, Storage: storage, MediaKey: "media-key", Rendition: "180p", IsVideo: true, TmpDir: outputDir, Expected: []int{0, 1}})
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
	select {
	case <-w.Done():
	case <-ctx.Done():
		t.Fatalf("worker did not finish before context deadline, state %v err %v", w.State(), w.Err())
	}
}
