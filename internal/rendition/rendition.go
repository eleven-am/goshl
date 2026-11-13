package rendition

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/h264level"
	"github.com/eleven-am/goshl/internal/playlist"
)

var ErrNoEncodableVideo = errors.New("no encodable video rendition")

type class struct {
	shortSide       int
	minBitrate      int
	maxBitrate      int
	estimateBitrate int
}

var classes = []class{
	{shortSide: 2160, minBitrate: 8_000_000, maxBitrate: 20_000_000, estimateBitrate: 15_000_000},
	{shortSide: 1080, minBitrate: 2_000_000, maxBitrate: 8_000_000, estimateBitrate: 5_000_000},
	{shortSide: 720, minBitrate: 1_000_000, maxBitrate: 4_000_000, estimateBitrate: 2_500_000},
	{shortSide: 480, minBitrate: 500_000, maxBitrate: 2_000_000, estimateBitrate: 1_200_000},
	{shortSide: 360, minBitrate: 300_000, maxBitrate: 1_000_000, estimateBitrate: 800_000},
}

var passthroughCeilings = map[string]int{
	"ac3":  640_000,
	"eac3": 1_536_000,
}

type Dropped struct {
	Name   string
	Reason string
}

func GenerateVideo(meta *domain.Metadata, targetDuration float64) ([]domain.VideoRendition, []Dropped, error) {
	video := meta.Video
	if video.Width <= 0 || video.Height <= 0 {
		return nil, nil, nil
	}

	sourceEnd := videoEndBeforePlan(meta)
	direct := directStreamCompatible(video) && sourceEnd == 0 && playlist.KeyframeAligned(meta.Keyframes, meta.Duration, targetDuration)
	sourceBitrate := sourceVideoBitrate(meta)

	width, height := video.Width, video.Height
	if !direct {
		width, height = evenDown(width), evenDown(height)
	}
	sourceShort := min(width, height)
	sourceClass := classFor(sourceShort)

	colour := domain.PlanColour(video)
	window := planWindow(meta, targetDuration)
	var renditions []domain.VideoRendition
	var dropped []Dropped

	topBitrate := sourceBitrate
	if !direct {
		topBitrate = min(sourceBitrate, sourceClass.maxBitrate)
	}
	ceiling := topBitrate
	if direct {
		renditions = append(renditions, directRendition(video, topBitrate))
		renditions[0].PeakBandwidth = directPeakBandwidth(meta, window)
		renditions[0].AverageBandwidth = min(encodedAverageBandwidth(renditions[0]), renditions[0].PeakBandwidth)
	} else if top, err := transcodedRendition(width, height, topBitrate, video.FrameRate); err != nil {
		dropped = append(dropped, Dropped{Name: rungName(width, height), Reason: err.Error()})
	} else {
		top.Colour = colour
		top.SourceEnd = sourceEnd
		top.PeakBandwidth = encodedPeakBandwidth(top, window)
		top.AverageBandwidth = encodedAverageBandwidth(top)
		renditions = append(renditions, top)
	}

	sourcePixels := float64(width) * float64(height)
	for _, rung := range classes {
		if rung.shortSide >= sourceShort {
			continue
		}
		rungWidth, rungHeight := scaleToShortSide(video.Width, video.Height, rung.shortSide)
		ratio := float64(rungWidth) * float64(rungHeight) / sourcePixels
		bitrate := min(clamp(int(float64(topBitrate)*ratio), rung.minBitrate, rung.maxBitrate), ceiling)
		candidate, err := transcodedRendition(rungWidth, rungHeight, bitrate, video.FrameRate)
		if err != nil {
			dropped = append(dropped, Dropped{Name: rungName(rungWidth, rungHeight), Reason: err.Error()})
			continue
		}
		candidate.Colour = colour
		candidate.SourceEnd = sourceEnd
		candidate.PeakBandwidth = encodedPeakBandwidth(candidate, window)
		candidate.AverageBandwidth = encodedAverageBandwidth(candidate)
		renditions = append(renditions, candidate)
		ceiling = bitrate
	}

	if len(renditions) == 0 {
		reasons := make([]string, len(dropped))
		for index, rung := range dropped {
			reasons[index] = rung.Reason
		}
		return nil, dropped, fmt.Errorf("%w: %s", ErrNoEncodableVideo, strings.Join(reasons, "; "))
	}
	return renditions, dropped, nil
}

func videoEndBeforePlan(meta *domain.Metadata) float64 {
	if meta.Duration-meta.Video.End <= domain.StreamTailSlack {
		return 0
	}
	return math.Max(meta.Video.End, 0)
}

func directRendition(video domain.VideoStream, bitrate int) domain.VideoRendition {
	return domain.VideoRendition{
		Name:               rungName(video.Width, video.Height),
		Width:              video.Width,
		Height:             video.Height,
		Bitrate:            bitrate,
		PeakBitrate:        peakBitrate(bitrate),
		FrameRate:          video.FrameRate,
		Codec:              sourceVideoCodecString(video),
		Level:              video.Level,
		Method:             domain.DirectStream,
		StripParameterSets: video.CodecTag == "avc1",
	}
}

func transcodedRendition(width, height, bitrate int, frameRate float64) (domain.VideoRendition, error) {
	name := rungName(width, height)
	peak := peakBitrate(bitrate)
	level, ok := h264level.Minimum(width, height, frameRate, peak)
	if !ok {
		return domain.VideoRendition{}, fmt.Errorf("%s (%dx%d at %.3f fps, peak %d) needs an H.264 level above %.1f", name, width, height, frameRate, peak, float64(h264level.Ceiling)/10)
	}
	return domain.VideoRendition{
		Name:        name,
		Width:       width,
		Height:      height,
		Bitrate:     bitrate,
		PeakBitrate: peak,
		BufferSize:  min(2*peak, h264level.MaxCPB(level)),
		FrameRate:   frameRate,
		Codec:       fmt.Sprintf("avc1.6400%02x", level),
		Level:       level,
		Method:      domain.Transcode,
	}, nil
}

func rungName(width, height int) string {
	return fmt.Sprintf("%dp", min(width, height))
}

func peakBitrate(bitrate int) int {
	return bitrate + bitrate/2
}

func sourceVideoBitrate(meta *domain.Metadata) int {
	if meta.Video.Bitrate > 0 {
		return meta.Video.Bitrate
	}
	remaining := meta.Bitrate
	for _, audio := range meta.Audios {
		remaining -= audio.Bitrate
	}
	if meta.Bitrate > 0 && remaining > 0 {
		return remaining
	}
	return classFor(min(meta.Video.Width, meta.Video.Height)).estimateBitrate
}

func classFor(shortSide int) class {
	chosen := classes[0]
	for _, candidate := range classes {
		if candidate.shortSide >= shortSide {
			chosen = candidate
		}
	}
	return chosen
}

func directStreamCompatible(video domain.VideoStream) bool {
	if domain.HDRTransfer(video.ColorTransfer) {
		return false
	}
	if video.Codec != "h264" || video.BitDepth != 0 && video.BitDepth != 8 {
		return false
	}
	if video.Level <= 0 || video.Level > h264level.Ceiling {
		return false
	}
	if video.Rotation != 0 {
		return false
	}
	if video.SampleAspect != "" && video.SampleAspect != "1:1" && video.SampleAspect != "0:1" {
		return false
	}
	if video.PixelFormat != "yuv420p" && video.PixelFormat != "yuvj420p" {
		return false
	}
	if !video.StartsWithKeyframe {
		return false
	}
	_, known := sourceProfiles[strings.ToLower(video.Profile)]
	return known
}

var sourceProfiles = map[string]string{
	"baseline":             "4200",
	"constrained baseline": "4240",
	"main":                 "4d00",
	"high":                 "6400",
}

func sourceVideoCodecString(video domain.VideoStream) string {
	return fmt.Sprintf("avc1.%s%02x", sourceProfiles[strings.ToLower(video.Profile)], video.Level)
}

func scaleToShortSide(width, height, shortSide int) (int, int) {
	if width >= height {
		return evenUp(int(float64(shortSide) * float64(width) / float64(height))), shortSide
	}
	return shortSide, evenUp(int(float64(shortSide) * float64(height) / float64(width)))
}

func evenUp(value int) int {
	return value + value%2
}

func evenDown(value int) int {
	return max(value-value%2, 2)
}

func clamp(value, low, high int) int {
	return min(max(value, low), high)
}

func GenerateAudioTracks(audios []domain.AudioStream) []domain.AudioRendition {
	var renditions []domain.AudioRendition

	defaultTrack := 0
	for i, audio := range audios {
		if audio.Default {
			defaultTrack = i
			break
		}
	}
	for i, audio := range audios {
		prefixNames := len(audios) > 1
		streamIndex := audio.Index
		if i > 0 && streamIndex == 0 {
			streamIndex = i
		}
		renditions = append(renditions, generateAudio(audio, streamIndex, prefixNames, i == defaultTrack)...)
	}

	return renditions
}

func generateAudio(audio domain.AudioStream, streamIndex int, prefixNames bool, defaultTrack bool) []domain.AudioRendition {
	var renditions []domain.AudioRendition

	renditions = append(renditions, audioRendition("aac_stereo", "aac", 128000, 2, domain.Transcode, audio, streamIndex, prefixNames, defaultTrack))

	if audio.Channels >= 6 {
		renditions = append(renditions, audioRendition("aac_surround", "aac", 384000, 6, domain.Transcode, audio, streamIndex, prefixNames, defaultTrack))
	}

	if ceiling, passthrough := passthroughCeilings[audio.Codec]; passthrough {
		bitrate := audio.Bitrate
		if bitrate <= 0 {
			bitrate = ceiling
		}
		renditions = append(renditions, audioRendition(audio.Codec+"_passthrough", audio.Codec, bitrate, audio.Channels, domain.DirectStream, audio, streamIndex, prefixNames, defaultTrack))
	}

	return renditions
}

func audioRendition(name, codec string, bitrate, channels int, method domain.PlaybackMethod, audio domain.AudioStream, streamIndex int, prefixNames bool, isDefault bool) domain.AudioRendition {
	if prefixNames {
		name = fmt.Sprintf("audio%d_%s", streamIndex, name)
	}

	return domain.AudioRendition{
		Name:             name,
		Codec:            codec,
		CodecString:      audioCodecString(codec),
		Bitrate:          bitrate,
		PeakBandwidth:    audioPeakBandwidth(bitrate, audio.SampleRate, method),
		AverageBandwidth: audioAverageBandwidth(bitrate, audio.SampleRate, method),
		Channels:         channels,
		Method:           method,
		StreamIndex:      streamIndex,
		Language:         audio.Language,
		Title:            audio.Title,
		Default:          isDefault,
		SourceEnd:        audio.End,
		SampleRate:       audio.SampleRate,
	}
}

func audioCodecString(codec string) string {
	switch codec {
	case "ac3":
		return "ac-3"
	case "eac3":
		return "ec-3"
	default:
		return "mp4a.40.2"
	}
}
