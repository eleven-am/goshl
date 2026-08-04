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

func TestPublicContractCompilesForExternalConsumers(t *testing.T) {
	var pathGenerator goshl.PathGenerator = publicPathGenerator{}
	var observer goshl.Observer = publicObserver{}
	job := goshl.Job{
		Segments:       []goshl.Segment{{Index: 1}},
		VideoRendition: &goshl.VideoRendition{Method: goshl.Transcode},
		AudioRendition: &goshl.AudioRendition{Method: goshl.DirectStream},
		EnqueuedAt:     time.Now(),
		Deadline:       time.Now(),
	}
	_ = pathGenerator
	_ = observer
	_ = job
	_ = goshl.EventSegmentNotifyFailed
	_ = goshl.SegmentStateReady
	_ = goshl.StreamSubtitle
	t.Log(job.Deadline)
}
