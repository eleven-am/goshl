package domain

import (
	"context"
	"errors"
	"time"
)

var ErrLockHeld = errors.New("lock is held by another instance")

type Coordinator interface {
	Enqueue(ctx context.Context, job Job) error
	Subscribe(ctx context.Context, streamType StreamType) (<-chan Job, error)
	Ack(ctx context.Context, jobID string) error

	NotifySegment(ctx context.Context, info SegmentData, status SegmentStatus) error
	WaitSegment(ctx context.Context, info SegmentData) (<-chan SegmentStatus, error)

	AcquireLock(ctx context.Context, key string, ttl time.Duration) (token string, err error)
	ReleaseLock(ctx context.Context, key string, token string) error

	Close()
}
