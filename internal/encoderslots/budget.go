package encoderslots

import (
	"context"

	"golang.org/x/sync/semaphore"
)

type Budget struct {
	total *semaphore.Weighted
	live  *semaphore.Weighted
}

func New(total, liveMax int) *Budget {
	if total <= 0 {
		return nil
	}
	if liveMax < 0 {
		liveMax = 0
	}
	return &Budget{
		total: semaphore.NewWeighted(int64(total)),
		live:  semaphore.NewWeighted(int64(liveMax)),
	}
}

func (b *Budget) Acquire(ctx context.Context) error {
	if b == nil {
		return nil
	}
	return b.total.Acquire(ctx, 1)
}

func (b *Budget) Release() {
	if b == nil {
		return
	}
	b.total.Release(1)
}

func (b *Budget) TryAcquireLive() bool {
	if b == nil {
		return true
	}
	if !b.live.TryAcquire(1) {
		return false
	}
	if !b.total.TryAcquire(1) {
		b.live.Release(1)
		return false
	}
	return true
}

func (b *Budget) ReleaseLive() {
	if b == nil {
		return
	}
	b.total.Release(1)
	b.live.Release(1)
}
