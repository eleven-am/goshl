package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
)

type Prober struct {
	storage domain.Storage
}

func NewProber(storage domain.Storage) *Prober {
	return &Prober{storage: storage}
}

func (p *Prober) Probe(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	exists, err := p.storage.MetadataExists(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	if exists {
		data, err := p.storage.GetMetadata(ctx, sourceURL)
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

	if err := p.storage.SetMetadata(ctx, sourceURL, data); err != nil {
		return nil, err
	}

	return metadata, nil
}

func (p *Prober) probe(ctx context.Context, url string) (*domain.Metadata, error) {
	streams, err := p.probeStreams(ctx, url)
	if err != nil {
		return nil, err
	}

	keyframes, err := p.probeKeyframes(ctx, url)
	if err != nil {
		return nil, err
	}

	streams.Keyframes = keyframes
	return streams, nil
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
	RFrameRate  string            `json:"r_frame_rate"`
	Channels    int               `json:"channels"`
	BitRate     string            `json:"bit_rate"`
	Tags        map[string]string `json:"tags"`
	Disposition ffprobeDisp       `json:"disposition"`
}

type ffprobeFormat struct {
	Duration string `json:"duration"`
}

type ffprobeDisp struct {
	Forced int `json:"forced"`
}

func (p *Prober) probeStreams(ctx context.Context, url string) (*domain.Metadata, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_format",
		"-show_streams",
		"-of", "json",
		url,
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var ff ffprobeOutput
	if err := json.Unmarshal(output, &ff); err != nil {
		return nil, err
	}

	metadata := &domain.Metadata{}

	if dur, err := strconv.ParseFloat(ff.Format.Duration, 64); err == nil {
		metadata.Duration = dur
	}

	for _, s := range ff.Streams {
		switch s.CodecType {
		case "video":
			if metadata.Video.Index == 0 && metadata.Video.Codec == "" {
				metadata.Video = domain.VideoStream{
					Index:     s.Index,
					Codec:     s.CodecName,
					Width:     s.Width,
					Height:    s.Height,
					Bitrate:   parseBitrate(s.Tags["BPS"]),
					FrameRate: parseFrameRate(s.RFrameRate),
				}
			}
		case "audio":
			metadata.Audios = append(metadata.Audios, domain.AudioStream{
				Index:    s.Index,
				Codec:    s.CodecName,
				Language: s.Tags["language"],
				Channels: s.Channels,
				Bitrate:  parseBitrate(s.BitRate),
			})
		case "subtitle":
			metadata.Subtitles = append(metadata.Subtitles, domain.SubtitleStream{
				Index:    s.Index,
				Codec:    s.CodecName,
				Language: s.Tags["language"],
				Forced:   s.Disposition.Forced == 1,
			})
		}
	}

	return metadata, nil
}

func (p *Prober) probeKeyframes(ctx context.Context, url string) ([]float64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0",
		url,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var keyframes []float64
	scanner := bufio.NewScanner(stdout)

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

	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	return keyframes, nil
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
