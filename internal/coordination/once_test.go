package coordination

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/coordination/coordtest"
	"github.com/eleven-am/goshl/internal/domain"
	"golang.org/x/sync/singleflight"
)

func TestLockWaiterTakesOverWhenHolderReleasesWithoutAsset(t *testing.T) {
	lockWaiting := make(chan struct{}, 1)
	runner := Runner{Kind: "asset", Coordinator: coordtest.NewMemCoordinator(), Timeout: func() time.Duration { return time.Minute }, Observe: func(_ context.Context, event domain.Event) {
		if event.Type == domain.EventLockWait {
			select {
			case lockWaiting <- struct{}{}:
			default:
			}
		}
	}}
	notReady := func(context.Context) (bool, error) { return false, nil }

	holderIn := make(chan struct{})
	holderOut := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- runner.Coordinate(context.Background(), "k", notReady, func(context.Context) error {
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
	go func() {
		waiterDone <- runner.Coordinate(ctx, "k", notReady, func(context.Context) error {
			ran.Add(1)
			return nil
		})
	}()
	select {
	case <-lockWaiting:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never started waiting for the lock")
	}
	start := time.Now()
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

func TestGenerationTimeoutComesFromOptions(t *testing.T) {
	run := func(timeout time.Duration) (time.Duration, bool) {
		runner := Runner{Kind: "asset", Coordinator: coordtest.NewMemCoordinator(), Timeout: func() time.Duration {
			if timeout > 0 {
				return timeout
			}
			return 10 * time.Minute
		}, Observe: func(context.Context, domain.Event) {}}
		var group singleflight.Group
		ended := make(chan time.Time, 1)
		start := time.Now()
		go func() {
			_ = runner.Once(context.Background(), &group, "k",
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

type failingLockCoordinator struct {
	domain.Coordinator
	keys []string
}

func (c *failingLockCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.keys = append(c.keys, key)
	return "", errors.New("coordinator down")
}

func TestRunnerLockKeyAndErrorFollowKind(t *testing.T) {
	for _, kind := range []string{"metadata", "sprites"} {
		coord := &failingLockCoordinator{}
		runner := Runner{Kind: kind, Coordinator: coord, Timeout: func() time.Duration { return time.Minute }, Observe: func(context.Context, domain.Event) {}}

		err := runner.Coordinate(context.Background(), "k", func(context.Context) (bool, error) { return false, nil }, func(context.Context) error { return nil })

		if len(coord.keys) != 1 || coord.keys[0] != kind+":k" {
			t.Fatalf("kind %q: expected lock key %q, got %v", kind, kind+":k", coord.keys)
		}
		if err == nil || err.Error() != "acquire "+kind+" lock: coordinator down" {
			t.Fatalf("kind %q: unexpected error %v", kind, err)
		}
	}
}

func TestRunnerRejectsEmptyKind(t *testing.T) {
	coord := &failingLockCoordinator{}
	runner := Runner{Coordinator: coord, Timeout: func() time.Duration { return time.Minute }, Observe: func(context.Context, domain.Event) {}}
	ready := func(context.Context) (bool, error) { return false, nil }
	generate := func(context.Context) error { return nil }

	if err := runner.Coordinate(context.Background(), "k", ready, generate); !errors.Is(err, ErrMissingKind) {
		t.Fatalf("expected ErrMissingKind from Coordinate, got %v", err)
	}
	if err := runner.Once(context.Background(), &singleflight.Group{}, "k", ready, generate); !errors.Is(err, ErrMissingKind) {
		t.Fatalf("expected ErrMissingKind from Once, got %v", err)
	}
	if len(coord.keys) != 0 {
		t.Fatalf("no lock may be attempted, got %v", coord.keys)
	}
}

func newLifecycleRunner() Runner {
	return Runner{Lifecycle: NewLifecycle(), Kind: "asset", Timeout: func() time.Duration { return time.Minute }, Observe: func(context.Context, domain.Event) {}}
}

func TestLifecycleCloseStopsAndCancelsContext(t *testing.T) {
	l := NewLifecycle()
	if l.Stopped() {
		t.Fatal("a new lifecycle must not be stopped")
	}
	if l.Context().Err() != nil {
		t.Fatal("a new lifecycle context must be live")
	}
	if !l.Enter() {
		t.Fatal("Enter must succeed before Close")
	}
	l.Exit()

	l.Close()
	l.Close()

	if !l.Stopped() {
		t.Fatal("Close must mark the lifecycle stopped")
	}
	if l.Context().Err() == nil {
		t.Fatal("Close must cancel the lifecycle context")
	}
	if l.Enter() {
		t.Fatal("Enter must fail after Close")
	}
	if !l.Wait(time.Second) {
		t.Fatal("a failed Enter must not count as active work")
	}
}

func TestLifecycleWaitIsBoundedAndCountsActiveWork(t *testing.T) {
	l := NewLifecycle()
	if !l.Wait(time.Hour) {
		t.Fatal("Wait with no active work must return immediately")
	}
	if !l.Enter() || !l.Enter() {
		t.Fatal("Enter must succeed")
	}

	started := time.Now()
	if l.Wait(50 * time.Millisecond) {
		t.Fatal("Wait must report false when the bound elapses with work active")
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("Wait bound not honoured: %v", elapsed)
	}

	l.Exit()
	if l.Wait(30 * time.Millisecond) {
		t.Fatal("Wait must keep waiting while one entrant remains")
	}

	released := make(chan bool, 1)
	go func() { released <- l.Wait(10 * time.Second) }()
	time.Sleep(20 * time.Millisecond)
	l.Exit()
	select {
	case ok := <-released:
		if !ok {
			t.Fatal("Wait must report true once the last entrant exits")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not wake when the last entrant exited")
	}
}

func TestOnceRejectsWhenAlreadyStoppedEvenIfCallIsInFlight(t *testing.T) {
	runner := newLifecycleRunner()
	var group singleflight.Group
	release := make(chan struct{})
	entered := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- runner.Once(context.Background(), &group, "k",
			func(context.Context) (bool, error) { return false, nil },
			func(context.Context) error {
				close(entered)
				<-release
				return nil
			})
	}()
	<-entered
	runner.Lifecycle.Close()

	var ready atomic.Int32
	second := make(chan error, 1)
	go func() {
		second <- runner.Once(context.Background(), &group, "k",
			func(context.Context) (bool, error) { ready.Add(1); return false, nil },
			func(context.Context) error { return nil })
	}()
	select {
	case err := <-second:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("got %v, want ErrStopped without joining the in-flight call", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a stopped runner must not join an in-flight call")
	}
	if ready.Load() != 0 {
		t.Fatal("a stopped runner must not run any work")
	}
	close(release)
	<-firstDone
}

func TestOnceTracksWorkInLifecycleAndRunsOnItsContext(t *testing.T) {
	runner := newLifecycleRunner()
	var group singleflight.Group
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runner.Once(context.Background(), &group, "k",
			func(context.Context) (bool, error) { return false, nil },
			func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
	}()
	<-entered

	if runner.Lifecycle.Wait(20 * time.Millisecond) {
		t.Fatal("running work must be tracked by the lifecycle")
	}
	runner.Lifecycle.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("got %v, want ErrStopped when the lifecycle closes mid-run", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("work must run on the lifecycle context and stop with it")
	}
	if !runner.Lifecycle.Wait(time.Second) {
		t.Fatal("finished work must leave the lifecycle")
	}
}

func TestOnceMapsErrorsToErrStoppedOnlyAfterClose(t *testing.T) {
	boom := errors.New("boom")
	notReady := func(context.Context) (bool, error) { return false, nil }

	running := newLifecycleRunner()
	if err := running.Once(context.Background(), &singleflight.Group{}, "k", notReady, func(context.Context) error { return boom }); !errors.Is(err, boom) || errors.Is(err, ErrStopped) {
		t.Fatalf("got %v, want the original error while running", err)
	}

	closing := newLifecycleRunner()
	err := closing.Once(context.Background(), &singleflight.Group{}, "k", notReady, func(context.Context) error {
		closing.Lifecycle.Close()
		return boom
	})
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("got %v, want ErrStopped for a failure after Close", err)
	}

	succeeding := newLifecycleRunner()
	if err := succeeding.Once(context.Background(), &singleflight.Group{}, "k", notReady, func(context.Context) error {
		succeeding.Lifecycle.Close()
		return nil
	}); err != nil {
		t.Fatalf("a successful run must stay successful, got %v", err)
	}
}

func TestCoordinateSkipsGenerateWhenReadyUnderFreshLock(t *testing.T) {
	coord := coordtest.NewMemCoordinator()
	runner := Runner{Kind: "asset", Coordinator: coord, Timeout: func() time.Duration { return time.Minute }, Observe: func(context.Context, domain.Event) {}}
	var checks, generated atomic.Int32

	err := runner.Coordinate(context.Background(), "k",
		func(context.Context) (bool, error) { checks.Add(1); return true, nil },
		func(context.Context) error { generated.Add(1); return nil })

	if err != nil {
		t.Fatalf("got %v, want nil for an asset that is already ready", err)
	}
	if checks.Load() != 1 {
		t.Fatalf("ready checked %d times under the lock, want 1", checks.Load())
	}
	if generated.Load() != 0 {
		t.Fatalf("generate ran %d times for an asset that was already ready", generated.Load())
	}
	token, err := coord.AcquireLock(context.Background(), "asset:k", time.Minute)
	if err != nil || token == "" {
		t.Fatalf("the lock must be released after the ready recheck, got token %q err %v", token, err)
	}
}

func TestCoordinateReturnsReadyErrorUnderLockWithoutGenerating(t *testing.T) {
	coord := coordtest.NewMemCoordinator()
	runner := Runner{Kind: "asset", Coordinator: coord, Timeout: func() time.Duration { return time.Minute }, Observe: func(context.Context, domain.Event) {}}
	storageDown := errors.New("storage down")
	var generated atomic.Int32

	err := runner.Coordinate(context.Background(), "k",
		func(context.Context) (bool, error) { return false, storageDown },
		func(context.Context) error { generated.Add(1); return nil })

	if !errors.Is(err, storageDown) {
		t.Fatalf("got %v, want the ready error", err)
	}
	if generated.Load() != 0 {
		t.Fatalf("generate ran %d times after ready failed", generated.Load())
	}
	if _, err := coord.AcquireLock(context.Background(), "asset:k", time.Minute); err != nil {
		t.Fatalf("the lock must be released after a ready error, got %v", err)
	}
}

func TestLockWaiterRechecksReadyAfterTakingOverTheLock(t *testing.T) {
	coord := coordtest.NewMemCoordinator()
	lockWaiting := make(chan struct{}, 1)
	runner := Runner{Kind: "asset", Coordinator: coord, Timeout: func() time.Duration { return time.Minute }, Observe: func(_ context.Context, event domain.Event) {
		if event.Type == domain.EventLockWait {
			select {
			case lockWaiting <- struct{}{}:
			default:
			}
		}
	}}
	holderToken, err := coord.AcquireLock(context.Background(), "asset:k", time.Minute)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	var checks, generated atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- runner.Coordinate(ctx, "k",
			func(ctx context.Context) (bool, error) {
				if checks.Add(1) > 1 {
					return true, nil
				}
				return false, coord.ReleaseLock(ctx, "asset:k", holderToken)
			},
			func(context.Context) error { generated.Add(1); return nil })
	}()

	select {
	case <-lockWaiting:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never started waiting for the lock")
	}
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiter error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never returned")
	}
	if checks.Load() != 2 {
		t.Fatalf("ready checked %d times, want 2 (once while waiting, once under the lock)", checks.Load())
	}
	if generated.Load() != 0 {
		t.Fatalf("waiter regenerated an asset the holder finished between its poll and its lock, generate ran %d times", generated.Load())
	}
	if _, err := coord.AcquireLock(context.Background(), "asset:k", time.Minute); err != nil {
		t.Fatalf("the waiter must release the lock it took over, got %v", err)
	}
}

func TestCoordinateWithoutCoordinatorChecksReadyBeforeGenerating(t *testing.T) {
	runner := Runner{Kind: "asset", Timeout: func() time.Duration { return time.Minute }, Observe: func(context.Context, domain.Event) {}}
	storageDown := errors.New("storage down")
	var generated atomic.Int32
	generate := func(context.Context) error { generated.Add(1); return nil }

	if err := runner.Coordinate(context.Background(), "k", func(context.Context) (bool, error) { return true, nil }, generate); err != nil {
		t.Fatalf("got %v, want nil for an asset that is already ready", err)
	}
	if err := runner.Coordinate(context.Background(), "k", func(context.Context) (bool, error) { return false, storageDown }, generate); !errors.Is(err, storageDown) {
		t.Fatalf("got %v, want the ready error", err)
	}
	if generated.Load() != 0 {
		t.Fatalf("generate ran %d times, want 0", generated.Load())
	}
	if err := runner.Coordinate(context.Background(), "k", func(context.Context) (bool, error) { return false, nil }, generate); err != nil {
		t.Fatalf("got %v, want nil from generate", err)
	}
	if generated.Load() != 1 {
		t.Fatalf("generate ran %d times for a missing asset, want 1", generated.Load())
	}
}
