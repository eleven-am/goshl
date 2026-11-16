package goshl

import (
	"context"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/live"
)

type (
	LiveSource = live.Source

	LiveOptions = live.Options

	LiveSession = live.Session

	LiveState = live.State

	LiveStatus = live.Status

	LiveProbeInfo = live.ProbeInfo

	LiveVideoInfo = live.VideoInfo

	LiveAudioInfo = live.AudioInfo

	LiveAction = live.Action
)

const (
	LiveStarting = live.StatusStarting

	LiveRunning = live.StatusRunning

	LiveFailed = live.StatusFailed

	LiveStopped = live.StatusStopped

	LiveCopy = live.ActionCopy

	LiveTranscode = live.ActionTranscode

	LiveNone = live.ActionNone

	LiveRendition = live.RenditionName

	EventLiveStarted = domain.EventLiveStarted

	EventLiveReady = domain.EventLiveReady

	EventLiveReconnecting = domain.EventLiveReconnecting

	EventLiveSwitched = domain.EventLiveSwitched

	EventLiveFailed = domain.EventLiveFailed

	EventLiveStopped = domain.EventLiveStopped
)

var (
	ErrLiveSourceUnavailable = live.ErrSourceUnavailable
	ErrLiveUnsupportedCodec  = live.ErrUnsupportedCodec
	ErrLiveNoData            = live.ErrNoData
	ErrLiveCapacity          = live.ErrCapacity
	ErrLiveStopped           = live.ErrStopped
	ErrLiveSegmentNotFound   = live.ErrSegmentNotFound
	ErrLiveInvalidOptions    = live.ErrInvalidOptions
)

func (c *Controller) Live(ctx context.Context, key string, src LiveSource, opts LiveOptions) (*LiveSession, error) {
	return c.engine.Live(ctx, key, src, opts)
}

func (c *Controller) LiveProbe(ctx context.Context, src LiveSource, budget time.Duration) (LiveProbeInfo, error) {
	return c.engine.LiveProbe(ctx, src, budget)
}
