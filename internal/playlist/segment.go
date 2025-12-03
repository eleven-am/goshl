package playlist

import "github.com/eleven-am/goshl/internal/domain"

func CalculateSegments(keyframes []float64, duration float64, targetDuration float64) []domain.Segment {
	if len(keyframes) == 0 {
		return nil
	}

	var segments []domain.Segment
	segmentStart := keyframes[0]
	segmentIndex := 0

	for i := 1; i < len(keyframes); i++ {
		elapsed := keyframes[i] - segmentStart

		if elapsed >= targetDuration {
			segments = append(segments, domain.Segment{
				Index:    segmentIndex,
				Start:    segmentStart,
				End:      keyframes[i],
				Duration: keyframes[i] - segmentStart,
			})
			segmentStart = keyframes[i]
			segmentIndex++
		}
	}

	if segmentStart < duration {
		segments = append(segments, domain.Segment{
			Index:    segmentIndex,
			Start:    segmentStart,
			End:      duration,
			Duration: duration - segmentStart,
		})
	}

	return segments
}
