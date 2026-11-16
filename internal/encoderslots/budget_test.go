package encoderslots

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNilBudgetIsUnlimited(t *testing.T) {
	var b *Budget
	for i := 0; i < 100; i++ {
		if err := b.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if !b.TryAcquireLive() {
			t.Fatal("TryAcquireLive refused on nil budget")
		}
	}
	b.Release()
	b.ReleaseLive()
}

func TestZeroTotalIsUnlimited(t *testing.T) {
	b := New(0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 10; i++ {
		if err := b.Acquire(ctx); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if !b.TryAcquireLive() {
			t.Fatal("TryAcquireLive refused on zero budget")
		}
	}
}

func TestAcquireBlocksUntilRelease(t *testing.T) {
	b := New(1, 1)
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got atomic.Bool
	done := make(chan error, 1)
	go func() {
		err := b.Acquire(context.Background())
		got.Store(true)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if got.Load() {
		t.Fatal("second Acquire did not block")
	}
	b.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b.Release()
}

func TestAcquireCancellation(t *testing.T) {
	b := New(1, 1)
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Acquire(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	b.Release()
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatalf("slot leaked after cancellation: %v", err)
	}
}

func TestAcquireWithCancelledContextWhenFree(t *testing.T) {
	b := New(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Acquire(ctx); err == nil {
		b.Release()
	}
	if !b.TryAcquireLive() {
		t.Fatal("slot leaked by cancelled Acquire")
	}
}

func TestLiveCapIsSeparate(t *testing.T) {
	b := New(4, 1)
	if !b.TryAcquireLive() {
		t.Fatal("first live refused")
	}
	if b.TryAcquireLive() {
		t.Fatal("second live accepted over live cap")
	}
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Release()
	b.ReleaseLive()
	if !b.TryAcquireLive() {
		t.Fatal("live refused after release")
	}
}

func TestLiveNeverBlocksAndRollsBackOnTotalExhausted(t *testing.T) {
	b := New(1, 1)
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.TryAcquireLive() {
		t.Fatal("live accepted with total exhausted")
	}
	b.Release()
	if !b.TryAcquireLive() {
		t.Fatal("live unit leaked when total was exhausted")
	}
	b.ReleaseLive()
}

func TestLiveHoldsTotalSoVODWaits(t *testing.T) {
	b := New(1, 1)
	if !b.TryAcquireLive() {
		t.Fatal("live refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := b.Acquire(ctx); err != context.DeadlineExceeded {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	b.ReleaseLive()
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseOnEveryPathNoLeak(t *testing.T) {
	const total, liveMax = 3, 2
	b := New(total, liveMax)
	var inUse, peak atomic.Int64
	track := func() {
		n := inUse.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				if err := b.Acquire(context.Background()); err != nil {
					return
				}
				track()
				time.Sleep(time.Millisecond)
				inUse.Add(-1)
				b.Release()
			case 1:
				if b.TryAcquireLive() {
					track()
					time.Sleep(time.Millisecond)
					inUse.Add(-1)
					b.ReleaseLive()
				}
			default:
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()
				if err := b.Acquire(ctx); err == nil {
					track()
					inUse.Add(-1)
					b.Release()
				}
			}
		}(i)
	}
	wg.Wait()
	if peak.Load() > total {
		t.Fatalf("peak %d exceeded total %d", peak.Load(), total)
	}
	for i := 0; i < total; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := b.Acquire(ctx); err != nil {
			cancel()
			t.Fatalf("slot %d leaked: %v", i, err)
		}
		cancel()
	}
	for i := 0; i < total; i++ {
		b.Release()
	}
	for i := 0; i < liveMax; i++ {
		if !b.TryAcquireLive() {
			t.Fatalf("live slot %d leaked", i)
		}
	}
}
