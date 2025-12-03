package transcode

import (
	"context"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

type stubCoordinator struct {
	publishes []domain.SegmentStatus
}

func (s *stubCoordinator) Enqueue(ctx context.Context, job domain.Job) error {
	return nil
}
func (s *stubCoordinator) Subscribe(ctx context.Context, streamType domain.StreamType) (<-chan domain.Job, error) {
	ch := make(chan domain.Job)
	close(ch)
	return ch, nil
}
func (s *stubCoordinator) Ack(ctx context.Context, jobID string) error {
	return nil
}
func (s *stubCoordinator) NotifySegment(ctx context.Context, info domain.SegmentData, status domain.SegmentStatus) error {
	s.publishes = append(s.publishes, status)
	return nil
}
func (s *stubCoordinator) WaitSegment(ctx context.Context, info domain.SegmentData) (<-chan domain.SegmentStatus, error) {
	ch := make(chan domain.SegmentStatus)
	close(ch)
	return ch, nil
}
func (s *stubCoordinator) Close() {}

func TestExtractSegmentsRespectsRangeAndDuration(t *testing.T) {
	p := &Pool{}
	keyframes := []float64{0, 2, 4, 9, 15}
	segments := p.extractSegments(keyframes, 16, 1, 2)

	if len(segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segments))
	}
	if segments[0].Index != 1 || segments[0].Start != 9 || segments[0].End != 15 {
		t.Fatalf("unexpected first segment: %#v", segments[0])
	}
	if segments[1].Index != 2 || segments[1].End != 16 {
		t.Fatalf("unexpected second segment: %#v", segments[1])
	}
}

func TestFindNearestKeyframePrefersPreviousWhenVeryClose(t *testing.T) {
	keyframes := []float64{5, 10, 15}
	got := findNearestKeyframe(keyframes, 10.005)
	if got != 5 {
		t.Fatalf("expected fallback to previous keyframe, got %v", got)
	}
}

func TestFindVideoAndAudioRenditions(t *testing.T) {
	p := &Pool{}
	meta := &domain.Metadata{Video: domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000}, Audios: []domain.AudioStream{{Codec: "ac3", Channels: 6, Bitrate: 640_000}}}

	if p.findVideoRendition(meta, "720p") == nil {
		t.Fatalf("expected to find 720p rendition")
	}
	if p.findVideoRendition(meta, "unknown") != nil {
		t.Fatalf("should return nil for missing video rendition")
	}

	if p.findAudioRendition(meta, "aac_stereo") == nil {
		t.Fatalf("expected stereo audio rendition")
	}
	if p.findAudioRendition(&domain.Metadata{}, "aac_stereo") != nil {
		t.Fatalf("should return nil when no audio present")
	}
}

func TestPublishErrorSendsRange(t *testing.T) {
	coord := &stubCoordinator{}
	p := &Pool{coordinator: coord, streamType: domain.StreamVideo}
	job := domain.Job{Rendition: "1080p", StartIndex: 2, EndIndex: 4}

	p.publishError(context.Background(), job, assertErr("boom"))

	if len(coord.publishes) != 3 {
		t.Fatalf("expected publish per segment, got %d", len(coord.publishes))
	}
	for _, status := range coord.publishes {
		if status.State != domain.SegmentStateError || status.Error == "" {
			t.Fatalf("status missing error: %#v", status)
		}
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
