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
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/coordination/coordtest"
	"github.com/eleven-am/goshl/internal/testmedia"
	"github.com/eleven-am/goshl/internal/webvtt"
)

type strictVTTStore struct {
	*stubStorage
	mu   sync.Mutex
	data map[string][]byte
}

func newStrictVTTStore() *strictVTTStore {
	return &strictVTTStore{stubStorage: &stubStorage{}, data: map[string][]byte{}}
}

func (s *strictVTTStore) WriteSubtitleVTT(ctx context.Context, mediaID string, lang string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[mediaID+"/"+lang] = append([]byte(nil), data...)
	return nil
}

func (s *strictVTTStore) ReadSubtitleVTT(ctx context.Context, mediaID string, lang string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[mediaID+"/"+lang]
	if !ok {
		return nil, fmt.Errorf("subtitle %s/%s: %w", mediaID, lang, os.ErrNotExist)
	}
	return data, nil
}

func (s *strictVTTStore) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[mediaID+"/"+lang]
	return ok, nil
}

func twoSubtitleFixture(t *testing.T) string {
	t.Helper()
	testmedia.Require(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	var script strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&script, "%d\n00:00:0%d,000 --> 00:00:0%d,500\ncue %d\n\n", i+1, i, i, i+1)
	}
	if err := os.WriteFile(srt, []byte(script.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "two.mkv")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=d=6:s=160x90:r=10", "-i", srt, "-i", srt, "-map", "0:v", "-map", "1", "-map", "2", "-c:v", "libx264", "-c:s", "srt", out)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, data)
	}
	return out
}

func TestSubtitleRetryAfterFailedBatchStoresCompleteVTT(t *testing.T) {
	source := twoSubtitleFixture(t)
	store := newStrictVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	jobs := []SubtitleJob{
		{Key: "m", TrackID: "en", StreamIndex: 1, Codec: "subrip"},
		{Key: "m", TrackID: "stale", StreamIndex: 0, Codec: "subrip"},
		{Key: "m", TrackID: "fr", StreamIndex: 2, Codec: "subrip"},
	}
	outcomes, err := gen.GetSubtitlesBatch(context.Background(), source, jobs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if outcomes["stale"].Err == nil {
		t.Fatalf("stale track must fail")
	}
	for _, id := range []string{"en", "fr"} {
		if outcomes[id].Err != nil {
			t.Fatalf("track %s failed: %v", id, outcomes[id].Err)
		}
		cues, err := webvtt.Parse(store.data["m/"+id])
		if err != nil || len(cues) != 5 {
			t.Fatalf("track %s stored %d cues (%v):\n%q", id, len(cues), err, store.data["m/"+id])
		}
	}
}

func TestSubtitleRetryDiscardsPartialBatchOutput(t *testing.T) {
	source := twoSubtitleFixture(t)
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nn=0\nfor a; do case \"$a\" in *.vtt) n=$((n+1));; esac; done\nif [ $n -gt 1 ]; then\nfor a; do case \"$a\" in *.vtt) printf 'WEBVTT\\n\\n00:00:00.000 --> 00:00:00.500\\ncue 1\\n\\n' > \"$a\";; esac; done\nexit 1\nfi\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := newStrictVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	jobs := []SubtitleJob{
		{Key: "m", TrackID: "en", StreamIndex: 1, Codec: "subrip"},
		{Key: "m", TrackID: "fr", StreamIndex: 2, Codec: "subrip"},
	}
	outcomes, err := gen.GetSubtitlesBatch(context.Background(), source, jobs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, id := range []string{"en", "fr"} {
		if outcomes[id].Err != nil {
			t.Fatalf("track %s failed: %v", id, outcomes[id].Err)
		}
		cues, err := webvtt.Parse(store.data["m/"+id])
		if err != nil || len(cues) != 5 {
			t.Fatalf("track %s stored %d cues, want 5 (%v)", id, len(cues), err)
		}
	}
}

func TestSubtitleBatchJoinerReceivesTrackFailure(t *testing.T) {
	store := newStrictVTTStore()
	gen := NewGeneratorWithCoordinator(store, coordtest.NewMemCoordinator(), nil)
	boom := errors.New("boom")
	entered := make(chan struct{}, 2)
	gate := make(chan struct{})
	gen.extractSubtitles = func(ctx context.Context, sourceURL string, jobs []SubtitleJob, failures map[string]error) error {
		entered <- struct{}{}
		<-gate
		failures["en"] = boom
		return nil
	}
	jobs := []SubtitleJob{{Key: "m", TrackID: "en", StreamIndex: 2, Codec: "subrip"}}
	var wg sync.WaitGroup
	results := make([]map[string]SubtitleOutcome, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = gen.GetSubtitlesBatch(context.Background(), "src", jobs)
		}(i)
	}
	<-entered
	time.Sleep(200 * time.Millisecond)
	close(gate)
	wg.Wait()
	if len(entered) != 0 {
		t.Fatalf("extraction ran more than once")
	}
	for i := range results {
		if !errors.Is(results[i]["en"].Err, boom) {
			t.Fatalf("caller %d outcome err = %v, want boom", i, results[i]["en"].Err)
		}
	}
}
