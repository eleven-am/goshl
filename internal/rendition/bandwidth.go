package rendition

import (
	"math"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/h264level"
	"github.com/eleven-am/goshl/internal/playlist"
)

const (
	transportPacketBytes  = 188
	transportPayloadBytes = 184
	transportFrameBytes   = 216
	transportSecondBytes  = 4700
	transportSegmentBytes = 1128
	encodedAudioPeak      = 1.25
	defaultAudioRate      = 48000
)

type peakWindow struct {
	segments []domain.Segment
	shortest float64
	longest  float64
}

func planWindow(meta *domain.Metadata, targetDuration float64) peakWindow {
	segments := playlist.CalculateSegments(meta.Keyframes, meta.Duration, targetDuration)
	longest := 0.0
	for _, segment := range segments {
		longest = math.Max(longest, segment.Duration)
	}
	target := math.Ceil(longest)
	if target <= 0 {
		target = math.Max(targetDuration, 1)
	}
	return peakWindow{segments: segments, shortest: target / 2, longest: target * 3 / 2}
}

func transportBits(payloadBytes, frames, seconds float64) float64 {
	bytes := payloadBytes*transportPacketBytes/transportPayloadBytes + frames*transportFrameBytes + seconds*transportSecondBytes + transportSegmentBytes
	return bytes * 8
}

func encodedPeakBandwidth(rendition domain.VideoRendition, window peakWindow) int {
	seconds := window.shortest
	payload := (float64(rendition.PeakBitrate)*seconds + float64(rendition.BufferSize)) / 8
	frames := math.Ceil(rendition.FrameRate*seconds) + 1
	return int(math.Ceil(transportBits(payload, frames, seconds) / seconds))
}

func levelCeilingBandwidth(video domain.VideoStream, window peakWindow) int {
	cpb := max(h264level.MaxCPB(video.Level), h264level.MaxCPB(h264level.Floor))
	seconds := window.shortest
	frames := math.Ceil(video.FrameRate*seconds) + 1
	return int(math.Ceil(transportBits(float64(cpb)*seconds/8, frames, seconds) / seconds))
}

func directPeakBandwidth(meta *domain.Metadata, window peakWindow) int {
	if len(meta.KeyframeBytes) != len(meta.Keyframes) || len(meta.Keyframes) == 0 || len(window.segments) == 0 {
		return levelCeilingBandwidth(meta.Video, window)
	}
	bits := make([]float64, len(window.segments))
	next := 0
	for position, segment := range window.segments {
		payload := 0.0
		for next < len(meta.Keyframes) && meta.Keyframes[next] < segment.End-domain.BoundaryTolerance {
			payload += float64(meta.KeyframeBytes[next])
			next++
		}
		frames := math.Ceil(meta.Video.FrameRate*segment.Duration) + 1
		bits[position] = transportBits(payload, frames, segment.Duration)
	}
	peak := 0.0
	whole, total := 0.0, 0.0
	for first := range window.segments {
		sum, seconds := 0.0, 0.0
		for last := first; last < len(window.segments); last++ {
			sum += bits[last]
			seconds += window.segments[last].Duration
			if seconds > window.longest && last > first {
				break
			}
			if seconds >= window.shortest {
				peak = math.Max(peak, sum/seconds)
			}
		}
		whole += bits[first]
		total += window.segments[first].Duration
	}
	if peak == 0 && total > 0 {
		peak = whole / total
	}
	return int(math.Ceil(peak))
}

func audioPeakBandwidth(bitrate, sampleRate int, method domain.PlaybackMethod) int {
	rate := float64(defaultAudioRate)
	samples := 1024.0
	payload := float64(bitrate) * encodedAudioPeak
	if method == domain.DirectStream {
		samples = 1536
		payload = float64(bitrate)
		if sampleRate > 0 {
			rate = float64(sampleRate)
		}
	}
	return int(math.Ceil(transportBits(payload/8, rate/samples+1, 1)))
}
