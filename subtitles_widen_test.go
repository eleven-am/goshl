package goshl

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestSubtitleFullPassServesLaterTracksFromOnePass(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	subs := textTracks(2, 4, 5)
	subs = append(subs, domain.SubtitleStream{Index: 6, ID: "pgs", Codec: "hdmv_pgs_subtitle", Language: "en"})
	svc := newSubtitleController(t, subs)
	source := nonMP4Source(t)

	for _, id := range []string{"t2", "t4", "t5"} {
		data, err := svc.SubtitleVTT(context.Background(), source, id)
		if want := wantStubSubtitleVTT("0:" + strings.TrimPrefix(id, "t")); err != nil || string(data) != want {
			t.Fatalf("track %s: %q %v, want %q", id, data, err, want)
		}
	}
	calls := ffmpegCalls(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("three sequential track requests ran %d ffmpeg passes, want 1: %v", len(calls), calls)
	}
	for _, mapped := range []string{"0:2", "0:4", "0:5"} {
		if !strings.Contains(calls[0], "-map "+mapped+" ") {
			t.Fatalf("the pass does not extract %s: %s", mapped, calls[0])
		}
	}
	if strings.Contains(calls[0], "0:6") {
		t.Fatalf("bitmap sibling reached ffmpeg: %s", calls[0])
	}
}

type concurrentSubtitleStorage struct {
	*keyedSubtitleStorage
	metadata []byte
}

func (s *concurrentSubtitleStorage) MetadataExists(ctx context.Context, sourceURL string) (bool, error) {
	return true, nil
}

func (s *concurrentSubtitleStorage) GetMetadata(ctx context.Context, sourceURL string) ([]byte, error) {
	return s.metadata, nil
}

func TestSubtitleFullPassIsSharedByConcurrentTrackRequests(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	t.Setenv("FAKE_DELAY", "0.4")
	metadata, _ := json.Marshal(&domain.Metadata{Subtitles: textTracks(2, 4, 5)})
	svc := NewController(Options{
		Storage:     &concurrentSubtitleStorage{keyedSubtitleStorage: &keyedSubtitleStorage{stubStorage: &stubStorage{}}, metadata: metadata},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})
	source := nonMP4Source(t)

	var group sync.WaitGroup
	results := make(map[string]string)
	var lock sync.Mutex
	for _, id := range []string{"t2", "t4", "t5", "t2"} {
		group.Add(1)
		go func() {
			defer group.Done()
			data, err := svc.SubtitleVTT(context.Background(), source, id)
			lock.Lock()
			defer lock.Unlock()
			if err != nil {
				results[id] = "error: " + err.Error()
				return
			}
			results[id] = string(data)
		}()
	}
	group.Wait()
	for _, id := range []string{"t2", "t4", "t5"} {
		if want := wantStubSubtitleVTT("0:" + strings.TrimPrefix(id, "t")); results[id] != want {
			t.Fatalf("track %s: %q, want %q", id, results[id], want)
		}
	}
	if calls := ffmpegCalls(t, logPath); len(calls) != 1 {
		t.Fatalf("concurrent requests for different tracks ran %d ffmpeg passes, want 1: %v", len(calls), calls)
	}
}

func TestSubtitleFullPassNarrowsWhenASiblingBreaksIt(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	svc := newSubtitleController(t, textTracks(2, 3, 4))
	source := nonMP4Source(t)

	for _, id := range []string{"t2", "t4"} {
		data, err := svc.SubtitleVTT(context.Background(), source, id)
		if want := wantStubSubtitleVTT("0:" + strings.TrimPrefix(id, "t")); err != nil || string(data) != want {
			t.Fatalf("track %s: %q %v, want %q", id, data, err, want)
		}
	}
	calls := ffmpegCalls(t, logPath)
	if len(calls) != 3 {
		t.Fatalf("expected one failed wide pass and one narrow pass per track, got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "0:3") || strings.Contains(calls[1], "0:3") || strings.Contains(calls[2], "0:3") || strings.Contains(calls[2], "0:2") {
		t.Fatalf("unexpected pass contents: %v", calls)
	}
	if _, err := svc.SubtitleVTT(context.Background(), source, "t3"); err == nil {
		t.Fatal("the broken track must still report its failure")
	}
}

type cachedTrackStorage struct {
	*keyedSubtitleStorage
	cached string
}

func (s *cachedTrackStorage) SubtitleVTTExists(ctx context.Context, mediaID string, lang string) (bool, error) {
	if lang == s.cached {
		return true, nil
	}
	return s.keyedSubtitleStorage.SubtitleVTTExists(ctx, mediaID, lang)
}

func TestSubtitleFullPassSkipsCachedSiblings(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	metadata, _ := json.Marshal(&domain.Metadata{Subtitles: textTracks(2, 4, 5)})
	svc := NewController(Options{
		Storage:     &cachedTrackStorage{keyedSubtitleStorage: &keyedSubtitleStorage{stubStorage: &stubStorage{metaData: metadata, metaExists: true}}, cached: "t4"},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	})
	if _, err := svc.SubtitleVTT(context.Background(), nonMP4Source(t), "t2"); err != nil {
		t.Fatalf("track t2: %v", err)
	}
	calls := ffmpegCalls(t, logPath)
	if len(calls) != 1 || !strings.Contains(calls[0], "-map 0:2 ") || !strings.Contains(calls[0], "-map 0:5 ") || strings.Contains(calls[0], "0:4") {
		t.Fatalf("pass must extract t2 and t5 and skip the cached t4: %v", calls)
	}
}
