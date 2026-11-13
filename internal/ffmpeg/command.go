package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/h264level"
	"github.com/eleven-am/goshl/internal/mediaexec"
	"github.com/eleven-am/goshl/internal/source"
)

const (
	videoSegmentTimeDelta = "0.050000"
	audioSegmentTimeDelta = "0.000001"
	keyframeLeadMicros    = 1000
	trailingGuardSeconds  = 1.0
	uncutMarginMicros     = 3600 * 1000000
	aacSampleRate         = 48000
	aacFrameSamples       = 1024
)

var (
	ErrNoSegments           = errors.New("job has no segments")
	ErrLandingRequired      = errors.New("stream copy requires a measured seek landing")
	ErrTrailingNotSupported = errors.New("trailing segment is only supported for copied video")
	ErrTrailingNotAdjacent  = errors.New("trailing segment does not follow the last segment")
	ErrOverlapNotAdjacent   = errors.New("overlap segment does not precede the first segment")
)

var sourceTimelineArgs = []string{"-copyts", "-start_at_zero"}

var sourceFrameTimingArgs = []string{"-fps_mode", "passthrough", "-enc_time_base", "demux"}

type CommandBuilder struct {
	HWAccel *domain.HWAccelConfig
}

func NewCommandBuilder(hwAccel *domain.HWAccelConfig) *CommandBuilder {
	return &CommandBuilder{HWAccel: hwAccel}
}

type VideoParams struct {
	InputURL    string
	StreamIndex int
	Rendition   domain.VideoRendition
	Segments    []domain.Segment
	Overlap     *domain.Segment
	Trailing    *domain.Segment
	Landing     *float64
	OutputDir   string
}

type AudioParams struct {
	InputURL    string
	StreamIndex int
	Rendition   domain.AudioRendition
	Segments    []domain.Segment
	Overlap     *domain.Segment
	Trailing    *domain.Segment
	Landing     *float64
	OutputDir   string
}

type jobTimeline struct {
	inputURL    string
	inputArgs   []string
	streamIndex int
	inputFlags  []string
	encodeArgs  []string
	outputDir   string
	firstIndex  int
	seek        float64
	end         float64
	reference   float64
	cuts        []float64
	timeDelta   string
}

func (b *CommandBuilder) Video(p VideoParams) ([]string, error) {
	first, last, err := jobBounds(p.Segments, p.Overlap)
	if err != nil {
		return nil, err
	}
	copied := p.Rendition.Method == domain.DirectStream
	if p.Trailing != nil {
		if !copied {
			return nil, ErrTrailingNotSupported
		}
		if !domain.Adjacent(last, *p.Trailing) {
			return nil, ErrTrailingNotAdjacent
		}
	}

	input, err := source.InputArgs(p.InputURL)
	if err != nil {
		return nil, err
	}

	timeline := jobTimeline{
		inputURL:    p.InputURL,
		inputArgs:   input,
		streamIndex: p.StreamIndex,
		outputDir:   p.OutputDir,
		firstIndex:  first.Index,
		seek:        first.Start,
		end:         last.End,
		reference:   first.Start,
		cuts:        segmentCuts(p.Segments, p.Overlap),
		timeDelta:   videoSegmentTimeDelta,
	}

	if copied {
		if p.Landing == nil {
			return nil, ErrLandingRequired
		}
		timeline.reference = *p.Landing
		timeline.encodeArgs = []string{"-c:v", "copy", "-bsf:v", "h264_mp4toannexb,dump_extra"}
		if p.Trailing != nil {
			timeline.cuts = append(timeline.cuts, p.Trailing.Start)
			timeline.end = math.Min(p.Trailing.End, p.Trailing.Start+trailingGuardSeconds)
		}
		return timeline.args(), nil
	}

	timeline.seek = leadSeconds(first.Start)
	timeline.end = leadSeconds(last.End)
	timeline.inputFlags = b.HWAccel.DecodeFlags
	timeline.encodeArgs = b.transcodeVideoArgs(p.Rendition)
	timeline.encodeArgs = append(timeline.encodeArgs, sourceFrameTimingArgs...)
	timeline.encodeArgs = append(timeline.encodeArgs, b.HWAccel.KeyframeFlag, keyframeTimes(p.Segments))
	if b.HWAccel.Accelerator == domain.AccelCUDA {
		timeline.encodeArgs = append(timeline.encodeArgs, "-forced-idr", "1")
	}
	return timeline.args(), nil
}

func (b *CommandBuilder) Audio(p AudioParams) ([]string, error) {
	first, last, err := jobBounds(p.Segments, p.Overlap)
	if err != nil {
		return nil, err
	}
	if p.Trailing != nil {
		return nil, ErrTrailingNotSupported
	}

	input, err := source.InputArgs(p.InputURL)
	if err != nil {
		return nil, err
	}

	timeline := jobTimeline{
		inputURL:    p.InputURL,
		inputArgs:   input,
		streamIndex: p.StreamIndex,
		outputDir:   p.OutputDir,
		firstIndex:  first.Index,
		seek:        first.Start,
		end:         last.End,
		cuts:        segmentCuts(p.Segments, p.Overlap),
		timeDelta:   audioSegmentTimeDelta,
	}

	if p.Rendition.Method == domain.DirectStream {
		if p.Landing == nil {
			return nil, ErrLandingRequired
		}
		timeline.reference = *p.Landing
		timeline.encodeArgs = []string{"-c:a", "copy"}
		return timeline.args(), nil
	}

	frames := int64(math.Floor(first.Start * aacSampleRate / aacFrameSamples))
	timeline.seek = float64(frames*aacFrameSamples) / aacSampleRate
	timeline.reference = float64((frames-1)*aacFrameSamples) / aacSampleRate
	timeline.encodeArgs = []string{
		"-c:a", "aac",
		"-ac", strconv.Itoa(p.Rendition.Channels),
		"-b:a", strconv.Itoa(p.Rendition.Bitrate),
		"-ar", strconv.Itoa(aacSampleRate),
		"-af", fmt.Sprintf("aresample=%d:first_pts=%d", aacSampleRate, frames*aacFrameSamples),
	}
	return timeline.args(), nil
}

func (t jobTimeline) args() []string {
	args := []string{"-nostats", "-hide_banner", "-loglevel", "warning"}
	args = append(args, t.inputFlags...)
	args = append(args, seekInputArgs(t.inputArgs, t.seek)...)
	args = append(args, "-to", formatSeconds(t.end))
	args = append(args, sourceTimelineArgs...)
	args = append(args,
		"-muxdelay", "0",
		"-output_ts_offset", formatSeconds(domain.OutputTimelineOffset.Seconds()),
		"-map", fmt.Sprintf("0:%d", t.streamIndex),
	)
	args = append(args, t.encodeArgs...)
	args = append(args,
		"-f", "segment",
		"-segment_time_delta", t.timeDelta,
		"-segment_format", "mpegts",
		"-segment_list_type", "flat",
		"-segment_list", "pipe:1",
		"-segment_start_number", strconv.Itoa(t.firstIndex),
		"-segment_times", t.segmentTimes(),
		filepath.Join(t.outputDir, "segment-%05d.ts"),
	)
	return args
}

func (t jobTimeline) segmentTimes() string {
	reference := micros(t.reference)
	if len(t.cuts) == 0 {
		return formatMicros(micros(t.end) - reference + uncutMarginMicros)
	}
	times := make([]string, 0, len(t.cuts))
	for _, cut := range t.cuts {
		times = append(times, formatMicros(micros(cut)-reference))
	}
	return strings.Join(times, ",")
}

func jobBounds(segments []domain.Segment, overlap *domain.Segment) (domain.Segment, domain.Segment, error) {
	if len(segments) == 0 {
		return domain.Segment{}, domain.Segment{}, ErrNoSegments
	}
	first := segments[0]
	last := segments[len(segments)-1]
	if overlap == nil {
		return first, last, nil
	}
	if !domain.Adjacent(*overlap, first) {
		return domain.Segment{}, domain.Segment{}, ErrOverlapNotAdjacent
	}
	return *overlap, last, nil
}

func segmentCuts(segments []domain.Segment, overlap *domain.Segment) []float64 {
	cuts := make([]float64, 0, len(segments)+1)
	if overlap != nil {
		cuts = append(cuts, segments[0].Start)
	}
	for _, segment := range segments[1:] {
		cuts = append(cuts, segment.Start)
	}
	return cuts
}

func keyframeTimes(segments []domain.Segment) string {
	times := make([]string, 0, len(segments))
	for _, segment := range segments {
		times = append(times, keyframeTime(segment.Start))
	}
	return strings.Join(times, ",")
}

func keyframeTime(start float64) string {
	return formatSeconds(leadSeconds(start))
}

func leadSeconds(boundary float64) float64 {
	lead := micros(boundary) - keyframeLeadMicros
	if lead < 0 {
		lead = 0
	}
	return float64(lead) / 1e6
}

func micros(seconds float64) int64 {
	return int64(math.Round(seconds * 1e6))
}

func formatMicros(value int64) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	return fmt.Sprintf("%s%d.%06d", sign, value/1000000, value%1000000)
}

func formatSeconds(seconds float64) string {
	return formatMicros(micros(seconds))
}

func seekInputArgs(input []string, start float64) []string {
	return append([]string{"-ss", formatSeconds(start)}, input...)
}

func seekLandingArgs(inputURL string, start float64, streamIndex int) ([]string, error) {
	input, err := source.InputArgs(inputURL)
	if err != nil {
		return nil, err
	}
	args := []string{"-nostats", "-hide_banner", "-loglevel", "error"}
	args = append(args, seekInputArgs(input, start)...)
	args = append(args,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-c", "copy",
		"-frames", "1",
	)
	args = append(args, sourceTimelineArgs...)
	return append(args, "-f", "framecrc", "-"), nil
}

func (b *CommandBuilder) SeekLanding(ctx context.Context, inputURL string, start float64, streamIndex int) (float64, error) {
	args, err := seekLandingArgs(inputURL, start, streamIndex)
	if err != nil {
		return 0, fmt.Errorf("seek landing: %w", err)
	}
	out, err := mediaexec.Output(ctx, "ffmpeg", args...)
	if err != nil {
		return 0, fmt.Errorf("seek landing: %w", err)
	}

	landing, err := parseFramecrcFirstPTS(string(out))
	if err != nil {
		return 0, fmt.Errorf("seek landing: %w", err)
	}
	return landing, nil
}

func parseFramecrcFirstPTS(output string) (float64, error) {
	var timebase float64
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#tb 0:") {
			num, den, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#tb 0:")), "/")
			if !ok {
				return 0, fmt.Errorf("malformed timebase %q", line)
			}
			n, nErr := strconv.ParseFloat(num, 64)
			d, dErr := strconv.ParseFloat(den, 64)
			if nErr != nil || dErr != nil || d == 0 {
				return 0, fmt.Errorf("malformed timebase %q", line)
			}
			timebase = n / d
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 || timebase == 0 {
			return 0, fmt.Errorf("malformed packet line %q", line)
		}
		pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed packet pts %q: %w", line, err)
		}
		return float64(pts) * timebase, nil
	}
	return 0, fmt.Errorf("no packet at seek position")
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
