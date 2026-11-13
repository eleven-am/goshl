package goshl

import "github.com/eleven-am/goshl/internal/domain"

type (
	Storage = domain.Storage

	SpriteStorage = domain.SpriteStorage

	Coordinator = domain.Coordinator

	PathGenerator = domain.PathGenerator

	StreamType = domain.StreamType

	SegmentData = domain.SegmentData

	SegmentStatus = domain.SegmentStatus

	Job = domain.Job

	JobDelivery = domain.JobDelivery

	JobState = domain.JobState

	JobStatus = domain.JobStatus

	PreviewConfig = domain.PreviewConfig

	ThumbnailConfig = domain.ThumbnailConfig

	Metadata = domain.Metadata

	VideoStream = domain.VideoStream

	AudioStream = domain.AudioStream

	SubtitleStream = domain.SubtitleStream

	Segment = domain.Segment

	VideoRendition = domain.VideoRendition

	AudioRendition = domain.AudioRendition

	PlaybackMethod = domain.PlaybackMethod

	Accelerator = domain.Accelerator

	HWAccelConfig = domain.HWAccelConfig

	SegmentState = domain.SegmentState

	Observer = domain.Observer

	ObserverFunc = domain.ObserverFunc

	Event = domain.Event

	EventType = domain.EventType

	DecodeMode = domain.DecodeMode
)

const (
	StreamVideo = domain.StreamVideo

	StreamAudio = domain.StreamAudio

	StreamSubtitle = domain.StreamSubtitle

	AccelNone = domain.AccelNone

	AccelCUDA = domain.AccelCUDA

	AccelVideoToolbox = domain.AccelVideoToolbox

	AccelVAAPI = domain.AccelVAAPI

	AccelQSV = domain.AccelQSV

	SegmentStateReady = domain.SegmentStateReady

	SegmentStateError = domain.SegmentStateError

	JobStateCompleted = domain.JobStateCompleted

	JobStateFailed = domain.JobStateFailed

	JobStateCancelled = domain.JobStateCancelled

	DecodeSafe = domain.DecodeSafe

	DecodeDevice = domain.DecodeDevice

	DirectPlay = domain.DirectPlay

	DirectStream = domain.DirectStream

	Transcode = domain.Transcode

	EventMetadataCacheHit = domain.EventMetadataCacheHit

	EventMetadataCacheMiss = domain.EventMetadataCacheMiss

	EventMetadataProbed = domain.EventMetadataProbed

	EventSegmentCacheHit = domain.EventSegmentCacheHit

	EventSegmentCacheMiss = domain.EventSegmentCacheMiss

	EventLockWait = domain.EventLockWait

	EventJobEnqueued = domain.EventJobEnqueued
	EventJobJoined   = domain.EventJobJoined

	EventJobStarted = domain.EventJobStarted

	EventJobCompleted = domain.EventJobCompleted

	EventJobFailed = domain.EventJobFailed

	EventJobAckFailed = domain.EventJobAckFailed

	EventJobLeaseFailed = domain.EventJobLeaseFailed

	EventJobSettleFailed = domain.EventJobSettleFailed

	EventJobDeliveryRejected = domain.EventJobDeliveryRejected

	EventPoolSubscriptionLost = domain.EventPoolSubscriptionLost

	EventSegmentReady = domain.EventSegmentReady

	EventSegmentNotifyFailed = domain.EventSegmentNotifyFailed

	EventHardwareSelected = domain.EventHardwareSelected

	EventHardwareFallback = domain.EventHardwareFallback

	EventLockReleaseFailed = domain.EventLockReleaseFailed
)

var (
	ErrLockHeld = domain.ErrLockHeld
)
