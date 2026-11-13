package playlist

import (
	"math"
	"math/rand"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestCalculateSegments_NoKeyframes(t *testing.T) {
	got := CalculateSegments(nil, 10, 6)
	want := []domain.Segment{
		{Index: 0, Start: 0, End: 6, Duration: 6},
		{Index: 1, Start: 6, End: 10, Duration: 4},
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d segments, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i].Index != want[i].Index || !almostEqual(got[i].Start, want[i].Start) || !almostEqual(got[i].End, want[i].End) || !almostEqual(got[i].Duration, want[i].Duration) {
			t.Fatalf("segment %d mismatch: want %#v got %#v", i, want[i], got[i])
		}
	}
}

func TestCalculateSegments_NoKeyframesNoDuration(t *testing.T) {
	if got := CalculateSegments(nil, 0, 6); got != nil {
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

	want := domain.Segment{Index: 0, Start: 0, End: 4.0, Duration: 4.0}
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

func requireContiguous(t *testing.T, segments []domain.Segment, duration float64) {
	t.Helper()
	if len(segments) == 0 {
		t.Fatalf("no segments")
	}
	if segments[0].Start != 0 {
		t.Fatalf("first segment starts at %v", segments[0].Start)
	}
	for i, segment := range segments {
		if segment.Index != i {
			t.Fatalf("segment %d has index %d", i, segment.Index)
		}
		if !(segment.End > segment.Start) || math.Abs(segment.Duration-(segment.End-segment.Start)) > 1e-9 {
			t.Fatalf("segment %d is malformed: %#v", i, segment)
		}
		if i > 0 && segment.Start != segments[i-1].End {
			t.Fatalf("segment %d starts at %v, previous ends at %v", i, segment.Start, segments[i-1].End)
		}
	}
	if last := segments[len(segments)-1]; last.End != duration {
		t.Fatalf("last segment ends at %v, want %v", last.End, duration)
	}
}

func TestMaxSegmentDuration(t *testing.T) {
	for target, want := range map[float64]float64{2: 12, 6: 12, 10: 20} {
		if got := MaxSegmentDuration(target); got != want {
			t.Fatalf("MaxSegmentDuration(%v) = %v, want %v", target, got, want)
		}
	}
}

func TestCalculateSegmentsSplitsLongKeyframeGaps(t *testing.T) {
	segments := CalculateSegments([]float64{0, 40, 46}, 52, 6)
	requireContiguous(t, segments, 52)
	if len(segments) != 9 {
		t.Fatalf("expected 9 segments, got %d: %#v", len(segments), segments)
	}
	for i, segment := range segments[:7] {
		if !almostEqual(segment.Start, 40*float64(i)/7) || segment.Duration > 6 {
			t.Fatalf("segment %d is not an equal piece of the gap: %#v", i, segment)
		}
	}
	if segments[7].Start != 40 || segments[8].Start != 46 {
		t.Fatalf("keyframe boundaries after the gap were lost: %#v", segments[7:])
	}
}

func TestCalculateSegmentsSplitsLongTail(t *testing.T) {
	segments := CalculateSegments([]float64{0, 6}, 60, 6)
	requireContiguous(t, segments, 60)
	if len(segments) != 10 {
		t.Fatalf("expected 10 segments, got %d: %#v", len(segments), segments)
	}
	for _, segment := range segments {
		if segment.Duration > 6+1e-9 {
			t.Fatalf("segment longer than target: %#v", segment)
		}
	}
}

func TestCalculateSegmentsCutsEarlyToStayKeyframeAligned(t *testing.T) {
	keyframes := []float64{0, 4, 14, 24}
	segments := CalculateSegments(keyframes, 30, 6)
	requireContiguous(t, segments, 30)
	want := []float64{0, 4, 14, 24}
	if len(segments) != len(want) {
		t.Fatalf("expected %d segments, got %d: %#v", len(want), len(segments), segments)
	}
	for i, start := range want {
		if segments[i].Start != start {
			t.Fatalf("segment %d starts at %v, want %v", i, segments[i].Start, start)
		}
	}
	if !KeyframeAligned(keyframes, 30, 6) {
		t.Fatalf("plan cut only at keyframes must be keyframe aligned")
	}
}

func TestCalculateSegmentsMergesTailSliver(t *testing.T) {
	segments := CalculateSegments([]float64{0, 6, 12}, 12.2, 6)
	requireContiguous(t, segments, 12.2)
	if len(segments) != 2 || segments[1].Start != 6 {
		t.Fatalf("expected the 0.2 s tail merged into [6,12.2], got %#v", segments)
	}
	if !KeyframeAligned([]float64{0, 6, 12}, 12.2, 6) {
		t.Fatalf("merging the tail must keep the plan keyframe aligned")
	}

	fixed := CalculateSegments(nil, 12.3, 6)
	requireContiguous(t, fixed, 12.3)
	if len(fixed) != 2 || fixed[1].Start != 6 {
		t.Fatalf("expected the 0.3 s fixed tail merged into [6,12.3], got %#v", fixed)
	}

	kept := CalculateSegments([]float64{0, 6, 12}, 12.5, 6)
	if len(kept) != 3 {
		t.Fatalf("a 0.5 s tail must be kept, got %#v", kept)
	}

	single := CalculateSegments([]float64{0}, 0.2, 6)
	if len(single) != 1 || single[0].End != 0.2 {
		t.Fatalf("a lone short segment must be kept, got %#v", single)
	}
}

func TestFixedSegmentsHaveNoFloatDrift(t *testing.T) {
	tenths := CalculateSegments(nil, 1.0, 0.1)
	requireContiguous(t, tenths, 1.0)
	if len(tenths) != 10 {
		t.Fatalf("expected 10 segments, got %d (last %#v)", len(tenths), tenths[len(tenths)-1])
	}

	const target = 6.006
	const count = 1200
	duration := target * count
	segments := CalculateSegments(nil, duration, target)
	requireContiguous(t, segments, duration)
	if len(segments) != count {
		t.Fatalf("expected %d segments, got %d", count, len(segments))
	}
	for i, segment := range segments {
		if segment.Start != float64(i)*target {
			t.Fatalf("segment %d starts at %.17g, want %.17g", i, segment.Start, float64(i)*target)
		}
	}
}

func TestKeyframeAlignedFalseWhenSplit(t *testing.T) {
	if KeyframeAligned([]float64{0, 40, 46}, 52, 6) {
		t.Fatalf("a split keyframe gap must not be keyframe aligned")
	}
	if KeyframeAligned([]float64{0, 6}, 60, 6) {
		t.Fatalf("a split tail must not be keyframe aligned")
	}
	if KeyframeAligned(nil, 60, 6) {
		t.Fatalf("a fixed plan must not be keyframe aligned")
	}
	if !KeyframeAligned([]float64{1, 7, 13}, 18, 6) {
		t.Fatalf("a plan whose first boundary has nothing before it stays keyframe aligned")
	}
	if KeyframeAligned([]float64{0, 6}, 0, 6) {
		t.Fatalf("an empty plan must not be keyframe aligned")
	}
	if !KeyframeAligned([]float64{0, 6, 12}, 18, 6) {
		t.Fatalf("a plan cut only at keyframes must be keyframe aligned")
	}
	if !KeyframeAligned([]float64{0, 12}, 24, 6) {
		t.Fatalf("a gap of exactly the maximum must stay keyframe aligned")
	}
}

func TestCalculateSegmentsInvariants(t *testing.T) {
	random := rand.New(rand.NewSource(23))
	for round := 0; round < 5000; round++ {
		target := []float64{2, 4, 6, 10}[random.Intn(4)]
		limit := MaxSegmentDuration(target)
		var keyframes []float64
		position := 0.0
		if random.Intn(8) == 0 {
			position = random.Float64() * 3
		}
		count := random.Intn(60)
		for i := 0; i < count; i++ {
			keyframes = append(keyframes, position)
			gap := 0.04 + random.Float64()*8
			if random.Intn(10) == 0 {
				gap = random.Float64() * 45
			}
			position += gap
		}
		duration := position + random.Float64()*2
		if random.Intn(10) == 0 {
			duration = position + random.Float64()*40
		}
		if duration <= 0 {
			continue
		}
		segments := CalculateSegments(keyframes, duration, target)
		requireContiguous(t, segments, duration)
		known := map[float64]bool{}
		for _, keyframe := range keyframes {
			known[keyframe] = true
		}
		onKeyframes := len(keyframes) > 0
		for i, segment := range segments {
			allowed := limit + 1e-6
			if i == len(segments)-1 {
				allowed += 0.5
			}
			if segment.Duration > allowed {
				t.Fatalf("round %d: segment %d lasts %v, limit %v (keyframes %v duration %v target %v)", round, i, segment.Duration, allowed, keyframes, duration, target)
			}
			if i == len(segments)-1 && len(segments) > 1 && segment.Duration < 0.5 {
				t.Fatalf("round %d: tail sliver %v survived", round, segment.Duration)
			}
			if i > 0 && !known[segment.Start] {
				onKeyframes = false
			}
		}
		if aligned := KeyframeAligned(keyframes, duration, target); aligned != onKeyframes {
			t.Fatalf("round %d: KeyframeAligned = %v but boundaries on keyframes = %v (keyframes %v duration %v target %v, segments %#v)", round, aligned, onKeyframes, keyframes, duration, target, segments)
		}
	}
}
