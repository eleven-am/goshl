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
	"time"

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
	var groupBytes []int64
	cutShort := false
	if streams.Video.Codec != "" {
		var indexed bool
		var header headerFacts
		header, indexed, err = p.indexKeyframes(ctx, url, formatName, streams.Video.Index)
		if err != nil {
			return nil, err
		}
		keyframes, groupBytes = header.keyframes, header.bytes
		if !indexed {
			scan, err := p.probeKeyframes(ctx, url, streams.Video.Index)
			if err != nil {
				return nil, err
			}
			if scan.packets == 0 {
				return nil, fmt.Errorf("video stream has no readable packets")
			}
			keyframes, groupBytes = scan.keyframes, scan.bytes
			declared := streams.Video.End
			if scan.end > streams.StartTime {
				streams.Video.End = scan.end - streams.StartTime
			}
			cutShort = declared > 0 && streams.Video.End < declared-durationSlack
		}
	}

	if !cutShort {
		if err := p.confirmAudioEnds(ctx, url, streams); err != nil {
			return nil, err
		}
	}
	if err := p.measureStreamEnds(ctx, url, streams); err != nil {
		return nil, err
	}

	if latest, complete := latestStreamEnd(streams); complete && streamTimings(formatName) == timingMeasured {
		streams.Duration = math.Max(streams.Duration, latest)
	}
	streams.Duration = boundedDuration(streams)
	if cutShort {
		clampToVideoEnd(streams)
	}
	if err := validateMetadata(streams); err != nil {
		return nil, err
	}

	streams.Keyframes = normalizeKeyframes(keyframes, streams.StartTime, streams.Duration)
	streams.KeyframeBytes = assignKeyframeBytes(keyframes, groupBytes, streams.StartTime, streams.Keyframes)
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
	CodecTag    string            `json:"codec_tag_string"`
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
	ClosedCaps  int               `json:"closed_captions"`
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
	args := append([]string{"-v", "error", "-show_format", "-show_streams", "-analyze_frames", "-read_intervals", "%+#120", "-of", "json"}, input...)
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

	ends := endReader{formatStart: metadata.StartTime, timestampTags: writtenByLibavformat(ff.Format.Tags), container: streamTimings(ff.Format.FormatName)}
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
					CodecTag:       s.CodecTag,
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
					ClosedCaptions: s.ClosedCaps == 1,
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
				ChannelLayout: s.Layout,
				SampleRate:    parseBitrate(s.SampleRate),
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
	bytes     []int64
	leading   int64
	end       float64
	packets   int
}

func (scan *keyframeScan) add(parts []string) {
	scan.packets++
	var size int64
	if len(parts) >= 4 {
		size, _ = strconv.ParseInt(parts[2], 10, 64)
	}
	pts, err := strconv.ParseFloat(parts[0], 64)
	if err == nil {
		if duration, err := strconv.ParseFloat(parts[1], 64); err == nil && duration > 0 {
			scan.end = math.Max(scan.end, pts+duration)
		}
		if strings.Contains(strings.Join(parts[1:], ","), "K") {
			scan.keyframes = append(scan.keyframes, pts)
			scan.bytes = append(scan.bytes, scan.leading)
			scan.leading = 0
		}
	}
	if len(scan.bytes) == 0 {
		scan.leading += size
		return
	}
	scan.bytes[len(scan.bytes)-1] += size
}

func (p *Prober) probeKeyframes(ctx context.Context, url string, streamIndex int) (keyframeScan, error) {
	input, err := source.InputArgs(url)
	if err != nil {
		return keyframeScan{}, err
	}
	args := append([]string{
		"-v", "error",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,duration_time,size,flags",
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
		scan.add(parts)
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

var (
	streamEndTail   = 15.0
	streamEndSample = 5.0
	streamEndBudget = 20 * time.Second
)

func (p *Prober) measureStreamEnds(ctx context.Context, url string, metadata *domain.Metadata) error {
	unknown := make(map[int]*float64)
	if metadata.Video.Codec != "" && metadata.Video.End <= 0 {
		unknown[metadata.Video.Index] = &metadata.Video.End
	}
	for position := range metadata.Audios {
		if metadata.Audios[position].End <= 0 {
			unknown[metadata.Audios[position].Index] = &metadata.Audios[position].End
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	budget, stop := context.WithTimeout(ctx, streamEndBudget)
	defer stop()
	exhausted := func() bool {
		return ctx.Err() == nil && errors.Is(budget.Err(), context.DeadlineExceeded)
	}

	tail := math.Max(math.Max(metadata.Duration, metadata.Video.End)-streamEndTail, 0)
	ends, err := p.probeStreamEnds(budget, url, metadata.StartTime+tail, math.Inf(1))
	if err != nil {
		if exhausted() {
			return nil
		}
		return err
	}
	indexes := make([]int, 0, len(unknown))
	for index, target := range unknown {
		if end, found := ends[index]; found && end > metadata.StartTime {
			*target = roundMicros(end - metadata.StartTime)
			continue
		}
		indexes = append(indexes, index)
	}
	if tail == 0 {
		return nil
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		end, err := p.searchStreamEnd(budget, url, metadata.StartTime, index, tail)
		if err != nil {
			if exhausted() {
				return nil
			}
			return err
		}
		*unknown[index] = end
	}
	return nil
}

const audioTailCheck = 2.0

func (p *Prober) confirmAudioEnds(ctx context.Context, url string, metadata *domain.Metadata) error {
	floor := 0.0
	if metadata.Video.Codec != "" {
		if metadata.Video.End <= 0 {
			return nil
		}
		floor = metadata.Video.End + audioTailCheck
	}
	latest := 0.0
	for _, audio := range metadata.Audios {
		latest = math.Max(latest, audio.End)
	}
	if latest <= floor {
		return nil
	}
	budget, stop := context.WithTimeout(ctx, streamEndBudget)
	defer stop()
	from := math.Max(latest-streamEndTail, 0)
	ends, err := p.probeStreamEnds(budget, url, metadata.StartTime+from, math.Inf(1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if errors.Is(budget.Err(), context.DeadlineExceeded) {
			return nil
		}
		ends = nil
	}
	cut := false
	for position := range metadata.Audios {
		audio := &metadata.Audios[position]
		if audio.End <= floor || audio.End < from {
			continue
		}
		readable := 0.0
		if end, found := ends[audio.Index]; found && end > metadata.StartTime {
			readable = roundMicros(end - metadata.StartTime)
		}
		if readable < audio.End-durationSlack {
			audio.End = readable
			cut = true
		}
	}
	if !cut {
		return nil
	}
	for position := range metadata.Audios {
		if audio := &metadata.Audios[position]; audio.End > floor && audio.End < from {
			audio.End = 0
		}
	}
	return nil
}

func (p *Prober) searchStreamEnd(ctx context.Context, url string, start float64, index int, absentFrom float64) (float64, error) {
	alive, absent := 0.0, absentFrom
	for absent-alive > streamEndTail {
		middle := (alive + absent) / 2
		ends, err := p.probeStreamEnds(ctx, url, start+middle, start+middle+streamEndSample)
		if err != nil {
			return 0, err
		}
		if _, found := ends[index]; found {
			alive = middle
		} else {
			absent = middle
		}
	}
	ends, err := p.probeStreamEnds(ctx, url, start+alive, start+absentFrom+streamEndSample)
	if err != nil {
		return 0, err
	}
	if end, found := ends[index]; found && end > start {
		return roundMicros(end - start), nil
	}
	return 0, nil
}

func roundMicros(seconds float64) float64 {
	return math.Round(seconds*1e6) / 1e6
}

func streamEndArgs(from, until float64) []string {
	interval := strconv.FormatFloat(from, 'f', 6, 64) + "%"
	if !math.IsInf(until, 1) {
		interval += strconv.FormatFloat(until, 'f', 6, 64)
	}
	return []string{
		"-v", "error",
		"-show_entries", "packet=stream_index,pts_time,duration_time",
		"-of", "csv=p=0",
		"-read_intervals", interval,
	}
}

func (p *Prober) probeStreamEnds(ctx context.Context, url string, from, until float64) (map[int]float64, error) {
	input, err := source.InputArgs(url)
	if err != nil {
		return nil, err
	}
	args := append(streamEndArgs(from, until), input...)
	scanCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd, output, stderr, err := mediaexec.StartOutput(scanCtx, "ffprobe", args...)
	if err != nil {
		return nil, err
	}

	ends := make(map[int]float64)
	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ",")
		if len(parts) < 2 {
			continue
		}
		index, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		end, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			continue
		}
		if len(parts) > 2 {
			if duration, err := strconv.ParseFloat(parts[2], 64); err == nil && duration > 0 {
				end += duration
			}
		}
		if current, found := ends[index]; !found || end > current {
			ends[index] = end
		}
	}
	if err := scanner.Err(); err != nil {
		stop()
		_ = cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, mediaexec.Error("ffprobe", args, stderr.String(), err)
	}
	return ends, nil
}

type headerFacts struct {
	keyframes []float64
	bytes     []int64
}

func (p *Prober) indexKeyframes(ctx context.Context, url, formatName string, streamIndex int) (headerFacts, bool, error) {
	read := headerIndex(formatName)
	if read == nil {
		return headerFacts{}, false, nil
	}
	reader, err := mediasource.Open(ctx, url)
	if err != nil {
		if errors.Is(err, mediasource.ErrUnsupported) {
			return headerFacts{}, false, nil
		}
		return headerFacts{}, false, err
	}
	defer reader.Close()
	facts, err := read(reader, reader.Size(), streamIndex)
	if err != nil {
		if errors.Is(err, mp4index.ErrUnsupported) || errors.Is(err, mkvindex.ErrUnsupported) || errors.Is(err, mediasource.ErrUnsupported) {
			return headerFacts{}, false, nil
		}
		return headerFacts{}, false, err
	}
	return facts, true, nil
}

func headerIndex(formatName string) func(io.ReaderAt, int64, int) (headerFacts, error) {
	for _, name := range strings.Split(formatName, ",") {
		switch name {
		case "mp4", "mov":
			return func(r io.ReaderAt, size int64, streamIndex int) (headerFacts, error) {
				index, err := mp4index.IndexFrom(r, size, streamIndex)
				return headerFacts{keyframes: index.Keyframes, bytes: index.GroupBytes}, err
			}
		case "matroska", "webm":
			return func(r io.ReaderAt, size int64, streamIndex int) (headerFacts, error) {
				keyframes, groups, err := mkvindex.KeyframeGroupsFrom(r, size, streamIndex)
				if errors.Is(err, mkvindex.ErrUnsupported) {
					keyframes, err = mkvindex.KeyframesFrom(r, size, streamIndex)
					groups = nil
				}
				return headerFacts{keyframes: keyframes, bytes: groups}, err
			}
		}
	}
	return nil
}

func assignKeyframeBytes(raw []float64, groups []int64, startTime float64, keyframes []float64) []int64 {
	if len(groups) == 0 || len(groups) != len(raw) || len(keyframes) == 0 {
		return nil
	}
	assigned := make([]int64, len(keyframes))
	for position, keyframe := range raw {
		value := keyframe - startTime
		target := sort.Search(len(keyframes), func(i int) bool { return keyframes[i] > value+domain.BoundaryTolerance }) - 1
		if target < 0 {
			target = 0
		}
		assigned[target] += groups[position]
	}
	return assigned
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
		if math.Abs(value) < domain.BoundaryTolerance {
			value = 0
		}
		if value < 0 || value > duration+domain.BoundaryTolerance || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		normalized = append(normalized, value)
	}
	sort.Float64s(normalized)
	result := normalized[:0]
	for _, value := range normalized {
		if len(result) == 0 || math.Abs(result[len(result)-1]-value) > domain.BoundaryTolerance {
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

type streamTiming int

const (
	timingDeclared streamTiming = iota
	timingTagged
	timingMeasured
)

func streamTimings(formatName string) streamTiming {
	for _, name := range strings.Split(formatName, ",") {
		switch name {
		case "matroska", "webm":
			return timingTagged
		case "mpegts":
			return timingMeasured
		}
	}
	return timingDeclared
}

type endReader struct {
	formatStart   float64
	timestampTags bool
	container     streamTiming
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
	if r.container == timingMeasured {
		return 0
	}
	start, startErr := strconv.ParseFloat(s.StartTime, 64)
	duration, durationErr := strconv.ParseFloat(s.Duration, 64)
	filled := durationErr == nil && duration > 0
	end := 0.0
	if r.container == timingDeclared && filled && startErr == nil {
		end = start + duration
	} else if r.timestampTags {
		end = parseClock(s.Tags["DURATION"])
	} else if tagged := parseClock(statisticsTag(s.Tags, "DURATION")); tagged > 0 && startErr == nil && !filled {
		end = start + tagged
	}
	if end == 0 {
		return 0
	}
	end -= r.formatStart
	if end <= 0 || math.IsNaN(end) || math.IsInf(end, 0) {
		return 0
	}
	return roundMicros(end)
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
