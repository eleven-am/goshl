package goshl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type metadataStore struct {
	*stubStorage
	mu      sync.Mutex
	data    []byte
	exists  bool
	deleted []string
}

func (s *metadataStore) MetadataExists(ctx context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exists, nil
}

func (s *metadataStore) GetMetadata(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data, nil
}

func (s *metadataStore) SetMetadata(ctx context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	s.exists = true
	return nil
}

func (s *metadataStore) DeleteMedia(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, key)
	s.data = nil
	s.exists = false
	return nil
}

func (s *metadataStore) snapshot() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data, s.exists
}

type lockingCoordinator struct {
	*stubCoordinator
	mu   sync.Mutex
	held map[string]bool
}

func newLockingCoordinator() *lockingCoordinator {
	return &lockingCoordinator{stubCoordinator: &stubCoordinator{}, held: map[string]bool{}}
}

func (c *lockingCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[key] {
		return "", ErrLockHeld
	}
	c.held[key] = true
	return "token", nil
}

func (c *lockingCoordinator) ReleaseLock(ctx context.Context, key string, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, key)
	return nil
}

func (c *lockingCoordinator) holdExternally(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[key] = true
}

func (c *lockingCoordinator) releaseExternally(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, key)
}

func installCountingFFprobe(t *testing.T, sleepSeconds string, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	body := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then exit 0; fi\necho x >> '" + counter + "'\nsleep " + sleepSeconds + "\n"
	if fail {
		body += "echo boom >&2\nexit 1\n"
	} else {
		body += `if printf "%s" "$*" | grep -q "show_entries"; then
  printf '0.000000,K\n3.000000,K\n'
  exit 0
fi
echo '{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1920,"height":1080,"r_frame_rate":"30/1"}],"format":{"duration":"12.5"}}'
`
	}
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(body), 0755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func probeCalls(counter string) int {
	data, err := os.ReadFile(counter)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "x")
}

type eventLog struct {
	mu     sync.Mutex
	events []domain.Event
}

func (l *eventLog) observer() domain.Observer {
	return domain.ObserverFunc(func(ctx context.Context, event domain.Event) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.events = append(l.events, event)
	})
}

func (l *eventLog) withError() []domain.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []domain.Event
	for _, event := range l.events {
		if event.Error != "" {
			out = append(out, event)
		}
	}
	return out
}

func metadataController(t *testing.T, store *metadataStore, coord domain.Coordinator, opts Options) *Controller {
	t.Helper()
	t.Cleanup(installFakeFFmpeg(t))
	opts.Storage = store
	opts.Coordinator = coord
	opts.PathGen = stubPathGen{}
	return NewController(opts)
}

func TestMetadataWaiterProbesWhenHolderFails(t *testing.T) {
	installCountingFFprobe(t, "0", false)
	store := &metadataStore{stubStorage: &stubStorage{}}
	coord := newLockingCoordinator()
	svc := metadataController(t, store, coord, Options{ProbeTimeout: 4 * time.Second})
	mediaKey, err := svc.MediaKey(context.Background(), "file:///media")
	if err != nil {
		t.Fatal(err)
	}
	lockKey := "metadata:" + mediaKey
	coord.holdExternally(lockKey)
	time.AfterFunc(300*time.Millisecond, func() {
		coord.releaseExternally(lockKey)
	})

	started := time.Now()
	meta, err := svc.Metadata(context.Background(), "file:///media")

	if err != nil {
		t.Fatalf("waiter must take over after the holder fails, got %v after %v", err, time.Since(started))
	}
	if meta == nil || meta.Duration != 12.5 {
		t.Fatalf("unexpected metadata %#v", meta)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("takeover took %v", elapsed)
	}
}

func TestMetadataProbeSurvivesRequesterCancellation(t *testing.T) {
	counter := installCountingFFprobe(t, "1", false)
	store := &metadataStore{stubStorage: &stubStorage{}}
	svc := metadataController(t, store, newLockingCoordinator(), Options{ProbeTimeout: 10 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(250*time.Millisecond, cancel)
	if _, err := svc.Metadata(ctx, "file:///media"); !errors.Is(err, context.Canceled) {
		t.Fatalf("requester should observe its own cancellation, got %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store.snapshot(); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, ok := store.snapshot()
	if !ok {
		t.Fatalf("probe was killed with the requester (ffprobe calls: %d)", probeCalls(counter))
	}
	var meta domain.Metadata
	if err := json.Unmarshal(data, &meta); err != nil || meta.Duration != 12.5 {
		t.Fatalf("stored metadata invalid: %v %#v", err, meta)
	}
}

func TestCorruptCachedMetadataIsReprobed(t *testing.T) {
	counter := installCountingFFprobe(t, "0", false)
	store := &metadataStore{stubStorage: &stubStorage{}, data: []byte("{not json"), exists: true}
	log := &eventLog{}
	svc := metadataController(t, store, newLockingCoordinator(), Options{ProbeTimeout: 5 * time.Second, Observer: log.observer()})

	meta, err := svc.Metadata(context.Background(), "file:///media")

	if err != nil {
		t.Fatalf("corrupt cache must be a miss, got %v", err)
	}
	if meta.Duration != 12.5 || probeCalls(counter) == 0 {
		t.Fatalf("expected re-probe, got %#v calls=%d", meta, probeCalls(counter))
	}
	data, _ := store.snapshot()
	var stored domain.Metadata
	if err := json.Unmarshal(data, &stored); err != nil || stored.Duration != 12.5 {
		t.Fatalf("corrupt entry was not overwritten: %v %q", err, data)
	}
	found := false
	for _, event := range log.withError() {
		if strings.Contains(event.Error, "corrupt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an observer event with Error for the corrupt entry, got %#v", log.withError())
	}
}

func TestFailedProbeNotRepeatedWithinNegativeTTL(t *testing.T) {
	counter := installCountingFFprobe(t, "0", true)
	store := &metadataStore{stubStorage: &stubStorage{}}
	svc := metadataController(t, store, newLockingCoordinator(), Options{ProbeTimeout: 5 * time.Second})

	_, first := svc.Metadata(context.Background(), "file:///media")
	callsAfterFirst := probeCalls(counter)
	_, second := svc.Metadata(context.Background(), "file:///media")

	if first == nil || second == nil {
		t.Fatalf("expected both calls to fail, got %v / %v", first, second)
	}
	if callsAfterFirst == 0 {
		t.Fatal("first call did not probe")
	}
	if got := probeCalls(counter); got != callsAfterFirst {
		t.Fatalf("failed probe repeated within negative TTL: %d -> %d", callsAfterFirst, got)
	}
	if first.Error() != second.Error() {
		t.Fatalf("cached failure should report the original error: %v vs %v", first, second)
	}
}

func TestInvalidateClearsNegativeMetadataCache(t *testing.T) {
	counter := installCountingFFprobe(t, "0", true)
	store := &metadataStore{stubStorage: &stubStorage{}}
	svc := metadataController(t, store, newLockingCoordinator(), Options{ProbeTimeout: 5 * time.Second})

	_, _ = svc.Metadata(context.Background(), "file:///media")
	before := probeCalls(counter)
	if err := svc.Invalidate(context.Background(), "file:///media"); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.Metadata(context.Background(), "file:///media")

	if got := probeCalls(counter); got <= before {
		t.Fatalf("invalidate must clear the negative cache: calls %d -> %d", before, got)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestNegativeMetadataCacheExpiresAfterTenSeconds(t *testing.T) {
	clock := newFakeClock()
	state := &metadataState{now: clock.Now}
	failure := errors.New("probe failed")
	state.recordFailure("key", failure, state.currentEpoch())

	clock.Advance(9*time.Second + 900*time.Millisecond)
	if got := state.recentFailure("key"); !errors.Is(got, failure) {
		t.Fatalf("failure must still be cached just under 10s, got %v", got)
	}
	clock.Advance(200 * time.Millisecond)
	if got := state.recentFailure("key"); got != nil {
		t.Fatalf("failure must expire after 10s, got %v", got)
	}
}

func TestNegativeMetadataCacheTTLThroughController(t *testing.T) {
	counter := installCountingFFprobe(t, "0", true)
	store := &metadataStore{stubStorage: &stubStorage{}}
	svc := metadataController(t, store, newLockingCoordinator(), Options{ProbeTimeout: 5 * time.Second})
	clock := newFakeClock()
	svc.metadata.now = clock.Now

	_, _ = svc.Metadata(context.Background(), "file:///media")
	first := probeCalls(counter)
	clock.Advance(9 * time.Second)
	_, _ = svc.Metadata(context.Background(), "file:///media")
	if got := probeCalls(counter); got != first {
		t.Fatalf("probe repeated before the 10s TTL: %d -> %d", first, got)
	}
	clock.Advance(2 * time.Second)
	_, _ = svc.Metadata(context.Background(), "file:///media")
	if got := probeCalls(counter); got <= first {
		t.Fatalf("probe must be retried after the 10s TTL: %d -> %d", first, got)
	}
}

func TestNegativeMetadataCachePrunesExpiredOnInsert(t *testing.T) {
	clock := newFakeClock()
	state := &metadataState{now: clock.Now}
	for i := 0; i < 50; i++ {
		state.recordFailure(fmt.Sprintf("old-%d", i), errors.New("x"), state.currentEpoch())
	}
	clock.Advance(metadataNegativeTTL + time.Second)
	state.recordFailure("fresh", errors.New("x"), state.currentEpoch())

	if got := len(state.failures); got != 1 {
		t.Fatalf("expired entries must be pruned on insert, have %d", got)
	}
}

func TestNegativeMetadataCacheIsCapped(t *testing.T) {
	clock := newFakeClock()
	state := &metadataState{now: clock.Now}
	for i := 0; i < metadataNegativeMax+500; i++ {
		clock.Advance(time.Microsecond)
		state.recordFailure(fmt.Sprintf("key-%d", i), errors.New("x"), state.currentEpoch())
	}

	if got := len(state.failures); got != metadataNegativeMax {
		t.Fatalf("negative cache must hold at most %d entries, have %d", metadataNegativeMax, got)
	}
	if state.recentFailure("key-0") != nil {
		t.Fatal("oldest entry should have been evicted")
	}
	if state.recentFailure(fmt.Sprintf("key-%d", metadataNegativeMax+499)) == nil {
		t.Fatal("newest entry must be retained")
	}
}

func TestCorruptCacheEmitsOneEventAcrossWaiterPolls(t *testing.T) {
	installCountingFFprobe(t, "0", false)
	store := &metadataStore{stubStorage: &stubStorage{}, data: []byte("{not json"), exists: true}
	coord := newLockingCoordinator()
	log := &eventLog{}
	svc := metadataController(t, store, coord, Options{ProbeTimeout: 5 * time.Second, Observer: log.observer()})
	mediaKey, err := svc.MediaKey(context.Background(), "file:///media")
	if err != nil {
		t.Fatal(err)
	}
	lockKey := "metadata:" + mediaKey
	coord.holdExternally(lockKey)
	time.AfterFunc(550*time.Millisecond, func() { coord.releaseExternally(lockKey) })

	if _, err := svc.Metadata(context.Background(), "file:///media"); err != nil {
		t.Fatal(err)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	misses, corrupt := 0, 0
	for _, event := range log.events {
		if event.Type == domain.EventMetadataCacheMiss {
			misses++
			if strings.Contains(event.Error, "corrupt") {
				corrupt++
			}
		}
	}
	if misses != 1 || corrupt != 1 {
		t.Fatalf("want exactly one miss event carrying the corrupt error, got misses=%d corrupt=%d", misses, corrupt)
	}
}

func TestHolderProbeTimeoutSurfacesAsMetadataTimeout(t *testing.T) {
	installCountingFFprobe(t, "2", false)
	store := &metadataStore{stubStorage: &stubStorage{}}
	svc := metadataController(t, store, newLockingCoordinator(), Options{ProbeTimeout: 300 * time.Millisecond})

	_, err := svc.Metadata(context.Background(), "file:///media")

	if !errors.Is(err, ErrMetadataTimeout) {
		t.Fatalf("holder timeout must be ErrMetadataTimeout, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error must wrap the deadline cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "killed") {
		t.Fatalf("timeout error must keep the underlying probe cause, got %v", err)
	}
}

func TestProbeStartedBeforeInvalidateDoesNotRecordFailure(t *testing.T) {
	counter := installCountingFFprobe(t, "1", true)
	store := &metadataStore{stubStorage: &stubStorage{}}
	svc := metadataController(t, store, newLockingCoordinator(), Options{ProbeTimeout: 5 * time.Second})

	done := make(chan error, 1)
	go func() {
		_, err := svc.Metadata(context.Background(), "file:///media")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for probeCalls(counter) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if probeCalls(counter) == 0 {
		t.Fatal("probe never started")
	}
	if err := svc.Invalidate(context.Background(), "file:///media"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("in-flight probe was expected to fail")
	}
	before := probeCalls(counter)

	_, _ = svc.Metadata(context.Background(), "file:///media")

	if got := probeCalls(counter); got <= before {
		t.Fatalf("failure of a probe started before Invalidate must not be cached: calls %d -> %d", before, got)
	}
}
