package domain

import (
	"errors"
	"fmt"
	"math"
)

type JobPriority int

const (
	PriorityBackground JobPriority = 0
	PriorityPlayback   JobPriority = 100
	PriorityChannel    JobPriority = 200
)

const (
	ChannelRenditionPrefix = "channel-"
	ChannelAudioSampleRate = 48000
	ChannelAudioChannels   = 2
	ChannelCodecs          = "avc1.64001f,mp4a.40.2"
)

const (
	channelDefaultHeight        = 720
	channelDefaultFrameRateNum  = 24000
	channelDefaultFrameRateDen  = 1001
	channelDefaultSegmentFrames = 48
	channelDefaultVideoBitrate  = 2_500_000
	channelDefaultAudioBitrate  = 160_000
	channelMinHeight            = 240
	channelMaxHeight            = 720
	channelMinFrameRate         = 1.0
	channelMaxFrameRate         = 60.0
	channelMinSegmentSeconds    = 1.0
	channelMaxSegmentSeconds    = 10.0
	channelMinVideoBitrate      = 200_000
	channelMaxVideoBitrate      = 20_000_000
	channelMinAudioBitrate      = 32_000
	channelMaxAudioBitrate      = 512_000
	channelSliverSeconds        = 0.5
)

var ErrInvalidChannelProfile = errors.New("invalid channel profile")

type ChannelProfile struct {
	Height        int
	FrameRateNum  int
	FrameRateDen  int
	SegmentFrames int
	VideoBitrate  int
	AudioBitrate  int
}

type ChannelPlan struct {
	MediaKey     string
	Rendition    string
	Codecs       string
	FrameRateNum int
	FrameRateDen int
	Frames       int
	Duration     float64
	Segments     []Segment
}

func (p ChannelPlan) SegmentData(index int) SegmentData {
	return SegmentData{MediaKey: p.MediaKey, Index: index, Rendition: p.Rendition, IsVideo: true}
}

type ChannelSpec struct {
	FormatVersion  int
	Width          int
	Height         int
	PictureWidth   int
	PictureHeight  int
	FrameRateNum   int
	FrameRateDen   int
	SegmentFrames  int
	SegmentsPerJob int
	TotalFrames    int
	AudioTrack     int
	Silent         bool
	Phase          float64

	SourceFrameRate float64
}

func (p ChannelProfile) Normalised() (ChannelProfile, error) {
	if p.Height == 0 {
		p.Height = channelDefaultHeight
	}
	if p.FrameRateNum == 0 && p.FrameRateDen == 0 {
		p.FrameRateNum, p.FrameRateDen = channelDefaultFrameRateNum, channelDefaultFrameRateDen
	}
	if p.FrameRateDen == 0 {
		p.FrameRateDen = 1
	}
	if p.SegmentFrames == 0 {
		p.SegmentFrames = channelDefaultSegmentFrames
	}
	if p.VideoBitrate == 0 {
		p.VideoBitrate = channelDefaultVideoBitrate
	}
	if p.AudioBitrate == 0 {
		p.AudioBitrate = channelDefaultAudioBitrate
	}
	if p.Height < channelMinHeight || p.Height > channelMaxHeight || p.Height%2 != 0 {
		return p, fmt.Errorf("%w: Height must be even and between %d and %d", ErrInvalidChannelProfile, channelMinHeight, channelMaxHeight)
	}
	if p.FrameRateNum <= 0 || p.FrameRateDen <= 0 {
		return p, fmt.Errorf("%w: frame rate must be positive", ErrInvalidChannelProfile)
	}
	if divisor := gcd(p.FrameRateNum, p.FrameRateDen); divisor > 1 {
		p.FrameRateNum, p.FrameRateDen = p.FrameRateNum/divisor, p.FrameRateDen/divisor
	}
	if rate := p.FrameRate(); rate < channelMinFrameRate || rate > channelMaxFrameRate {
		return p, fmt.Errorf("%w: frame rate must be between %g and %g", ErrInvalidChannelProfile, channelMinFrameRate, channelMaxFrameRate)
	}
	if p.SegmentFrames < 0 {
		return p, fmt.Errorf("%w: SegmentFrames must be positive", ErrInvalidChannelProfile)
	}
	if seconds := p.SegmentSeconds(); seconds < channelMinSegmentSeconds || seconds > channelMaxSegmentSeconds {
		return p, fmt.Errorf("%w: a segment must last between %g and %g seconds", ErrInvalidChannelProfile, channelMinSegmentSeconds, channelMaxSegmentSeconds)
	}
	if p.VideoBitrate < channelMinVideoBitrate || p.VideoBitrate > channelMaxVideoBitrate {
		return p, fmt.Errorf("%w: VideoBitrate must be between %d and %d", ErrInvalidChannelProfile, channelMinVideoBitrate, channelMaxVideoBitrate)
	}
	if p.AudioBitrate < channelMinAudioBitrate || p.AudioBitrate > channelMaxAudioBitrate {
		return p, fmt.Errorf("%w: AudioBitrate must be between %d and %d", ErrInvalidChannelProfile, channelMinAudioBitrate, channelMaxAudioBitrate)
	}
	return p, nil
}

func (p ChannelProfile) FrameRate() float64 {
	return float64(p.FrameRateNum) / float64(p.FrameRateDen)
}

func (p ChannelProfile) SegmentSeconds() float64 {
	return ChannelFrameTime(p.SegmentFrames, p.FrameRateNum, p.FrameRateDen)
}

func (p ChannelProfile) Width() int {
	width := (p.Height*16 + 8) / 9
	return width + width%2
}

func (p ChannelProfile) SliverFrames() int {
	return int(math.Round(channelSliverSeconds * p.FrameRate()))
}

func ChannelFrameTime(frames, frameRateNum, frameRateDen int) float64 {
	return float64(int64(frames)*int64(frameRateDen)) / float64(frameRateNum)
}

func ChannelFrameCount(duration float64, frameRateNum, frameRateDen int) int {
	frames := int(math.Round(duration * float64(frameRateNum) / float64(frameRateDen)))
	if frames < 1 {
		return 1
	}
	return frames
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (s ChannelSpec) SegmentSeconds() float64 {
	return ChannelFrameTime(s.SegmentFrames, s.FrameRateNum, s.FrameRateDen)
}
