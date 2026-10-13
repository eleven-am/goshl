package rendition

import (
	"fmt"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/h264level"
)

type bounds struct {
	min int
	max int
}

var targetHeights = []int{2160, 1080, 720, 480, 360}

var bitrateBounds = map[int]bounds{
	2160: {min: 8000000, max: 20000000},
	1080: {min: 2000000, max: 8000000},
	720:  {min: 1000000, max: 4000000},
	480:  {min: 500000, max: 2000000},
	360:  {min: 300000, max: 1000000},
}

var directStreamCodecs = map[string]bool{
	"h264": true,
}

func GenerateVideo(video domain.VideoStream) []domain.VideoRendition {
	var renditions []domain.VideoRendition

	srcWidth := video.Width
	srcHeight := video.Height
	srcBitrate := video.Bitrate
	srcPixels := srcWidth * srcHeight
	srcCodec := video.Codec

	if srcBitrate <= 0 {
		srcBitrate = estimateBitrate(srcHeight)
	}

	heights := make([]int, 0, len(targetHeights)+1)
	if srcHeight > 0 {
		heights = append(heights, srcHeight)
	}
	for _, height := range targetHeights {
		if height < srcHeight {
			heights = append(heights, height)
		}
	}

	for _, targetHeight := range heights {

		targetWidth := calculateWidth(srcWidth, srcHeight, targetHeight)
		targetPixels := targetWidth * targetHeight

		ratio := float64(targetPixels) / float64(srcPixels)
		bitrate := int(float64(srcBitrate) * ratio)

		if targetHeight == srcHeight {
			bitrate = srcBitrate
		} else {
			bitrate = clampBitrate(targetHeight, bitrate)
		}

		method := domain.Transcode
		if directStreamCodecs[srcCodec] && targetHeight == srcHeight && directStreamCompatible(video) {
			method = domain.DirectStream
		}
		peakBitrate := int(float64(bitrate) * 1.5)
		level := h264level.Minimum(targetWidth, targetHeight, video.FrameRate, peakBitrate)
		codec := videoCodecString(level)
		if method == domain.DirectStream {
			codec = sourceVideoCodecString(video)
			level = video.Level
		}

		renditions = append(renditions, domain.VideoRendition{
			Name:        fmt.Sprintf("%dp", targetHeight),
			Width:       targetWidth,
			Height:      targetHeight,
			Bitrate:     bitrate,
			PeakBitrate: peakBitrate,
			FrameRate:   video.FrameRate,
			Codec:       codec,
			Level:       level,
			Method:      method,
		})
	}

	return renditions
}

func directStreamCompatible(video domain.VideoStream) bool {
	if video.Codec != "h264" || video.BitDepth != 0 && video.BitDepth != 8 {
		return false
	}
	if video.Level <= 0 || video.Level > 52 {
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
	profile := strings.ToLower(video.Profile)
	return profile == "baseline" || profile == "constrained baseline" || profile == "main" || profile == "high"
}

func sourceVideoCodecString(video domain.VideoStream) string {
	profile := 100
	switch strings.ToLower(video.Profile) {
	case "baseline", "constrained baseline":
		profile = 66
	case "main":
		profile = 77
	}
	level := video.Level
	if level <= 0 {
		level = 40
	}
	return fmt.Sprintf("avc1.%02x00%02x", profile, level)
}

func videoCodecString(level int) string {
	return fmt.Sprintf("avc1.6400%02x", level)
}

func calculateWidth(srcWidth, srcHeight, targetHeight int) int {
	aspectRatio := float64(srcWidth) / float64(srcHeight)
	width := int(float64(targetHeight) * aspectRatio)
	if width%2 != 0 {
		width++
	}
	return width
}

func clampBitrate(height, bitrate int) int {
	b, ok := bitrateBounds[height]
	if !ok {
		return bitrate
	}
	if bitrate < b.min {
		return b.min
	}
	if bitrate > b.max {
		return b.max
	}
	return bitrate
}

func estimateBitrate(height int) int {
	switch {
	case height >= 2160:
		return 15000000
	case height >= 1080:
		return 5000000
	case height >= 720:
		return 2500000
	case height >= 480:
		return 1200000
	default:
		return 800000
	}
}

var passthroughCodecs = map[string]bool{
	"ac3":  true,
	"eac3": true,
}

func GenerateAudio(audio domain.AudioStream) []domain.AudioRendition {
	return generateAudio(audio, 0, false, true)
}

func GenerateAudioTracks(audios []domain.AudioStream) []domain.AudioRendition {
	var renditions []domain.AudioRendition

	for i, audio := range audios {
		prefixNames := len(audios) > 1
		streamIndex := audio.Index
		if i > 0 && streamIndex == 0 {
			streamIndex = i
		}
		renditions = append(renditions, generateAudio(audio, streamIndex, prefixNames, i == 0 || audio.Default)...)
	}

	return renditions
}

func generateAudio(audio domain.AudioStream, streamIndex int, prefixNames bool, defaultTrack bool) []domain.AudioRendition {
	var renditions []domain.AudioRendition

	renditions = append(renditions, audioRendition("aac_stereo", "aac", 128000, 2, domain.Transcode, audio, streamIndex, prefixNames, defaultTrack))

	if audio.Channels >= 6 {
		renditions = append(renditions, audioRendition("aac_surround", "aac", 384000, 6, domain.Transcode, audio, streamIndex, prefixNames, false))
	}

	if passthroughCodecs[audio.Codec] {
		renditions = append(renditions, audioRendition(audio.Codec+"_passthrough", audio.Codec, audio.Bitrate, audio.Channels, domain.DirectStream, audio, streamIndex, prefixNames, false))
	}

	return renditions
}

func audioRendition(name, codec string, bitrate, channels int, method domain.PlaybackMethod, audio domain.AudioStream, streamIndex int, prefixNames bool, isDefault bool) domain.AudioRendition {
	if prefixNames {
		name = fmt.Sprintf("audio%d_%s", streamIndex, name)
	}

	return domain.AudioRendition{
		Name:        name,
		Codec:       codec,
		CodecString: audioCodecString(codec),
		Bitrate:     bitrate,
		Channels:    channels,
		Method:      method,
		StreamIndex: streamIndex,
		Language:    audio.Language,
		Default:     isDefault,
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
