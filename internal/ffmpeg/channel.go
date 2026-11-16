package ffmpeg

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/source"
)

const (
	channelSegmentTimeDelta = "0.010000"
	channelPreset           = "superfast"
)

var strippedMetadataArgs = []string{"-map_metadata", "-1", "-map_chapters", "-1"}

var ErrChannelIncomplete = errors.New("channel job lacks its format facts")

type ChannelParams struct {
	InputURL         string
	VideoStreamIndex int
	Video            domain.VideoRendition
	Audio            domain.AudioRendition
	Spec             domain.ChannelSpec
	Segments         []domain.Segment
	Overlap          *domain.Segment
	Trailing         *domain.Segment
	OutputDir        string
}

func (b *CommandBuilder) Channel(p ChannelParams) ([]string, error) {
	first, last, err := jobBounds(p.Segments, p.Overlap)
	if err != nil {
		return nil, err
	}
	if p.Trailing != nil && !domain.Adjacent(last, *p.Trailing) {
		return nil, ErrTrailingNotAdjacent
	}
	spec := p.Spec
	if spec.Width <= 0 || spec.Height <= 0 || spec.PictureWidth <= 0 || spec.PictureHeight <= 0 || spec.PictureWidth > spec.Width || spec.PictureHeight > spec.Height || spec.FrameRateNum <= 0 || spec.FrameRateDen <= 0 || spec.SegmentFrames <= 0 {
		return nil, fmt.Errorf("%w: %q", ErrChannelIncomplete, p.Video.Name)
	}
	if p.Video.Bitrate <= 0 || p.Video.PeakBitrate <= 0 || p.Video.BufferSize <= 0 || p.Video.Level <= 0 || p.Audio.Bitrate <= 0 {
		return nil, fmt.Errorf("%w: %q", ErrRenditionIncomplete, p.Video.Name)
	}
	input, err := source.InputArgs(p.InputURL)
	if err != nil {
		return nil, err
	}
	chain, err := b.channelChain(p.Video)
	if err != nil {
		return nil, err
	}

	start := first.Start
	end := leadSeconds(last.End)
	cuts := segmentCuts(p.Segments, p.Overlap)
	if p.Trailing != nil {
		cuts = append(cuts, p.Trailing.Start)
		end = math.Min(p.Trailing.End, p.Trailing.Start+trailingGuardSeconds)
	}
	keyframes := []string{keyframeTime(start)}
	times := make([]string, 0, len(cuts))
	for _, cut := range cuts {
		keyframes = append(keyframes, keyframeTime(cut))
		times = append(times, formatMicros(micros(cut)-micros(start)))
	}
	if len(times) == 0 {
		times = append(times, formatMicros(micros(end)-micros(start)+uncutMarginMicros))
	}

	audioFrames := int64(math.Floor(start * aacSampleRate / aacFrameSamples))
	audioGrid := audioFrames * aacFrameSamples
	silent := spec.Silent || p.Audio.SourceEnd > 0 && start >= p.Audio.SourceEnd-audioEndGuardSeconds

	videoSeek := leadSeconds(start)
	filters := make([]string, 0, 8)
	if spec.Phase != 0 {
		filters = append(filters, fmt.Sprintf("setpts=PTS%+.6f/TB", -spec.Phase))
	}
	if num, den := sourceGrid(p.Video.FrameRate); num > 0 {
		filters = append(filters, "settb=AVTB", fmt.Sprintf("setpts=round(PTS*TB*%d/%d)*%d/(%d*TB)", num, den, den, num))
	}
	rate := fmt.Sprintf("fps=%d/%d", spec.FrameRateNum, spec.FrameRateDen)
	pastVideoEnd := p.Video.SourceEnd > 0 && start > p.Video.SourceEnd-videoAnchorLeadSeconds
	if pastVideoEnd {
		videoSeek = math.Max(p.Video.SourceEnd-videoAnchorLeadSeconds, 0)
	} else {
		rate += ":start_time=" + formatSeconds(start)
	}
	filters = append(filters, rate)
	videoEndsInside := p.Video.SourceEnd > 0 && end > p.Video.SourceEnd-videoTailMarginSeconds
	if p.Trailing == nil || videoEndsInside {
		gap := 0.0
		if p.Video.SourceEnd > 0 {
			gap = math.Max(end-p.Video.SourceEnd, 0)
		}
		filters = append(filters, "tpad=stop_mode=clone:stop_duration="+formatSeconds(gap+spec.SegmentSeconds()+videoTailMarginSeconds))
	}
	if pastVideoEnd {
		filters = append(filters, "trim=start="+formatSeconds(leadSeconds(start)))
	}
	picture := p.Video
	picture.Width, picture.Height = spec.PictureWidth, spec.PictureHeight
	filters = append(filters, videoFilter(hwaccel.NewConfig(domain.AccelNone), chain, picture))
	if spec.PictureWidth != spec.Width || spec.PictureHeight != spec.Height {
		filters = append(filters, fmt.Sprintf("pad=%d:%d:%d:%d:color=black", spec.Width, spec.Height, (spec.Width-spec.PictureWidth)/2, (spec.Height-spec.PictureHeight)/2))
	}
	filters = append(filters, "sidedata=mode=delete")

	args := []string{"-nostats", "-hide_banner", "-loglevel", "warning"}
	args = append(args, seekInputArgs(input, videoSeek)...)
	audioMap := fmt.Sprintf("1:%d", p.Audio.StreamIndex)
	audioFilter := fmt.Sprintf("aresample=%d,aresample=%d:first_pts=%d,apad", aacSampleRate, aacSampleRate, audioGrid)
	if silent {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=stereo", aacSampleRate))
		audioMap = "1:0"
		audioFilter = fmt.Sprintf("asetpts=N%+d", audioGrid)
	} else {
		args = append(args, seekInputArgs(input, float64(audioGrid)/aacSampleRate)...)
	}
	args = append(args, "-to", formatSeconds(end))
	args = append(args, sourceTimelineArgs...)
	args = append(args,
		"-muxdelay", "0",
		"-output_ts_offset", formatSeconds(domain.OutputTimelineOffset.Seconds()),
		"-map", fmt.Sprintf("0:%d", p.VideoStreamIndex),
		"-map", audioMap,
		"-sn", "-dn",
	)
	args = append(args, strippedMetadataArgs...)
	args = append(args,
		"-metadata", "service_provider=goshl",
		"-metadata", "service_name=channel",
		"-c:v", "libx264",
		"-preset", channelPreset,
		"-vf", strings.Join(filters, ","),
		"-profile:v", "high",
		"-level:v", fmt.Sprintf("%d.%d", p.Video.Level/10, p.Video.Level%10),
		"-pix_fmt", "yuv420p",
		"-b:v", strconv.Itoa(p.Video.Bitrate),
		"-maxrate", strconv.Itoa(p.Video.PeakBitrate),
		"-bufsize", strconv.Itoa(p.Video.BufferSize),
		"-g", strconv.Itoa(spec.SegmentFrames),
		"-bf", "0",
		"-sc_threshold", "0",
		"-a53cc", "0",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-colorspace", "bt709",
		"-color_range", "tv",
		"-fps_mode", "passthrough",
		"-force_key_frames", strings.Join(keyframes, ","),
		"-c:a", "aac",
		"-ac", strconv.Itoa(domain.ChannelAudioChannels),
		"-ar", strconv.Itoa(aacSampleRate),
		"-b:a", strconv.Itoa(p.Audio.Bitrate),
		"-af", audioFilter,
		"-fflags", "+bitexact",
		"-flags:v", "+bitexact",
		"-flags:a", "+bitexact",
		"-f", "segment",
		"-segment_time_delta", channelSegmentTimeDelta,
		"-segment_format", "mpegts",
		"-segment_format_options", "mpegts_flags=+initial_discontinuity",
		"-segment_list_type", "csv",
		"-segment_list", "pipe:1",
		"-segment_start_number", strconv.Itoa(first.Index),
		"-segment_times", strings.Join(times, ","),
		filepath.Join(p.OutputDir, "segment-%05d.ts"),
	)
	return args, nil
}

func (b *CommandBuilder) channelChain(video domain.VideoRendition) (string, error) {
	if !video.Colour.Tonemap {
		return hwaccel.NewConfig(domain.AccelNone).ScaleFilter, nil
	}
	return b.tonemap()
}

func sourceGrid(rate float64) (int, int) {
	if rate <= 0 {
		return 0, 0
	}
	if whole := math.Round(rate); math.Abs(rate-whole) < 0.001 {
		return int(whole), 1
	}
	if scaled := math.Round(rate * 1.001); math.Abs(rate*1.001-scaled) < 0.002 {
		return int(scaled) * 1000, 1001
	}
	return 0, 0
}
