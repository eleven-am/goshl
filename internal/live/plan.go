package live

import (
	"fmt"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/ffmpeg"
	"github.com/eleven-am/goshl/internal/rendition"
)

var copyPixelFormats = map[string]bool{"yuv420p": true, "yuvj420p": true}

var transcodedVideo = map[string]bool{"hevc": true, "mpeg2video": true}

var decodableAudio = map[string]bool{"aac": true, "aac_latm": true, "mp2": true, "mp3": true, "ac3": true, "eac3": true}

func decide(info *ProbeInfo, opts Options) (ffmpeg.LiveParams, error) {
	video := info.Video
	params := ffmpeg.LiveParams{VideoPID: video.PID, Interlaced: video.Interlaced, SegmentSeconds: opts.SegmentDuration.Seconds()}
	switch {
	case video.Codec == "h264" && copyPixelFormats[video.PixelFormat] && (!video.Interlaced || !opts.TranscodeInterlaced):
		params.CopyVideo = true
		info.VideoAction = ActionCopy
	case video.Codec == "h264" || transcodedVideo[video.Codec]:
		info.VideoAction = ActionTranscode
		stream := domain.VideoStream{
			Codec:          video.Codec,
			Width:          video.Width,
			Height:         video.Height,
			FrameRate:      video.FrameRate,
			ColorTransfer:  video.Transfer,
			ColorPrimaries: video.Primaries,
			ColorSpace:     video.Matrix,
		}
		planned, err := rendition.Live(stream, video.DisplayW, opts.MaxHeight)
		if err != nil {
			return params, fmt.Errorf("%w: %w", ErrUnsupportedCodec, err)
		}
		params.Rendition = planned
	default:
		return params, fmt.Errorf("%w: video %s", ErrUnsupportedCodec, video.Codec)
	}
	info.AudioAction = ActionNone
	info.SelectedAudio = selectAudio(info.Audios, opts.PreferredLanguage)
	if info.SelectedAudio >= 0 {
		audio := info.Audios[info.SelectedAudio]
		params.HasAudio = true
		params.AudioPID = audio.PID
		params.CopyAudio = audio.Codec == "aac"
		info.AudioAction = ActionTranscode
		if params.CopyAudio {
			info.AudioAction = ActionCopy
		}
	}
	return params, nil
}

func selectAudio(audios []AudioInfo, preferred string) int {
	first := -1
	for index, audio := range audios {
		if !decodableAudio[audio.Codec] {
			continue
		}
		if first < 0 {
			first = index
		}
		if preferred != "" && strings.EqualFold(audio.Language, preferred) {
			return index
		}
	}
	return first
}
