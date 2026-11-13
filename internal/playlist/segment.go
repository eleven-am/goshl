package playlist

import (
	"math"

	"github.com/eleven-am/goshl/internal/domain"
)

const (
	maxSegmentFloor = 12.0
	maxSliverLength = 0.5
)

func MaxSegmentDuration(targetDuration float64) float64 {
	return math.Max(2*targetDuration, maxSegmentFloor)
}

func CalculateSegments(keyframes []float64, duration float64, targetDuration float64) []domain.Segment {
	segments, _ := planSegments(keyframes, duration, targetDuration)
	return segments
}

func KeyframeAligned(keyframes []float64, duration float64, targetDuration float64) bool {
	_, aligned := planSegments(keyframes, duration, targetDuration)
	return aligned
}

func sliverLength(targetDuration float64) float64 {
	return math.Min(maxSliverLength, targetDuration/2)
}

func planSegments(keyframes []float64, duration float64, targetDuration float64) ([]domain.Segment, bool) {
	if !(duration > 0) {
		return nil, false
	}
	if !(targetDuration > 0) {
		targetDuration = duration
	}
	sliver := sliverLength(targetDuration)
	if len(keyframes) == 0 {
		return buildSegments(mergeTail(fixedBoundaries(duration, targetDuration), sliver)), false
	}

	limit := MaxSegmentDuration(targetDuration) + domain.BoundaryTolerance
	aligned := math.Abs(keyframes[0]) <= domain.BoundaryTolerance
	boundaries := []float64{0}
	start := 0.0
	cut := func(end float64) {
		if span := end - start; span > limit {
			aligned = false
			pieces := pieceCount(span, targetDuration)
			for piece := 1; piece < pieces; piece++ {
				boundaries = append(boundaries, start+span*float64(piece)/float64(pieces))
			}
		}
		boundaries = append(boundaries, end)
		start = end
	}

	candidates := keyframes[1:]
	for i, keyframe := range candidates {
		if keyframe >= duration {
			break
		}
		elapsed := keyframe - start
		if elapsed <= 0 {
			continue
		}
		next := duration
		if i+1 < len(candidates) && candidates[i+1] < duration {
			next = candidates[i+1]
		}
		if elapsed >= targetDuration || elapsed >= sliver && next-start > limit {
			cut(keyframe)
		}
	}
	cut(duration)

	return buildSegments(mergeTail(boundaries, sliver)), aligned
}

func fixedBoundaries(duration float64, targetDuration float64) []float64 {
	pieces := pieceCount(duration, targetDuration)
	boundaries := make([]float64, 0, pieces+1)
	for piece := 0; piece < pieces; piece++ {
		boundaries = append(boundaries, float64(piece)*targetDuration)
	}
	return append(boundaries, duration)
}

func pieceCount(span float64, targetDuration float64) int {
	pieces := int(math.Ceil(span/targetDuration - domain.BoundaryTolerance))
	if pieces < 1 {
		return 1
	}
	return pieces
}

func mergeTail(boundaries []float64, sliver float64) []float64 {
	last := len(boundaries) - 1
	if last < 2 || boundaries[last]-boundaries[last-1] >= sliver {
		return boundaries
	}
	boundaries[last-1] = boundaries[last]
	return boundaries[:last]
}

func buildSegments(boundaries []float64) []domain.Segment {
	segments := make([]domain.Segment, 0, len(boundaries)-1)
	for index := 0; index+1 < len(boundaries); index++ {
		segments = append(segments, domain.Segment{
			Index:    index,
			Start:    boundaries[index],
			End:      boundaries[index+1],
			Duration: boundaries[index+1] - boundaries[index],
		})
	}
	return segments
}
