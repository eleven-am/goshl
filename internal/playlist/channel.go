package playlist

import "github.com/eleven-am/goshl/internal/domain"

func ChannelFrames(duration float64, profile domain.ChannelProfile) int {
	if !(duration > 0) {
		return 0
	}
	return domain.ChannelFrameCount(duration, profile.FrameRateNum, profile.FrameRateDen)
}

func CalculateChannelSegments(duration float64, profile domain.ChannelProfile) []domain.Segment {
	total := ChannelFrames(duration, profile)
	if total == 0 || profile.SegmentFrames <= 0 {
		return nil
	}
	count := total / profile.SegmentFrames
	remainder := total % profile.SegmentFrames
	if remainder != 0 && (count == 0 || remainder >= profile.SliverFrames()) {
		count++
	}
	segments := make([]domain.Segment, count)
	for index := range segments {
		startFrame := index * profile.SegmentFrames
		endFrame := startFrame + profile.SegmentFrames
		if index == count-1 {
			endFrame = total
		}
		start := domain.ChannelFrameTime(startFrame, profile.FrameRateNum, profile.FrameRateDen)
		end := domain.ChannelFrameTime(endFrame, profile.FrameRateNum, profile.FrameRateDen)
		segments[index] = domain.Segment{Index: index, Start: start, End: end, Duration: end - start}
	}
	return segments
}

func ChannelBatch(index, segmentsPerJob, count int) (start, end int) {
	start = index / segmentsPerJob * segmentsPerJob
	end = start + segmentsPerJob - 1
	if end > count-1 {
		end = count - 1
	}
	return start, end
}
