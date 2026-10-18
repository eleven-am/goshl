package domain

import (
	"context"
	"time"
)

type EventType string

const (
	EventMetadataCacheHit    EventType = "metadata.cache.hit"
	EventMetadataCacheMiss   EventType = "metadata.cache.miss"
	EventMetadataProbed      EventType = "metadata.probed"
	EventSegmentCacheHit     EventType = "segment.cache.hit"
	EventSegmentCacheMiss    EventType = "segment.cache.miss"
	EventLockWait            EventType = "segment.lock.wait"
	EventJobEnqueued         EventType = "job.enqueued"
	EventJobJoined           EventType = "job.joined"
	EventJobAbandoned        EventType = "job.abandoned"
	EventJobStarted          EventType = "job.started"
	EventJobCompleted        EventType = "job.completed"
	EventJobFailed           EventType = "job.failed"
	EventJobAckFailed        EventType = "job.ack.failed"
	EventJobLeaseFailed      EventType = "job.lease.failed"
	EventJobSettleFailed     EventType = "job.settle.failed"
	EventSegmentReady        EventType = "segment.ready"
	EventSegmentFailed       EventType = "segment.failed"
	EventSegmentNotifyFailed EventType = "segment.notify.failed"
	EventHardwareSelected    EventType = "hardware.selected"
	EventHardwareFallback    EventType = "hardware.fallback"
	EventLockReleaseFailed   EventType = "lock.release.failed"
)

type Event struct {
	Type        EventType
	Time        time.Time
	MediaKey    string
	JobID       string
	StreamType  StreamType
	Rendition   string
	Index       int
	StartIndex  int
	EndIndex    int
	Duration    time.Duration
	Bytes       int64
	Accelerator Accelerator
	Error       string
}

type Observer interface {
	Observe(context.Context, Event)
}

type ObserverFunc func(context.Context, Event)

func (f ObserverFunc) Observe(ctx context.Context, event Event) {
	f(ctx, event)
}
