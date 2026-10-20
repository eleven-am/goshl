package misc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"golang.org/x/sync/singleflight"
)

type memCoordinator struct {
	domain.Coordinator
	mu    sync.Mutex
	locks map[string]string
	next  int
}

func newMemCoordinator() *memCoordinator {
	return &memCoordinator{locks: map[string]string{}}
}

func (c *memCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.locks[key]; held {
		return "", domain.ErrLockHeld
	}
	c.next++
	token := key + "#" + time.Duration(c.next).String()
	c.locks[key] = token
	return token, nil
}

func (c *memCoordinator) ReleaseLock(ctx context.Context, key string, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[key] == token {
		delete(c.locks, key)
	}
	return nil
}

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

func TestLockWaiterTakesOverWhenHolderReleasesWithoutAsset(t *testing.T) {
	coordinator := newMemCoordinator()
	gen := NewGeneratorWithCoordinator(newVTTStore(), coordinator, nil)
	notReady := func(context.Context) (bool, error) { return false, nil }

	holderIn := make(chan struct{})
	holderOut := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- gen.coordinateGeneration(context.Background(), "k", notReady, func(context.Context) error {
			close(holderIn)
			<-holderOut
			return errors.New("holder failed")
		})
	}()
	<-holderIn

	var ran atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	waiterDone := make(chan error, 1)
	start := time.Now()
	go func() {
		waiterDone <- gen.coordinateGeneration(ctx, "k", notReady, func(context.Context) error {
			ran.Add(1)
			return nil
		})
	}()
	time.Sleep(300 * time.Millisecond)
	close(holderOut)
	<-holderDone

	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiter error: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("waiter never returned")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("waiter took %v after holder released", elapsed)
	}
	if ran.Load() != 1 {
		t.Fatalf("waiter generate ran %d times, want 1", ran.Load())
	}
}

func TestSubtitleBatchSurvivesCallerCancel(t *testing.T) {
	store := newVTTStore()
	gen := NewGeneratorWithCoordinator(store, newMemCoordinator(), nil)
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
	gen := NewGeneratorWithCoordinator(store, newMemCoordinator(), nil)
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

func TestGenerationTimeoutComesFromOptions(t *testing.T) {
	run := func(timeout time.Duration) (time.Duration, bool) {
		gen := NewGeneratorWithCoordinator(newVTTStore(), newMemCoordinator(), nil)
		gen.generationTimeout = timeout
		var group singleflight.Group
		ended := make(chan time.Time, 1)
		start := time.Now()
		go func() {
			_ = gen.generateOnce(context.Background(), &group, "k",
				func(context.Context) (bool, error) { return false, nil },
				func(ctx context.Context) error {
					<-ctx.Done()
					ended <- time.Now()
					return ctx.Err()
				})
		}()
		select {
		case at := <-ended:
			return at.Sub(start), true
		case <-time.After(400 * time.Millisecond):
			return 0, false
		}
	}

	elapsed, ended := run(150 * time.Millisecond)
	if !ended || elapsed < 100*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Fatalf("configured timeout: ended=%v elapsed=%v, want about 150ms", ended, elapsed)
	}
	if _, ended := run(0); ended {
		t.Fatal("default timeout ended within 400ms")
	}
}
