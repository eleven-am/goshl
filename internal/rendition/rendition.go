package rendition

import (
	"fmt"

	"github.com/eleven-am/goshl/internal/domain"
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

	for _, targetHeight := range targetHeights {
		if targetHeight > srcHeight {
			continue
		}

		targetWidth := calculateWidth(srcWidth, srcHeight, targetHeight)
		targetPixels := targetWidth * targetHeight

		ratio := float64(targetPixels) / float64(srcPixels)
		bitrate := int(float64(srcBitrate) * ratio)

		bitrate = clampBitrate(targetHeight, bitrate)

		method := domain.Transcode
		if directStreamCodecs[srcCodec] && targetHeight == srcHeight {
			method = domain.DirectStream
		}

		renditions = append(renditions, domain.VideoRendition{
			Name:    fmt.Sprintf("%dp", targetHeight),
			Width:   targetWidth,
			Height:  targetHeight,
			Bitrate: bitrate,
			Method:  method,
		})
	}

	return renditions
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
	var renditions []domain.AudioRendition

	renditions = append(renditions, domain.AudioRendition{
		Name:     "aac_stereo",
		Codec:    "aac",
		Bitrate:  128000,
		Channels: 2,
		Method:   domain.Transcode,
	})

	if audio.Channels >= 6 {
		renditions = append(renditions, domain.AudioRendition{
			Name:     "aac_surround",
			Codec:    "aac",
			Bitrate:  384000,
			Channels: 6,
			Method:   domain.Transcode,
		})
	}

	if passthroughCodecs[audio.Codec] {
		renditions = append(renditions, domain.AudioRendition{
			Name:     audio.Codec + "_passthrough",
			Codec:    audio.Codec,
			Bitrate:  audio.Bitrate,
			Channels: audio.Channels,
			Method:   domain.DirectStream,
		})
	}

	return renditions
}
