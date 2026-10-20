package goshl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type keyedSubtitleStorage struct {
	*stubStorage
	mu    sync.Mutex
	items map[string][]byte
}

func (s *keyedSubtitleStorage) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string][]byte{}
	}
	s.items[mediaID+"|"+lang] = data
	return nil
}

func (s *keyedSubtitleStorage) ReadSubtitleVTT(ctx context.Context, mediaID string, lang string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[mediaID+"|"+lang], nil
}

func (s *keyedSubtitleStorage) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.items[mediaID+"|"+lang]
	return ok, nil
}

func installSubtitleFFmpeg(t *testing.T) (logPath, pidPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	pidPath = filepath.Join(dir, "pid")
	script := `#!/bin/sh
if [ "$1" = "-hwaccels" ]; then
echo "Hardware acceleration methods:"; echo cuda; exit 0; fi
if [ "$1" = "-encoders" ]; then
echo "h264_nvenc"; exit 0; fi
echo "$*" >> "$FAKE_LOG"
if [ -n "$FAKE_HANG" ]; then
echo $$ > "$FAKE_PID"
exec sleep 30
fi
for a; do
case "$a" in 0:3) exit 1;; esac
done
prev=""; m=""
for a; do
case "$prev" in -map) m=$a;; esac
case "$a" in *.vtt) echo "WEBVTT stream $m" > "$a";; esac
prev=$a
done
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", logPath)
	t.Setenv("FAKE_PID", pidPath)
	return logPath, pidPath
}

func newSubtitleController(t *testing.T, subs []domain.SubtitleStream) *Controller {
	t.Helper()
	return newSubtitleControllerWithTimeout(t, subs, 0)
}

func newSubtitleControllerWithTimeout(t *testing.T, subs []domain.SubtitleStream, generationTimeout time.Duration) *Controller {
	t.Helper()
	metaBytes, _ := json.Marshal(&domain.Metadata{Subtitles: subs})
	return NewController(Options{
		Storage:           &keyedSubtitleStorage{stubStorage: &stubStorage{metaData: metaBytes, metaExists: true}},
		Coordinator:       &stubCoordinator{},
		PathGen:           stubPathGen{},
		GenerationTimeout: generationTimeout,
	})
}

func textTracks(indexes ...int) []domain.SubtitleStream {
	subs := make([]domain.SubtitleStream, 0, len(indexes))
	for _, idx := range indexes {
		subs = append(subs, domain.SubtitleStream{Index: idx, ID: "t" + strconv.Itoa(idx), Codec: "subrip", Language: "en"})
	}
	return subs
}

func ffmpegCalls(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestSubtitlesVTTUsesOneFFmpegForAllTracks(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	svc := newSubtitleController(t, textTracks(2, 4, 5, 7, 9))

	got, err := svc.SubtitlesVTT(context.Background(), "file:///media", []string{"t2", "t4", "t5", "t7", "t9"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if calls := ffmpegCalls(t, logPath); len(calls) != 1 {
		t.Fatalf("expected 1 ffmpeg invocation, got %d: %v", len(calls), calls)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 results, got %d", len(got))
	}
}

func TestSubtitlesVTTMapsOutputsToTracks(t *testing.T) {
	installSubtitleFFmpeg(t)
	svc := newSubtitleController(t, textTracks(2, 4, 5))

	got, err := svc.SubtitlesVTT(context.Background(), "file:///media", []string{"t2", "t4", "t5"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, id := range []string{"t2", "t4", "t5"} {
		want := "WEBVTT stream 0:" + strings.TrimPrefix(id, "t")
		if got[id].Err != nil || strings.TrimSpace(string(got[id].Data)) != want {
			t.Fatalf("track %s: data %q err %v, want %q", id, got[id].Data, got[id].Err, want)
		}
	}
}

func TestSubtitlesVTTBitmapTrackDoesNotFailOthers(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	subs := textTracks(2, 4)
	subs = append(subs, domain.SubtitleStream{Index: 6, ID: "pgs", Codec: "hdmv_pgs_subtitle", Language: "en"})
	svc := newSubtitleController(t, subs)

	got, err := svc.SubtitlesVTT(context.Background(), "file:///media", []string{"t2", "pgs", "t4"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got["pgs"].Err == nil {
		t.Fatalf("expected error for bitmap track")
	}
	if got["t2"].Err != nil || got["t4"].Err != nil || len(got["t2"].Data) == 0 || len(got["t4"].Data) == 0 {
		t.Fatalf("text tracks must succeed: %+v", got)
	}
	if calls := ffmpegCalls(t, logPath); len(calls) != 1 || strings.Contains(calls[0], "0:6") {
		t.Fatalf("bitmap track must not reach ffmpeg: %v", calls)
	}
}

func TestSubtitlesVTTFailingTrackDoesNotFailOthers(t *testing.T) {
	installSubtitleFFmpeg(t)
	svc := newSubtitleController(t, textTracks(2, 3, 4))

	got, err := svc.SubtitlesVTT(context.Background(), "file:///media", []string{"t2", "t3", "t4"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got["t3"].Err == nil {
		t.Fatalf("expected error for failing track")
	}
	if got["t2"].Err != nil || got["t4"].Err != nil || len(got["t2"].Data) == 0 || len(got["t4"].Data) == 0 {
		t.Fatalf("other tracks must succeed: %+v", got)
	}
}

func TestSubtitlesVTTSkipsCachedTracks(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	svc := newSubtitleController(t, textTracks(2, 4))

	if _, err := svc.SubtitlesVTT(context.Background(), "file:///media", []string{"t2", "t4"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	got, err := svc.SubtitlesVTT(context.Background(), "file:///media", []string{"t2", "t4"})
	if err != nil || len(got["t2"].Data) == 0 {
		t.Fatalf("second: %v %+v", err, got)
	}
	if calls := ffmpegCalls(t, logPath); len(calls) != 1 {
		t.Fatalf("expected cache hit on second call, got %d invocations", len(calls))
	}
}

func TestSubtitlesVTTIndexesProgressiveMP4AndCachesResult(t *testing.T) {
	path := makeIndexedSubtitleFixture(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var requests atomic.Int64
	var transferred atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start, end, ok := subtitleTestRange(request.Header.Get("Range"))
		if !ok || start < 0 || end >= int64(len(content)) {
			http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		requests.Add(1)
		transferred.Add(end - start + 1)
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(content[start : end+1])
	}))
	defer server.Close()

	metadata, err := json.Marshal(&domain.Metadata{Subtitles: []domain.SubtitleStream{{
		Index: 1,
		ID:    "english",
		Codec: "mov_text",
	}}})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	storage := &keyedSubtitleStorage{stubStorage: &stubStorage{metaData: metadata, metaExists: true}}
	service := NewController(Options{Storage: storage, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	sourceURL := server.URL + "/captions.mp4"

	first, err := service.SubtitleVTT(t.Context(), sourceURL, "english")
	if err != nil {
		t.Fatalf("first subtitle request: %v", err)
	}
	if !strings.Contains(string(first), "Indexed subtitle") {
		t.Fatalf("first subtitle response = %q", first)
	}
	requestCount := requests.Load()
	if requestCount == 0 || transferred.Load() >= 1<<20 {
		t.Fatalf("indexed request used %d requests and %d of %d bytes", requestCount, transferred.Load(), len(content))
	}

	second, err := service.SubtitleVTT(t.Context(), sourceURL, "english")
	if err != nil {
		t.Fatalf("cached subtitle request: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("cached response differs: %q != %q", second, first)
	}
	if requests.Load() != requestCount {
		t.Fatalf("cached request accessed source: requests changed from %d to %d", requestCount, requests.Load())
	}
}

func TestSubtitlesVTTUnknownTrackIsReported(t *testing.T) {
	installSubtitleFFmpeg(t)
	svc := newSubtitleController(t, textTracks(2))

	got, err := svc.SubtitlesVTT(context.Background(), "file:///media", []string{"t2", "nope"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got["nope"].Err == nil || got["t2"].Err != nil {
		t.Fatalf("unexpected results: %+v", got)
	}
}

func TestSubtitleVTTUsesBatchPath(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	svc := newSubtitleController(t, textTracks(2))

	data, err := svc.SubtitleVTT(context.Background(), "file:///media", "t2")
	if err != nil || strings.TrimSpace(string(data)) != "WEBVTT stream 0:2" {
		t.Fatalf("single: %q %v", data, err)
	}
	if calls := ffmpegCalls(t, logPath); len(calls) != 1 {
		t.Fatalf("expected 1 invocation, got %d", len(calls))
	}
}

func TestSubtitlesVTTCallerCancelDetachesAndGenerationTimeoutKillsFFmpeg(t *testing.T) {
	_, pidPath := installSubtitleFFmpeg(t)
	t.Setenv("FAKE_HANG", "1")
	svc := newSubtitleControllerWithTimeout(t, textTracks(2, 4), 1500*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := svc.SubtitlesVTT(ctx, "file:///media", []string{"t2", "t4"})
		done <- err
	}()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidPath)
		if err == nil {
			if n, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil {
				pid = n
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("ffmpeg never started")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected cancellation error")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("batch did not return after cancel")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("ffmpeg process %d died on caller cancel", pid)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ffmpeg process %d still running after generation timeout", pid)
}

func makeIndexedSubtitleFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	dir := t.TempDir()
	subtitlePath := filepath.Join(dir, "captions.srt")
	mediaPath := filepath.Join(dir, "captions.mp4")
	if err := os.WriteFile(subtitlePath, []byte("1\n00:00:00,250 --> 00:00:01,250\nIndexed subtitle\n"), 0o600); err != nil {
		t.Fatalf("write subtitle fixture: %v", err)
	}
	command := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=25:d=2",
		"-i", subtitlePath,
		"-map", "0:v", "-map", "1:s",
		"-c:v", "libx264", "-c:s", "mov_text",
		"-movflags", "+faststart", "-t", "2", mediaPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build subtitle fixture: %v: %s", err, output)
	}
	if err := os.Truncate(mediaPath, 32<<20); err != nil {
		t.Fatalf("pad subtitle fixture: %v", err)
	}
	return mediaPath
}

func subtitleTestRange(value string) (int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	return start, end, err == nil && start <= end
}
