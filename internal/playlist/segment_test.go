package playlist

import (
	"math"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestCalculateSegments_NoKeyframes(t *testing.T) {
	if got := CalculateSegments(nil, 10, 6); got != nil {
		t.Fatalf("expected nil, got %#v", got)
	}
}

func TestCalculateSegments_SplitsAtTargetAndCompletesRemainder(t *testing.T) {
	keyframes := []float64{0, 1, 2.5, 4.9, 7.1}
	segments := CalculateSegments(keyframes, 8.0, 2.0)

	want := []domain.Segment{
		{Index: 0, Start: 0, End: 2.5, Duration: 2.5},
		{Index: 1, Start: 2.5, End: 4.9, Duration: 2.4},
		{Index: 2, Start: 4.9, End: 7.1, Duration: 2.2},
		{Index: 3, Start: 7.1, End: 8.0, Duration: 0.9},
	}

	if len(segments) != len(want) {
		t.Fatalf("expected %d segments, got %d", len(want), len(segments))
	}

	for i, seg := range segments {
		if seg.Index != want[i].Index {
			t.Fatalf("segment %d index mismatch: want %d got %d", i, want[i].Index, seg.Index)
		}
		if !almostEqual(seg.Start, want[i].Start) || !almostEqual(seg.End, want[i].End) || !almostEqual(seg.Duration, want[i].Duration) {
			t.Fatalf("segment %d mismatch: want %#v got %#v", i, want[i], seg)
		}
	}
}

func TestCalculateSegments_SingleKeyframeCreatesTailSegment(t *testing.T) {
	keyframes := []float64{1.0}
	segments := CalculateSegments(keyframes, 4.0, 6.0)

	want := domain.Segment{Index: 0, Start: 1.0, End: 4.0, Duration: 3.0}
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if segments[0].Index != want.Index || !almostEqual(segments[0].Start, want.Start) || !almostEqual(segments[0].End, want.End) || !almostEqual(segments[0].Duration, want.Duration) {
		t.Fatalf("unexpected segment: want %#v got %#v", want, segments[0])
	}
}

func almostEqual(a, b float64) bool {
	const eps = 1e-9
	return math.Abs(a-b) <= eps
}
