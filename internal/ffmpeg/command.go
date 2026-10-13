package ffmpeg

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/h264level"
)

type StreamParams struct {
	InputURL    string
	StreamIndex int
	StartTime   float64
	EndTime     float64
}

type VideoStreamParams struct {
	StreamParams
	Rendition     domain.VideoRendition
	KeyframeTimes []float64
}

type AudioStreamParams struct {
	StreamParams
	Rendition domain.AudioRendition
}

type CommandBuilder struct {
	HWAccel *domain.HWAccelConfig
}

func NewCommandBuilder(hwAccel *domain.HWAccelConfig) *CommandBuilder {
	return &CommandBuilder{HWAccel: hwAccel}
}

type VideoParams struct {
	InputURL           string
	StreamIndex        int
	Rendition          domain.VideoRendition
	Segments           []domain.Segment
	OutputDir          string
	ActualSeekKeyframe float64
}

type AudioParams struct {
	InputURL    string
	StreamIndex int
	Rendition   domain.AudioRendition
	Segments    []domain.Segment
	OutputDir   string
}

func (b *CommandBuilder) Video(p VideoParams) []string {
	if len(p.Segments) == 0 {
		return nil
	}

	startSeg := p.Segments[0]
	endSeg := p.Segments[len(p.Segments)-1]

	args := []string{
		"-nostats", "-hide_banner", "-loglevel", "warning",
	}

	if p.Rendition.Method != domain.DirectStream {
		args = append(args, b.HWAccel.DecodeFlags...)
	}

	args = append(args,
		"-ss", fmt.Sprintf("%.6f", startSeg.Start),
		"-i", p.InputURL,
		"-to", fmt.Sprintf("%.6f", endSeg.End),
		"-copyts",
		"-start_at_zero",
		"-muxdelay", "0",
	)

	args = append(args, "-map", fmt.Sprintf("0:%d", p.StreamIndex))

	args = append(args, b.videoEncodeArgs(p)...)

	var segmentTimes string
	if p.Rendition.Method == domain.DirectStream && p.ActualSeekKeyframe > 0 {
		segmentTimes = formatSegmentTimesWithOffset(p.Segments, p.ActualSeekKeyframe)
	} else {
		segmentTimes = formatSegmentTimes(p.Segments)
	}
	outputPattern := filepath.Join(p.OutputDir, "segment-%05d.ts")

	args = append(args,
		"-f", "segment",
		"-segment_time_delta", "0.05",
		"-segment_format", "mpegts",
		"-segment_list_type", "flat",
		"-segment_list", "pipe:1",
		"-segment_start_number", fmt.Sprintf("%d", startSeg.Index),
	)

	if segmentTimes != "" {
		args = append(args, "-segment_times", segmentTimes)
	}

	args = append(args, outputPattern)

	return args
}

func (b *CommandBuilder) videoEncodeArgs(p VideoParams) []string {
	if p.Rendition.Method == domain.DirectStream {
		return []string{"-c:v", "copy", "-bsf:v", "h264_mp4toannexb,dump_extra"}
	}

	segmentTimes := formatKeyframeTimes(p.Segments)
	args := b.transcodeVideoArgs(p.Rendition)
	args = append(args, b.HWAccel.KeyframeFlag, segmentTimes)

	if b.HWAccel.Accelerator == domain.AccelCUDA {
		args = append(args, "-forced-idr", "1")
	}

	return args
}

func (b *CommandBuilder) Audio(p AudioParams) []string {
	if len(p.Segments) == 0 {
		return nil
	}

	startSeg := p.Segments[0]
	endSeg := p.Segments[len(p.Segments)-1]

	args := []string{
		"-nostats", "-hide_banner", "-loglevel", "warning",
		"-ss", fmt.Sprintf("%.6f", startSeg.Start),
		"-i", p.InputURL,
		"-to", fmt.Sprintf("%.6f", endSeg.End),
		"-copyts",
		"-start_at_zero",
		"-muxdelay", "0",
	}

	args = append(args, "-map", fmt.Sprintf("0:%d", p.StreamIndex))

	args = append(args, b.audioEncodeArgs(p)...)

	segmentTimes := formatSegmentTimes(p.Segments)
	outputPattern := filepath.Join(p.OutputDir, "segment-%05d.ts")

	args = append(args,
		"-f", "segment",
		"-segment_time_delta", "0.05",
		"-segment_format", "mpegts",
		"-segment_list_type", "flat",
		"-segment_list", "pipe:1",
		"-segment_start_number", fmt.Sprintf("%d", startSeg.Index),
	)

	if segmentTimes != "" {
		args = append(args, "-segment_times", segmentTimes)
	}

	args = append(args, outputPattern)

	return args
}

func (b *CommandBuilder) audioEncodeArgs(p AudioParams) []string {
	if p.Rendition.Method == domain.DirectStream {
		return []string{"-c:a", "copy"}
	}

	return []string{
		"-c:a", "aac",
		"-ac", fmt.Sprintf("%d", p.Rendition.Channels),
		"-b:a", fmt.Sprintf("%d", p.Rendition.Bitrate),
	}
}

func formatSegmentTimes(segments []domain.Segment) string {
	if len(segments) <= 1 {
		return ""
	}

	seekOffset := segments[0].Start
	times := make([]string, 0, len(segments)-1)
	for i := 1; i < len(segments); i++ {
		times = append(times, fmt.Sprintf("%.6f", segments[i].Start-seekOffset))
	}
	return strings.Join(times, ",")
}

func formatSegmentTimesWithOffset(segments []domain.Segment, actualKeyframe float64) string {
	if len(segments) <= 1 {
		return ""
	}

	times := make([]string, 0, len(segments)-1)
	for i := 1; i < len(segments); i++ {
		times = append(times, fmt.Sprintf("%.6f", segments[i].Start-actualKeyframe))
	}
	return strings.Join(times, ",")
}

func formatKeyframeTimes(segments []domain.Segment) string {
	if len(segments) == 0 {
		return ""
	}

	times := make([]string, 0, len(segments))
	for _, seg := range segments {
		times = append(times, fmt.Sprintf("%.6f", seg.Start))
	}
	return strings.Join(times, ",")
}

func (b *CommandBuilder) VideoStream(p VideoStreamParams) []string {
	args := []string{
		"-nostats", "-hide_banner", "-loglevel", "warning",
	}

	if p.Rendition.Method != domain.DirectStream {
		args = append(args, b.HWAccel.DecodeFlags...)
	}

	args = append(args,
		"-ss", fmt.Sprintf("%.6f", p.StartTime),
		"-i", p.InputURL,
		"-to", fmt.Sprintf("%.6f", p.EndTime),
		"-copyts",
		"-start_at_zero",
		"-muxdelay", "0",
	)

	args = append(args, "-map", fmt.Sprintf("0:%d", p.StreamIndex))

	args = append(args, b.videoStreamEncodeArgs(p)...)

	args = append(args, "-f", "mpegts", "pipe:1")

	return args
}

func (b *CommandBuilder) videoStreamEncodeArgs(p VideoStreamParams) []string {
	if p.Rendition.Method == domain.DirectStream {
		return []string{"-c:v", "copy"}
	}

	args := b.transcodeVideoArgs(p.Rendition)

	if len(p.KeyframeTimes) > 0 {
		times := make([]string, len(p.KeyframeTimes))
		for i, t := range p.KeyframeTimes {
			times[i] = fmt.Sprintf("%.6f", t)
		}
		args = append(args, b.HWAccel.KeyframeFlag, strings.Join(times, ","))
	}

	if b.HWAccel.Accelerator == domain.AccelCUDA {
		args = append(args, "-forced-idr", "1")
	}

	return args
}

func (b *CommandBuilder) transcodeVideoArgs(rendition domain.VideoRendition) []string {
	peakBitrate := rendition.PeakBitrate
	if peakBitrate <= 0 {
		peakBitrate = int(float64(rendition.Bitrate) * 1.5)
	}
	level := rendition.Level
	minimumLevel := h264level.Minimum(rendition.Width, rendition.Height, rendition.FrameRate, peakBitrate)
	if level < minimumLevel {
		level = minimumLevel
	}

	args := append([]string(nil), b.HWAccel.EncodeFlags...)
	return append(args,
		"-vf", fmt.Sprintf(b.HWAccel.ScaleFilter, rendition.Width, rendition.Height),
		"-profile:v", "high",
		"-level:v", fmt.Sprintf("%.1f", float64(level)/10),
		"-b:v", fmt.Sprintf("%d", rendition.Bitrate),
		"-maxrate", fmt.Sprintf("%d", peakBitrate),
		"-bufsize", fmt.Sprintf("%d", rendition.Bitrate*5),
	)
}

func (b *CommandBuilder) AudioStream(p AudioStreamParams) []string {
	args := []string{
		"-nostats", "-hide_banner", "-loglevel", "warning",
		"-ss", fmt.Sprintf("%.6f", p.StartTime),
		"-i", p.InputURL,
		"-to", fmt.Sprintf("%.6f", p.EndTime),
		"-copyts",
		"-start_at_zero",
		"-muxdelay", "0",
	}

	args = append(args, "-map", fmt.Sprintf("0:%d", p.StreamIndex))

	args = append(args, b.audioStreamEncodeArgs(p)...)

	args = append(args, "-f", "mpegts", "pipe:1")

	return args
}

func (b *CommandBuilder) audioStreamEncodeArgs(p AudioStreamParams) []string {
	if p.Rendition.Method == domain.DirectStream {
		return []string{"-c:a", "copy"}
	}

	return []string{
		"-c:a", "aac",
		"-ac", fmt.Sprintf("%d", p.Rendition.Channels),
		"-b:a", fmt.Sprintf("%d", p.Rendition.Bitrate),
	}
}
