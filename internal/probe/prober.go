package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/mediaexec"
	"github.com/eleven-am/goshl/internal/mediasource"
	"github.com/eleven-am/goshl/internal/probe/mkvindex"
	"github.com/eleven-am/goshl/internal/probe/mp4index"
	"github.com/eleven-am/goshl/internal/source"
)

type Prober struct{}

func NewProber() *Prober {
	return &Prober{}
}

func (p *Prober) Probe(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	return p.probe(ctx, sourceURL)
}

func (p *Prober) ProbeUncached(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	return p.probe(ctx, sourceURL)
}

func (p *Prober) probe(ctx context.Context, url string) (*domain.Metadata, error) {
	streams, formatName, err := p.probeStreams(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := validateStreams(streams); err != nil {
		return nil, err
	}

	var keyframes []float64
	cutShort := false
	if streams.Video.Codec != "" {
		var indexed bool
		keyframes, indexed, err = p.indexKeyframes(ctx, url, formatName, streams.Video.Index)
		if err != nil {
			return nil, err
		}
		if !indexed {
			scan, err := p.probeKeyframes(ctx, url, streams.Video.Index)
			if err != nil {
				return nil, err
			}
			if scan.packets == 0 {
				return nil, fmt.Errorf("video stream has no readable packets")
			}
			keyframes = scan.keyframes
			declared := streams.Video.End
			if scan.end > streams.StartTime {
				streams.Video.End = scan.end - streams.StartTime
			}
			cutShort = declared > 0 && streams.Video.End < declared-durationSlack
		}
	}

	streams.Duration = boundedDuration(streams)
	if cutShort {
		clampToVideoEnd(streams)
	}
	if err := validateMetadata(streams); err != nil {
		return nil, err
	}

	streams.Keyframes = normalizeKeyframes(keyframes, streams.StartTime, streams.Duration)
	streams.Video.StartsWithKeyframe = len(streams.Keyframes) > 0 && math.Abs(streams.Keyframes[0]-streams.Video.Start) < streamStartTolerance
	return streams, nil
}

const durationSlack = 0.25

func clampToVideoEnd(metadata *domain.Metadata) {
	end := metadata.Video.End
	if end <= 0 {
		return
	}
	if metadata.Duration <= 0 || metadata.Duration > end {
		metadata.Duration = end
	}
	for index := range metadata.Audios {
		if metadata.Audios[index].End > end {
			metadata.Audios[index].End = end
		}
	}
}

const streamStartTolerance = 0.0015

const maxDuration = 100 * 3600.0

func boundedDuration(metadata *domain.Metadata) float64 {
	latest, complete := latestStreamEnd(metadata)
	if metadata.Duration <= 0 {
		return latest
	}
	if complete && metadata.Duration > latest+durationSlack {
		return latest
	}
	return metadata.Duration
}

func latestStreamEnd(metadata *domain.Metadata) (float64, bool) {
	latest := 0.0
	complete := metadata.Video.Codec != "" || len(metadata.Audios) > 0
	if metadata.Video.Codec != "" {
		latest = metadata.Video.End
		complete = metadata.Video.End > 0
	}
	for _, audio := range metadata.Audios {
		latest = math.Max(latest, audio.End)
		complete = complete && audio.End > 0
	}
	return latest, complete
}

func validateStreams(metadata *domain.Metadata) error {
	if metadata == nil || metadata.Video.Codec == "" && len(metadata.Audios) == 0 {
		return fmt.Errorf("media has no playable audio or video stream")
	}
	if metadata.Video.Codec != "" && (metadata.Video.Width <= 0 || metadata.Video.Height <= 0) {
		return fmt.Errorf("video stream has invalid dimensions")
	}
	return nil
}

func validateMetadata(metadata *domain.Metadata) error {
	if metadata == nil || metadata.Duration <= 0 || math.IsNaN(metadata.Duration) || math.IsInf(metadata.Duration, 0) {
		return fmt.Errorf("media has no finite positive duration")
	}
	if metadata.Duration > maxDuration {
		return fmt.Errorf("media duration %.0fs exceeds the %.0fs limit", metadata.Duration, maxDuration)
	}
	return validateStreams(metadata)
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	Index       int               `json:"index"`
	CodecName   string            `json:"codec_name"`
	CodecType   string            `json:"codec_type"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Profile     string            `json:"profile"`
	Level       int               `json:"level"`
	PixelFormat string            `json:"pix_fmt"`
	BitDepth    string            `json:"bits_per_raw_sample"`
	RFrameRate  string            `json:"r_frame_rate"`
	AvgRate     string            `json:"avg_frame_rate"`
	StartTime   string            `json:"start_time"`
	Duration    string            `json:"duration"`
	Channels    int               `json:"channels"`
	SampleRate  string            `json:"sample_rate"`
	Layout      string            `json:"channel_layout"`
	BitRate     string            `json:"bit_rate"`
	ColorRange  string            `json:"color_range"`
	ColorSpace  string            `json:"color_space"`
	ColorTRC    string            `json:"color_transfer"`
	ColorPrim   string            `json:"color_primaries"`
	SAR         string            `json:"sample_aspect_ratio"`
	Tags        map[string]string `json:"tags"`
	Disposition ffprobeDisp       `json:"disposition"`
	SideData    []ffprobeSideData `json:"side_data_list"`
}

type ffprobeFormat struct {
	FormatName string            `json:"format_name"`
	StartTime  string            `json:"start_time"`
	Duration   string            `json:"duration"`
	BitRate    string            `json:"bit_rate"`
	Tags       map[string]string `json:"tags"`
}

type ffprobeDisp struct {
	Forced      int `json:"forced"`
	Default     int `json:"default"`
	AttachedPic int `json:"attached_pic"`
}

type ffprobeSideData struct {
	Rotation int `json:"rotation"`
}

func (p *Prober) probeStreams(ctx context.Context, url string) (*domain.Metadata, string, error) {
	input, err := source.InputArgs(url)
	if err != nil {
		return nil, "", err
	}
	args := append([]string{"-v", "error", "-show_format", "-show_streams", "-of", "json"}, input...)
	output, err := mediaexec.Output(ctx, "ffprobe", args...)
	if err != nil {
		return nil, "", err
	}

	var ff ffprobeOutput
	if err := json.Unmarshal(output, &ff); err != nil {
		return nil, "", err
	}

	metadata := &domain.Metadata{}

	if start, err := strconv.ParseFloat(ff.Format.StartTime, 64); err == nil {
		metadata.StartTime = start
	}
	if dur, err := strconv.ParseFloat(ff.Format.Duration, 64); err == nil {
		metadata.Duration = dur
	}
	metadata.Bitrate = parseBitrate(ff.Format.BitRate)

	ends := endReader{formatStart: metadata.StartTime, timestampTags: writtenByLibavformat(ff.Format.Tags)}
	videoIndex := selectVideoStream(ff.Streams)
	for _, s := range ff.Streams {
		switch s.CodecType {
		case "video":
			if s.Index == videoIndex {
				rotation := parseRotation(s.Tags, s.SideData)
				width, height := displayDimensions(s.Width, s.Height, s.SAR, rotation)
				metadata.Video = domain.VideoStream{
					Index:          s.Index,
					Codec:          s.CodecName,
					Profile:        s.Profile,
					Level:          s.Level,
					PixelFormat:    s.PixelFormat,
					BitDepth:       parseBitDepth(s.BitDepth, s.PixelFormat),
					Width:          width,
					Height:         height,
					Bitrate:        parseBitrate(firstNonEmpty(s.BitRate, statisticsTag(s.Tags, "BPS"))),
					FrameRate:      frameRate(s.AvgRate, s.RFrameRate),
					ColorRange:     s.ColorRange,
					ColorSpace:     s.ColorSpace,
					ColorTransfer:  s.ColorTRC,
					ColorPrimaries: s.ColorPrim,
					SampleAspect:   s.SAR,
					Rotation:       rotation,
					Default:        s.Disposition.Default == 1,
					Start:          ends.streamStart(s),
					End:            ends.streamEnd(s),
				}
			}
		case "audio":
			metadata.Audios = append(metadata.Audios, domain.AudioStream{
				Index:         s.Index,
				Codec:         s.CodecName,
				Language:      s.Tags["language"],
				Title:         s.Tags["title"],
				Channels:      s.Channels,
				SampleRate:    parseBitrate(s.SampleRate),
				ChannelLayout: s.Layout,
				Bitrate:       parseBitrate(firstNonEmpty(s.BitRate, statisticsTag(s.Tags, "BPS"))),
				Default:       s.Disposition.Default == 1,
				End:           ends.streamEnd(s),
			})
		case "subtitle":
			metadata.Subtitles = append(metadata.Subtitles, domain.SubtitleStream{
				Index:    s.Index,
				ID:       fmt.Sprintf("subtitle-%d", s.Index),
				Codec:    s.CodecName,
				Language: s.Tags["language"],
				Title:    s.Tags["title"],
				Forced:   s.Disposition.Forced == 1,
				Default:  s.Disposition.Default == 1,
			})
		}
	}

	return metadata, ff.Format.FormatName, nil
}

type keyframeScan struct {
	keyframes []float64
	end       float64
	packets   int
}

func (p *Prober) probeKeyframes(ctx context.Context, url string, streamIndex int) (keyframeScan, error) {
	input, err := source.InputArgs(url)
	if err != nil {
		return keyframeScan{}, err
	}
	args := append([]string{
		"-v", "error",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,duration_time,flags",
		"-of", "csv=p=0",
	}, input...)
	scanCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd, output, stderr, err := mediaexec.StartOutput(scanCtx, "ffprobe", args...)
	if err != nil {
		return keyframeScan{}, err
	}

	var scan keyframeScan
	scanner := bufio.NewScanner(output)

	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ",")
		if len(parts) < 2 {
			continue
		}
		scan.packets++
		pts, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			continue
		}
		if duration, err := strconv.ParseFloat(parts[1], 64); err == nil && duration > 0 {
			scan.end = math.Max(scan.end, pts+duration)
		}
		if strings.Contains(strings.Join(parts[1:], ","), "K") {
			scan.keyframes = append(scan.keyframes, pts)
		}
	}

	if err := scanner.Err(); err != nil {
		stop()
		_ = cmd.Wait()
		return keyframeScan{}, err
	}
	if err := cmd.Wait(); err != nil {
		return keyframeScan{}, mediaexec.Error("ffprobe", args, stderr.String(), err)
	}

	return scan, nil
}

func (p *Prober) indexKeyframes(ctx context.Context, url, formatName string, streamIndex int) ([]float64, bool, error) {
	read := headerIndex(formatName)
	if read == nil {
		return nil, false, nil
	}
	reader, err := mediasource.Open(ctx, url)
	if err != nil {
		if errors.Is(err, mediasource.ErrUnsupported) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer reader.Close()
	keyframes, err := read(reader, reader.Size(), streamIndex)
	if err != nil {
		if errors.Is(err, mp4index.ErrUnsupported) || errors.Is(err, mkvindex.ErrUnsupported) || errors.Is(err, mediasource.ErrUnsupported) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return keyframes, true, nil
}

func headerIndex(formatName string) func(io.ReaderAt, int64, int) ([]float64, error) {
	for _, name := range strings.Split(formatName, ",") {
		switch name {
		case "mp4", "mov":
			return mp4index.KeyframesFrom
		case "matroska", "webm":
			return mkvindex.KeyframesFrom
		}
	}
	return nil
}

func selectVideoStream(streams []ffprobeStream) int {
	selected := -1
	for _, stream := range streams {
		if stream.CodecType != "video" || stream.Disposition.AttachedPic == 1 {
			continue
		}
		if selected == -1 || stream.Disposition.Default == 1 {
			selected = stream.Index
		}
		if stream.Disposition.Default == 1 {
			break
		}
	}
	return selected
}

func normalizeKeyframes(keyframes []float64, startTime, duration float64) []float64 {
	normalized := make([]float64, 0, len(keyframes))
	for _, keyframe := range keyframes {
		value := keyframe - startTime
		if math.Abs(value) < 0.000001 {
			value = 0
		}
		if value < 0 || value > duration+0.000001 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		normalized = append(normalized, value)
	}
	sort.Float64s(normalized)
	result := normalized[:0]
	for _, value := range normalized {
		if len(result) == 0 || math.Abs(result[len(result)-1]-value) > 0.000001 {
			result = append(result, value)
		}
	}
	return result
}

func parseBitDepth(raw, pixelFormat string) int {
	if value, err := strconv.Atoi(raw); err == nil && value > 0 {
		return value
	}
	for _, depth := range []int{16, 14, 12, 10, 9} {
		if strings.Contains(pixelFormat, strconv.Itoa(depth)) {
			return depth
		}
	}
	if pixelFormat != "" {
		return 8
	}
	return 0
}

func parseRotation(tags map[string]string, sideData []ffprobeSideData) int {
	for _, data := range sideData {
		if data.Rotation != 0 {
			return normalizedRotation(data.Rotation)
		}
	}
	value, _ := strconv.Atoi(tags["rotate"])
	return normalizedRotation(value)
}

func normalizedRotation(rotation int) int {
	rotation %= 360
	if rotation < 0 {
		rotation += 360
	}
	return rotation
}

func displayDimensions(width, height int, sampleAspect string, rotation int) (int, int) {
	parts := strings.Split(sampleAspect, ":")
	if len(parts) == 2 {
		numerator, numeratorErr := strconv.ParseFloat(parts[0], 64)
		denominator, denominatorErr := strconv.ParseFloat(parts[1], 64)
		if numeratorErr == nil && denominatorErr == nil && numerator > 0 && denominator > 0 {
			width = int(math.Round(float64(width) * numerator / denominator))
			if width%2 != 0 {
				width++
			}
		}
	}
	if normalizedRotation(rotation)%180 != 0 {
		return height, width
	}
	return width, height
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseBitrate(s string) int {
	if s == "" {
		return 0
	}
	v, _ := strconv.Atoi(s)
	return v
}

func parseFrameRate(s string) float64 {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0
	}
	num, _ := strconv.ParseFloat(parts[0], 64)
	den, _ := strconv.ParseFloat(parts[1], 64)
	if den == 0 {
		return 0
	}
	return num / den
}

func frameRate(average, real string) float64 {
	if rate := parseFrameRate(average); rate > 0 {
		return rate
	}
	return parseFrameRate(real)
}

func statisticsTag(tags map[string]string, name string) string {
	if value := tags[name]; value != "" {
		return value
	}
	for key, value := range tags {
		if strings.HasPrefix(key, name+"-") && value != "" {
			return value
		}
	}
	return ""
}

type endReader struct {
	formatStart   float64
	timestampTags bool
}

func writtenByLibavformat(formatTags map[string]string) bool {
	for key, value := range formatTags {
		if strings.EqualFold(key, "encoder") && strings.HasPrefix(value, "Lavf") {
			return true
		}
	}
	return false
}

func (r endReader) streamStart(s ffprobeStream) float64 {
	start, err := strconv.ParseFloat(s.StartTime, 64)
	if err != nil || math.IsNaN(start) || math.IsInf(start, 0) || start <= r.formatStart {
		return 0
	}
	return start - r.formatStart
}

func (r endReader) streamEnd(s ffprobeStream) float64 {
	start, startErr := strconv.ParseFloat(s.StartTime, 64)
	end := 0.0
	if duration, err := strconv.ParseFloat(s.Duration, 64); err == nil && startErr == nil && duration > 0 {
		end = start + duration
	} else if r.timestampTags {
		end = parseClock(s.Tags["DURATION"])
	} else if tagged := parseClock(statisticsTag(s.Tags, "DURATION")); tagged > 0 && startErr == nil {
		end = start + tagged
	}
	if end == 0 {
		return 0
	}
	end -= r.formatStart
	if end <= 0 || math.IsNaN(end) || math.IsInf(end, 0) {
		return 0
	}
	return math.Round(end*1e6) / 1e6
}

func parseClock(value string) float64 {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0
	}
	hours, hoursErr := strconv.ParseFloat(parts[0], 64)
	minutes, minutesErr := strconv.ParseFloat(parts[1], 64)
	seconds, secondsErr := strconv.ParseFloat(parts[2], 64)
	if hoursErr != nil || minutesErr != nil || secondsErr != nil || hours < 0 || minutes < 0 || seconds < 0 {
		return 0
	}
	return hours*3600 + minutes*60 + seconds
}
