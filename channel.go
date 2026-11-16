package goshl

import (
	"context"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/engine"
)

type (
	ChannelProfile = domain.ChannelProfile

	ChannelPlan = domain.ChannelPlan

	ChannelSpec = domain.ChannelSpec

	JobPriority = domain.JobPriority
)

const (
	StreamChannel = domain.StreamChannel

	PriorityBackground = domain.PriorityBackground

	PriorityPlayback = domain.PriorityPlayback

	PriorityChannel = domain.PriorityChannel

	ChannelCodecs = domain.ChannelCodecs
)

var (
	ErrInvalidChannelProfile = engine.ErrInvalidChannelProfile
	ErrChannelDisabled       = engine.ErrChannelDisabled
)

func (c *Controller) ChannelPlan(ctx context.Context, sourceURL string, profile ChannelProfile, audioTrack int) (ChannelPlan, error) {
	return c.engine.ChannelPlan(ctx, sourceURL, profile, audioTrack)
}

func (c *Controller) ChannelSegment(ctx context.Context, sourceURL string, profile ChannelProfile, audioTrack int, index int) ([]byte, error) {
	return c.engine.ChannelSegment(ctx, sourceURL, profile, audioTrack, index)
}

func (c *Controller) ChannelPrefetch(ctx context.Context, sourceURL string, profile ChannelProfile, audioTrack int, from int, count int) error {
	return c.engine.ChannelPrefetch(ctx, sourceURL, profile, audioTrack, from, count)
}
