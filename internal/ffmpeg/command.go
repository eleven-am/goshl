package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/mediaexec"
	"github.com/eleven-am/goshl/internal/source"
)

const (
	videoSegmentTimeDelta  = "0.050000"
	audioSegmentTimeDelta  = "0.000001"
	keyframeLeadMicros     = 1000
	trailingGuardSeconds   = 1.0
	uncutMarginMicros      = 3600 * 1000000
	aacSampleRate          = 48000
	aacFrameSamples        = 1024
	ac3FrameSamples        = 1536
	ac3EncoderDelay        = 256
	audioEndGuardSeconds   = 0.05
	videoAnchorLeadSeconds = 2.0
	videoTailMarginSeconds = 1.0
	audioAnchorLeadSeconds = 0.5
	passthroughMaxChannels = 6
)

var (
	ErrNoSegments           = errors.New("job has no segments")
	ErrLandingRequired      = errors.New("stream copy requires a measured seek landing")
	ErrTrailingNotSupported = errors.New("trailing segment is only supported for copied video")
	ErrTrailingNotAdjacent  = errors.New("trailing segment does not follow the last segment")
	ErrOverlapNotAdjacent   = errors.New("overlap segment does not precede the first segment")
	ErrRenditionIncomplete  = errors.New("transcode rendition lacks encoder facts")
)

var sourceTimelineArgs = []string{"-copyts", "-start_at_zero"}

var sourceFrameTimingArgs = []string{"-fps_mode", "passthrough", "-enc_time_base", "demux"}

const tonemapDetectionTimeout = 10 * time.Second

type CommandBuilder struct {
	HWAccel       *domain.HWAccelConfig
	DetectTonemap func(context.Context) (string, error)

	tonemapMu     sync.Mutex
	tonemapFilter string
	tonemapErr    error
	tonemapKnown  bool
}

func NewCommandBuilder(hwAccel *domain.HWAccelConfig) *CommandBuilder {
	return &CommandBuilder{HWAccel: hwAccel, DetectTonemap: hwaccel.DetectTonemap}
}

func (b *CommandBuilder) tonemap() (string, error) {
	b.tonemapMu.Lock()
	defer b.tonemapMu.Unlock()
	if b.tonemapKnown {
		return b.tonemapFilter, b.tonemapErr
	}
	detect := b.DetectTonemap
	if detect == nil {
		detect = hwaccel.DetectTonemap
	}
	ctx, cancel := context.WithTimeout(context.Background(), tonemapDetectionTimeout)
	defer cancel()
	filter, err := detect(ctx)
	if err != nil && !errors.Is(err, hwaccel.ErrTonemapUnavailable) {
		return "", err
	}
	b.tonemapFilter, b.tonemapErr, b.tonemapKnown = filter, err, true
	return filter, err
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
	Landing     *float64
	OutputDir   string
}

var (
	videoOnlyArgs = []string{"-an", "-sn", "-dn"}
	audioOnlyArgs = []string{"-vn", "-sn", "-dn"}
)

type jobTimeline struct {
	inputArgs   []string
	streamIndex int
	inputFlags  []string
	only        []string
	encodeArgs  []string
	outputDir   string
	firstIndex  int
	generated   bool
	sampleRate  int
	seek        float64
	end         float64
	shift       float64
	reference   float64
	cuts        []float64
	timeDelta   string
}

type audioCoverage int

const (
	audioCovered audioCoverage = iota
	audioPadded
	audioSilent
)

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
		inputArgs:   input,
		streamIndex: p.StreamIndex,
		outputDir:   p.OutputDir,
		firstIndex:  first.Index,
		seek:        first.Start,
		end:         last.End,
		reference:   first.Start,
		cuts:        segmentCuts(p.Segments, p.Overlap),
		timeDelta:   videoSegmentTimeDelta,
		only:        videoOnlyArgs,
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

	config, chain, err := b.pipelineFor(p.Rendition)
	if err != nil {
		return nil, err
	}
	timeline.seek = leadSeconds(first.Start)
	timeline.end = leadSeconds(last.End)
	if p.Landing != nil && *p.Landing > first.Start {
		timeline.reference = *p.Landing
	}
	tail := videoTailFilter(p.Rendition, first, last)
	if tail != "" && first.Start > p.Rendition.SourceEnd-videoAnchorLeadSeconds {
		timeline.seek = math.Max(p.Rendition.SourceEnd-videoAnchorLeadSeconds, 0)
		tail += ",trim=start=" + formatSeconds(leadSeconds(first.Start))
	}
	timeline.inputFlags = config.DecodeFlags
	timeline.encodeArgs, err = transcodeVideoArgs(config, tail, chain, p.Rendition)
	if err != nil {
		return nil, err
	}
	timeline.encodeArgs = append(timeline.encodeArgs, sourceFrameTimingArgs...)
	timeline.encodeArgs = append(timeline.encodeArgs, config.KeyframeFlag, keyframeTimes(p.Segments))
	if config.Accelerator == domain.AccelCUDA {
		timeline.encodeArgs = append(timeline.encodeArgs, "-forced-idr", "1")
	}
	return timeline.args(), nil
}

func (b *CommandBuilder) Audio(p AudioParams) ([]string, error) {
	first, last, err := jobBounds(p.Segments, p.Overlap)
	if err != nil {
		return nil, err
	}
	input, err := source.InputArgs(p.InputURL)
	if err != nil {
		return nil, err
	}

	timeline := jobTimeline{
		inputArgs:   input,
		streamIndex: p.StreamIndex,
		outputDir:   p.OutputDir,
		firstIndex:  first.Index,
		seek:        first.Start,
		end:         last.End,
		cuts:        segmentCuts(p.Segments, p.Overlap),
		timeDelta:   audioSegmentTimeDelta,
		only:        audioOnlyArgs,
	}

	coverage := audioCoverageOf(p.Rendition, first, last)
	if p.Rendition.Method == domain.DirectStream {
		if p.Landing == nil {
			return nil, ErrLandingRequired
		}
		timeline.reference = *p.Landing
		if coverage == audioCovered {
			timeline.encodeArgs = []string{"-c:a", "copy"}
			return timeline.args(), nil
		}
		timeline.sampleRate = sourceSampleRate(p.Rendition)
		timeline.shift = float64(ac3EncoderDelay) / float64(timeline.sampleRate)
		timeline.encodeArgs = []string{
			"-c:a", p.Rendition.Codec,
			"-ac", strconv.Itoa(min(p.Rendition.Channels, passthroughMaxChannels)),
			"-b:a", strconv.Itoa(p.Rendition.Bitrate),
		}
		if coverage == audioPadded {
			timeline.inputFlags = []string{"-noaccurate_seek", "-flags2", "+skip_manual"}
			timeline.end = last.End - math.Min(*p.Landing, 0)
			timeline.encodeArgs = append(timeline.encodeArgs, "-af", "apad")
			return timeline.args(), nil
		}
		frame := float64(ac3FrameSamples) / float64(timeline.sampleRate)
		timeline.generated = true
		timeline.seek = *p.Landing + math.Floor((first.Start-*p.Landing)/frame+domain.BoundaryTolerance)*frame
		timeline.reference = timeline.seek
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
	}
	if coverage == audioSilent {
		timeline.generated = true
		return timeline.args(), nil
	}
	timeline.encodeArgs = append(timeline.encodeArgs, "-af", fmt.Sprintf("aresample=%d,aresample=%d:first_pts=%d,apad", aacSampleRate, aacSampleRate, frames*aacFrameSamples))
	return timeline.args(), nil
}

func sourceSampleRate(rendition domain.AudioRendition) int {
	if rendition.SampleRate > 0 {
		return rendition.SampleRate
	}
	return aacSampleRate
}

func (t jobTimeline) outputRate() int {
	if t.sampleRate > 0 {
		return t.sampleRate
	}
	return aacSampleRate
}

func (p AudioParams) LandingStart() float64 {
	first, last, err := jobBounds(p.Segments, p.Overlap)
	if err != nil {
		return 0
	}
	if audioCoverageOf(p.Rendition, first, last) != audioSilent {
		return first.Start
	}
	return math.Max(p.Rendition.SourceEnd-audioAnchorLeadSeconds, 0)
}

func audioCoverageOf(rendition domain.AudioRendition, first, last domain.Segment) audioCoverage {
	if rendition.SourceEnd <= 0 {
		return audioCovered
	}
	limit := rendition.SourceEnd - audioEndGuardSeconds
	switch {
	case first.Start >= limit:
		return audioSilent
	case last.End > limit:
		return audioPadded
	default:
		return audioCovered
	}
}

func (t jobTimeline) args() []string {
	args := []string{"-nostats", "-hide_banner", "-loglevel", "warning"}
	args = append(args, t.inputFlags...)
	offset := domain.OutputTimelineOffset.Seconds() + t.shift
	if t.generated {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=stereo", t.outputRate()))
		args = append(args, "-to", formatSeconds(t.end-t.seek))
		args = append(args,
			"-muxdelay", "0",
			"-output_ts_offset", formatSeconds(offset+t.seek),
			"-map", "0:0",
		)
	} else {
		args = append(args, seekInputArgs(t.inputArgs, t.seek)...)
		args = append(args, "-to", formatSeconds(t.end))
		args = append(args, sourceTimelineArgs...)
		args = append(args,
			"-muxdelay", "0",
			"-output_ts_offset", formatSeconds(offset),
			"-map", fmt.Sprintf("0:%d", t.streamIndex),
		)
	}
	args = append(args, t.only...)
	args = append(args, t.encodeArgs...)
	args = append(args,
		"-f", "segment",
		"-segment_time_delta", t.timeDelta,
		"-segment_format", "mpegts",
		"-segment_format_options", "mpegts_flags=+initial_discontinuity",
		"-segment_list_type", "csv",
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

func (b *CommandBuilder) pipelineFor(rendition domain.VideoRendition) (*domain.HWAccelConfig, string, error) {
	if !rendition.Colour.Tonemap {
		return b.HWAccel, b.HWAccel.ScaleFilter, nil
	}
	filter, err := b.tonemap()
	if err != nil {
		return nil, "", err
	}
	if b.HWAccel.DeviceFrames {
		return hwaccel.NewConfig(domain.AccelNone), filter, nil
	}
	return b.HWAccel, filter, nil
}

func videoFilter(config *domain.HWAccelConfig, chain string, rendition domain.VideoRendition) string {
	var filters []string
	if !config.DeviceFrames {
		var assumed []string
		if rendition.Colour.AssumePrimaries != "" {
			assumed = append(assumed, "color_primaries="+rendition.Colour.AssumePrimaries)
		}
		if rendition.Colour.AssumeMatrix != "" {
			assumed = append(assumed, "colorspace="+rendition.Colour.AssumeMatrix)
		}
		if len(assumed) > 0 {
			filters = append(filters, "setparams="+strings.Join(assumed, ":"))
		}
	}
	return strings.Join(append(filters, fmt.Sprintf(chain, rendition.Width, rendition.Height)), ",")
}

func videoTailFilter(rendition domain.VideoRendition, first, last domain.Segment) string {
	if rendition.SourceEnd <= 0 || last.End-rendition.SourceEnd <= domain.BoundaryTolerance {
		return ""
	}
	return "tpad=stop_mode=clone:stop_duration=" + formatSeconds(last.End-rendition.SourceEnd+videoTailMarginSeconds)
}

func transcodeVideoArgs(config *domain.HWAccelConfig, tail, chain string, rendition domain.VideoRendition) ([]string, error) {
	if rendition.Width <= 0 || rendition.Height <= 0 || rendition.Bitrate <= 0 || rendition.PeakBitrate <= 0 || rendition.BufferSize <= 0 || rendition.Level <= 0 {
		return nil, fmt.Errorf("%w: %q", ErrRenditionIncomplete, rendition.Name)
	}

	args := append([]string(nil), config.EncodeFlags...)
	return append(args,
		"-vf", strings.TrimPrefix(tail+","+videoFilter(config, chain, rendition), ","),
		"-profile:v", "high",
		"-level:v", fmt.Sprintf("%d.%d", rendition.Level/10, rendition.Level%10),
		"-b:v", strconv.Itoa(rendition.Bitrate),
		"-maxrate", strconv.Itoa(rendition.PeakBitrate),
		"-bufsize", strconv.Itoa(rendition.BufferSize),
	), nil
}
