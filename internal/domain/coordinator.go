package domain

import "context"

type Coordinator interface {
	Enqueue(ctx context.Context, job Job) error
	Subscribe(ctx context.Context, streamType StreamType) (<-chan Job, error)
	Ack(ctx context.Context, jobID string) error

	NotifySegment(ctx context.Context, info SegmentData, status SegmentStatus) error
	WaitSegment(ctx context.Context, info SegmentData) (<-chan SegmentStatus, error)

	Close()
}
