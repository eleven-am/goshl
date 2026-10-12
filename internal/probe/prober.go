package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/localpath"
	"github.com/eleven-am/goshl/internal/mediaexec"
	"github.com/eleven-am/goshl/internal/probe/mp4index"
)

type Prober struct {
	storage domain.Storage
}

func NewProber(storage domain.Storage) *Prober {
	return &Prober{storage: storage}
}

func (p *Prober) Probe(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	return p.ProbeWithKey(ctx, sourceURL, sourceURL)
}

func (p *Prober) ProbeWithKey(ctx context.Context, sourceURL, mediaKey string) (*domain.Metadata, error) {
	exists, err := p.storage.MetadataExists(ctx, mediaKey)
	if err != nil {
		return nil, err
	}
	if exists {
		data, err := p.storage.GetMetadata(ctx, mediaKey)
		if err != nil {
			return nil, err
		}
		var meta domain.Metadata
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil, err
		}
		return &meta, nil
	}

	metadata, err := p.probe(ctx, sourceURL)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	if err := p.storage.SetMetadata(ctx, mediaKey, data); err != nil {
		return nil, err
	}

	return metadata, nil
}

func (p *Prober) probe(ctx context.Context, url string) (*domain.Metadata, error) {
	streams, formatName, err := p.probeStreams(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := validateMetadata(streams); err != nil {
		return nil, err
	}

	var keyframes []float64
	if streams.Video.Codec != "" {
		var indexed bool
		keyframes, indexed = p.indexKeyframes(url, formatName, streams.Video.Index)
		if !indexed {
			keyframes, err = p.probeKeyframes(ctx, url, streams.Video.Index)
			if err != nil {
				return nil, err
			}
		}
	}

	streams.Keyframes = normalizeKeyframes(keyframes, streams.StartTime, streams.Duration)
	streams.Video.StartsWithKeyframe = len(streams.Keyframes) > 0 && math.Abs(streams.Keyframes[0]) < 0.000001
	return streams, nil
}

func validateMetadata(metadata *domain.Metadata) error {
	if metadata == nil || metadata.Duration <= 0 || math.IsNaN(metadata.Duration) || math.IsInf(metadata.Duration, 0) {
		return fmt.Errorf("media has no finite positive duration")
	}
	if metadata.Video.Codec == "" && len(metadata.Audios) == 0 {
		return fmt.Errorf("media has no playable audio or video stream")
	}
	if metadata.Video.Codec != "" && (metadata.Video.Width <= 0 || metadata.Video.Height <= 0) {
		return fmt.Errorf("video stream has invalid dimensions")
	}
	return nil
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
	Channels    int               `json:"channels"`
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
	FormatName string `json:"format_name"`
	StartTime  string `json:"start_time"`
	Duration   string `json:"duration"`
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
	output, err := mediaexec.Output(ctx, "ffprobe",
		"-v", "error",
		"-show_format",
		"-show_streams",
		"-of", "json",
		url,
	)
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
					Bitrate:        parseBitrate(firstNonEmpty(s.BitRate, s.Tags["BPS"])),
					FrameRate:      parseFrameRate(s.RFrameRate),
					ColorRange:     s.ColorRange,
					ColorSpace:     s.ColorSpace,
					ColorTransfer:  s.ColorTRC,
					ColorPrimaries: s.ColorPrim,
					SampleAspect:   s.SAR,
					Rotation:       rotation,
					Default:        s.Disposition.Default == 1,
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
				Bitrate:       parseBitrate(s.BitRate),
				Default:       s.Disposition.Default == 1,
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

func (p *Prober) probeKeyframes(ctx context.Context, url string, streamIndex int) ([]float64, error) {
	args := []string{
		"-v", "error",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0",
		url,
	}
	cmd, output, stderr, err := mediaexec.StartOutput(ctx, "ffprobe", args...)
	if err != nil {
		return nil, err
	}

	var keyframes []float64
	scanner := bufio.NewScanner(output)

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		if !strings.Contains(parts[1], "K") {
			continue
		}
		pts, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			continue
		}
		keyframes = append(keyframes, pts)
	}

	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, mediaexec.Error("ffprobe", args, stderr.String(), err)
	}

	return keyframes, nil
}

func (p *Prober) indexKeyframes(url, formatName string, streamIndex int) ([]float64, bool) {
	if !isISOBaseFormat(formatName) {
		return nil, false
	}
	path, local := localpath.Resolve(url)
	if !local {
		return nil, false
	}
	keyframes, err := mp4index.Keyframes(path, streamIndex)
	if err != nil {
		return nil, false
	}
	return keyframes, true
}

func isISOBaseFormat(formatName string) bool {
	for _, name := range strings.Split(formatName, ",") {
		if name == "mp4" || name == "mov" {
			return true
		}
	}
	return false
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
