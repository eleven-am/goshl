package domain

import (
	"context"
	"errors"
	"time"
)

var ErrLockHeld = errors.New("lock is held by another instance")

type Coordinator interface {
	Enqueue(ctx context.Context, job Job) error
	Subscribe(ctx context.Context, streamType StreamType, profile string) (<-chan JobDelivery, error)
	Renew(ctx context.Context, receipt string, ttl time.Duration) error
	Ack(ctx context.Context, receipt string) error
	SettleJob(ctx context.Context, receipt string, status JobStatus) error
	WaitJob(ctx context.Context, runID string) (<-chan JobStatus, error)

	NotifySegment(ctx context.Context, info SegmentData, status SegmentStatus) error
	WaitSegment(ctx context.Context, info SegmentData) (<-chan SegmentStatus, error)

	AcquireJob(ctx context.Context, jobID string, runID string, ttl time.Duration) (token string, activeRunID string, err error)
	ReleaseJob(ctx context.Context, jobID string, runID string, token string) error
	AcquireLock(ctx context.Context, key string, ttl time.Duration) (token string, err error)
	ReleaseLock(ctx context.Context, key string, token string) error
}
