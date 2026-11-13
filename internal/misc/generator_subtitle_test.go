package misc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/coordination/coordtest"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/testmedia"
	"github.com/eleven-am/goshl/internal/webvtt"
)

type vttStore struct {
	*stubStorage
	mu    sync.Mutex
	files map[string]bool
}

func newVTTStore() *vttStore {
	return &vttStore{stubStorage: &stubStorage{}, files: map[string]bool{}}
}

func (s *vttStore) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[mediaID+"/"+lang] = true
	return nil
}

func (s *vttStore) ReadSubtitleVTT(ctx context.Context, mediaID string, lang string) ([]byte, error) {
	return []byte("WEBVTT"), nil
}

func (s *vttStore) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[mediaID+"/"+lang], nil
}

func TestSubtitleBatchSurvivesCallerCancel(t *testing.T) {
	store := newVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	gate := make(chan struct{})
	started := make(chan struct{})
	gen.extractSubtitles = func(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) error {
		close(started)
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, job := range jobs {
			if err := store.WriteSubtitleVTT(ctx, job.Key, job.TrackID, []byte("WEBVTT")); err != nil {
				return err
			}
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	jobs := []SubtitleJob{{Key: "m", TrackID: "en", StreamIndex: 2, Codec: "subrip"}}
	errCh := make(chan error, 1)
	go func() {
		_, err := gen.GetSubtitlesBatch(ctx, "src", jobs)
		errCh <- err
	}()
	<-started
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller error = %v, want context.Canceled", err)
	}
	close(gate)

	deadline := time.Now().Add(2 * time.Second)
	for {
		ok, _ := store.SubtitleVTTExists(context.Background(), "m", "en")
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("subtitle was not written after caller cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConcurrentSubtitleBatchesExtractOnce(t *testing.T) {
	store := newVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	var extractions atomic.Int32
	gate := make(chan struct{})
	gen.extractSubtitles = func(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) error {
		extractions.Add(1)
		<-gate
		for _, job := range jobs {
			_ = store.WriteSubtitleVTT(ctx, job.Key, job.TrackID, []byte("WEBVTT"))
		}
		return nil
	}

	jobs := []SubtitleJob{{Key: "m", TrackID: "en", StreamIndex: 2, Codec: "subrip"}}
	var wg sync.WaitGroup
	results := make([]map[string]SubtitleOutcome, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = gen.GetSubtitlesBatch(context.Background(), "src", jobs)
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
		if results[i]["en"].Err != nil || string(results[i]["en"].Data) != "WEBVTT" {
			t.Fatalf("caller %d outcome = %+v", i, results[i]["en"])
		}
	}
	if extractions.Load() != 1 {
		t.Fatalf("extractions = %d, want 1", extractions.Load())
	}
}

type capturingVTTStore struct {
	*stubStorage
	mu   sync.Mutex
	data map[string][]byte
}

func newCapturingVTTStore() *capturingVTTStore {
	return &capturingVTTStore{stubStorage: &stubStorage{}, data: map[string][]byte{}}
}

func (s *capturingVTTStore) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[mediaID+"/"+lang] = append([]byte(nil), data...)
	return nil
}

func TestHLSSubtitleVTTCarriesTimestampMap(t *testing.T) {
	testmedia.Require(t)
	source := testmedia.H264(t, testmedia.Spec{Container: "mkv", Duration: 6, Subtitles: true})
	store := newCapturingVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	failures := map[string]error{}

	err := gen.extractSubtitleBatchFFmpeg(context.Background(), source, []SubtitleJob{{Key: "m", TrackID: "en", StreamIndex: 1, Codec: "subrip"}}, failures)
	if err != nil || failures["en"] != nil {
		t.Fatalf("extract: %v, failure %v", err, failures["en"])
	}
	data := string(store.data["m/en"])
	wantMap := fmt.Sprintf("X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000", int64(domain.OutputTimelineOffset.Seconds()*90000))
	if !strings.HasPrefix(data, "WEBVTT\n"+wantMap+"\n\n") {
		t.Fatalf("ffmpeg-path vtt lacks %q:\n%s", wantMap, data)
	}
	cues, err := webvtt.Parse([]byte(data))
	if err != nil || len(cues) == 0 || cues[0].Text != "cue 1" {
		t.Fatalf("re-emitted vtt does not parse back: %v %+v\n%s", err, cues, data)
	}
}

func TestFFmpegSubtitleOutputThatCannotBeParsedFailsTheTrack(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg")
	body := "#!/bin/sh\nfor last; do :; done\nprintf 'this is not webvtt' > \"$last\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh is unavailable: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := newCapturingVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	failures := map[string]error{}

	if err := gen.extractSubtitleBatchFFmpeg(context.Background(), "file:///media", []SubtitleJob{{Key: "m", TrackID: "en", StreamIndex: 1}}, failures); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if failures["en"] == nil || !strings.Contains(failures["en"].Error(), "parse extracted subtitle") {
		t.Fatalf("failure = %v, want parse extracted subtitle", failures["en"])
	}
	if len(store.data) != 0 {
		t.Fatalf("unparsable track was stored: %v", store.data)
	}
}
