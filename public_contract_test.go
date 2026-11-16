package goshl_test

import (
	"context"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
)

type publicPathGenerator struct{}

func (publicPathGenerator) VariantPlaylist(string, string, goshl.StreamType) string { return "" }
func (publicPathGenerator) Segment(string, string, goshl.StreamType, int) string    { return "" }
func (publicPathGenerator) Sprite(string, int) string                               { return "" }
func (publicPathGenerator) SubtitlePlaylist(string, string) string                  { return "" }
func (publicPathGenerator) SubtitleVTT(string, string) string                       { return "" }

type publicObserver struct{}

func (publicObserver) Observe(context.Context, goshl.Event) {}

type publicCoordinator struct{}

func (publicCoordinator) Enqueue(context.Context, goshl.Job) error { return nil }
func (publicCoordinator) Subscribe(context.Context, goshl.StreamType, string) (<-chan goshl.JobDelivery, error) {
	return make(chan goshl.JobDelivery), nil
}
func (publicCoordinator) Renew(context.Context, string, time.Duration) error       { return nil }
func (publicCoordinator) Ack(context.Context, string) error                        { return nil }
func (publicCoordinator) SettleJob(context.Context, string, goshl.JobStatus) error { return nil }
func (publicCoordinator) WaitJob(context.Context, string) (<-chan goshl.JobStatus, error) {
	return make(chan goshl.JobStatus), nil
}
func (publicCoordinator) NotifySegment(context.Context, goshl.SegmentData, goshl.SegmentStatus) error {
	return nil
}
func (publicCoordinator) WaitSegment(context.Context, goshl.SegmentData) (<-chan goshl.SegmentStatus, error) {
	return make(chan goshl.SegmentStatus), nil
}
func (publicCoordinator) AcquireJob(context.Context, string, string, time.Duration) (string, string, error) {
	return "token", "run", nil
}
func (publicCoordinator) ReleaseJob(context.Context, string, string, string) error { return nil }
func (publicCoordinator) AcquireLock(context.Context, string, time.Duration) (string, error) {
	return "token", nil
}
func (publicCoordinator) ReleaseLock(context.Context, string, string) error { return nil }

func TestPublicContractCompilesForExternalConsumers(t *testing.T) {
	var pathGenerator goshl.PathGenerator = publicPathGenerator{}
	var observer goshl.Observer = publicObserver{}
	var coordinator goshl.Coordinator = publicCoordinator{}
	job := goshl.Job{
		Segments:       []goshl.Segment{{Index: 1}},
		VideoRendition: &goshl.VideoRendition{Method: goshl.Transcode},
		AudioRendition: &goshl.AudioRendition{Method: goshl.DirectStream},
		EnqueuedAt:     time.Now(),
		StartBy:        time.Now(),
	}
	_ = pathGenerator
	_ = observer
	_ = coordinator
	_ = job
	_ = goshl.EventSegmentNotifyFailed
	_ = goshl.SegmentStateReady
	_ = goshl.StreamSubtitle
	_ = (*goshl.Controller).ProbeMetadata
	_ = (*goshl.Controller).ChannelPlan
	_ = (*goshl.Controller).ChannelSegment
	_ = (*goshl.Controller).ChannelPrefetch
	_ = (*goshl.Controller).Live
	_ = (*goshl.Controller).LiveProbe
	_ = (*goshl.LiveSession).WaitReady
	_ = (*goshl.LiveSession).Playlist
	_ = (*goshl.LiveSession).Segment
	_ = (*goshl.LiveSession).SwitchSource
	_ = (*goshl.LiveSession).State
	_ = (*goshl.LiveSession).Stop
	_ = (*goshl.LiveSession).Done
	_ = (*goshl.LiveSession).Label
	_ = (*goshl.LiveSession).Info
	_ = (goshl.ChannelProfile).Normalised
	_ = goshl.ChannelPlan.SegmentData
	_ = goshl.Event{Label: "channel"}.Label
	_ = goshl.LiveOptions{ReadySegments: 3}
	_ = goshl.LiveSource{Label: "source"}
	_ = goshl.LiveProbeInfo{VideoAction: goshl.LiveCopy, AudioAction: goshl.LiveTranscode}
	_ = []goshl.EventType{goshl.EventLiveStarted, goshl.EventLiveReady, goshl.EventLiveReconnecting, goshl.EventLiveSwitched, goshl.EventLiveFailed, goshl.EventLiveStopped}
	_ = []error{goshl.ErrLiveSourceUnavailable, goshl.ErrLiveUnsupportedCodec, goshl.ErrLiveNoData, goshl.ErrLiveCapacity, goshl.ErrLiveStopped, goshl.ErrLiveSegmentNotFound, goshl.ErrLiveInvalidOptions, goshl.ErrInvalidChannelProfile, goshl.ErrChannelDisabled}
	_ = []goshl.LiveStatus{goshl.LiveStarting, goshl.LiveRunning, goshl.LiveFailed, goshl.LiveStopped}
	_ = goshl.Options{ChannelPoolSize: 1, ChannelSegmentsPerJob: 1, EncoderSlots: 1, LiveMaxSessions: 1, LiveMaxTranscodes: 1}
	_ = []interface{}{goshl.StreamChannel, goshl.PriorityBackground, goshl.PriorityPlayback, goshl.PriorityChannel, goshl.ChannelCodecs, goshl.LiveRendition, goshl.JobPriority(0), goshl.Job{Priority: goshl.PriorityChannel, Channel: &goshl.ChannelSpec{SourceFrameRate: 24}}}
	t.Log(job.StartBy)
}
