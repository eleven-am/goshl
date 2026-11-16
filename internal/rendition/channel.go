package rendition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/h264level"
	"github.com/eleven-am/goshl/internal/playlist"
)

const channelPhaseThreshold = 0.0005

var (
	ErrChannelNoVideo    = errors.New("source has no video stream")
	ErrChannelAudioTrack = errors.New("audio track does not exist")
)

func IsChannelName(name string) bool {
	return strings.HasPrefix(name, domain.ChannelRenditionPrefix)
}

func ChannelName(profile domain.ChannelProfile, audioTrack, segmentsPerJob int) string {
	identity := struct {
		FormatVersion  int
		Profile        domain.ChannelProfile
		AudioTrack     int
		SegmentsPerJob int
	}{domain.ChannelFormatVersion, profile, audioTrack, segmentsPerJob}
	encoded, _ := json.Marshal(identity)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%s%dp-a%d-%s", domain.ChannelRenditionPrefix, profile.Height, audioTrack, hex.EncodeToString(sum[:])[:12])
}

func Channel(meta *domain.Metadata, profile domain.ChannelProfile, audioTrack, segmentsPerJob int) (domain.VideoRendition, domain.AudioRendition, domain.ChannelSpec, error) {
	source := meta.Video
	if source.Width <= 0 || source.Height <= 0 {
		return domain.VideoRendition{}, domain.AudioRendition{}, domain.ChannelSpec{}, ErrChannelNoVideo
	}
	if audioTrack < 0 || audioTrack > 0 && audioTrack >= len(meta.Audios) {
		return domain.VideoRendition{}, domain.AudioRendition{}, domain.ChannelSpec{}, fmt.Errorf("%w: %d", ErrChannelAudioTrack, audioTrack)
	}
	name := ChannelName(profile, audioTrack, segmentsPerJob)
	width, height := profile.Width(), profile.Height
	level, ok := h264level.Minimum(width, height, profile.FrameRate(), profile.VideoBitrate)
	if !ok {
		return domain.VideoRendition{}, domain.AudioRendition{}, domain.ChannelSpec{}, fmt.Errorf("%w: no H.264 level fits %dx%d at %d b/s", domain.ErrInvalidChannelProfile, width, height, profile.VideoBitrate)
	}
	video := domain.VideoRendition{
		Name:        name,
		Width:       width,
		Height:      height,
		Bitrate:     profile.VideoBitrate,
		PeakBitrate: profile.VideoBitrate,
		BufferSize:  min(2*profile.VideoBitrate, h264level.MaxCPB(level)),
		FrameRate:   profile.FrameRate(),
		Codec:       fmt.Sprintf("avc1.6400%02x", level),
		Level:       level,
		Method:      domain.Transcode,
		Colour:      domain.PlanColour(source),
		SourceEnd:   math.Max(source.End, 0),
	}
	audio := domain.AudioRendition{
		Name:        name,
		Codec:       "aac",
		CodecString: audioCodecString("aac"),
		Bitrate:     profile.AudioBitrate,
		Channels:    domain.ChannelAudioChannels,
		SampleRate:  domain.ChannelAudioSampleRate,
		Method:      domain.Transcode,
	}
	spec := domain.ChannelSpec{
		FormatVersion:  domain.ChannelFormatVersion,
		Width:          width,
		Height:         height,
		FrameRateNum:   profile.FrameRateNum,
		FrameRateDen:   profile.FrameRateDen,
		SegmentFrames:  profile.SegmentFrames,
		SegmentsPerJob: segmentsPerJob,
		TotalFrames:    playlist.ChannelFrames(meta.Duration, profile),
		AudioTrack:     audioTrack,
		Silent:         len(meta.Audios) == 0,
		Phase:          channelPhase(source.Start, profile),
	}
	spec.PictureWidth, spec.PictureHeight = channelPicture(source, width, height)
	if !spec.Silent {
		track := meta.Audios[audioTrack]
		audio.StreamIndex = track.Index
		audio.SourceEnd = track.End
		audio.Language = track.Language
	}
	return video, audio, spec, nil
}

func channelPhase(start float64, profile domain.ChannelProfile) float64 {
	rate := profile.FrameRate()
	phase := start - math.Round(start*rate)/rate
	if math.Abs(phase) < channelPhaseThreshold {
		return 0
	}
	return math.Round(phase*1e6) / 1e6
}

func channelPicture(source domain.VideoStream, width, height int) (int, int) {
	displayWidth, displayHeight := float64(source.Width), float64(source.Height)
	if num, den, ok := sampleAspect(source.SampleAspect); ok {
		displayWidth *= num / den
	}
	if rotation := ((source.Rotation % 360) + 360) % 360; rotation == 90 || rotation == 270 {
		displayWidth, displayHeight = displayHeight, displayWidth
	}
	if displayWidth*float64(height) >= displayHeight*float64(width) {
		return width, fitEven(float64(width)*displayHeight/displayWidth, height)
	}
	return fitEven(float64(height)*displayWidth/displayHeight, width), height
}

func fitEven(value float64, limit int) int {
	fitted := int(math.Round(value/2)) * 2
	return min(max(fitted, 2), limit)
}

func sampleAspect(value string) (float64, float64, bool) {
	left, right, found := strings.Cut(value, ":")
	if !found {
		return 0, 0, false
	}
	num, numErr := strconv.ParseFloat(left, 64)
	den, denErr := strconv.ParseFloat(right, 64)
	if numErr != nil || denErr != nil || num <= 0 || den <= 0 {
		return 0, 0, false
	}
	return num, den, true
}
