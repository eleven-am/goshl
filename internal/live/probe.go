package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/eleven-am/goshl/internal/mediaexec"
)

const (
	probeChunk    = 64 << 10
	maxProbeBytes = 16 << 20
)

type probeStream struct {
	Index          int    `json:"index"`
	ID             string `json:"id"`
	CodecType      string `json:"codec_type"`
	CodecName      string `json:"codec_name"`
	Profile        string `json:"profile"`
	PixFmt         string `json:"pix_fmt"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	SAR            string `json:"sample_aspect_ratio"`
	FieldOrder     string `json:"field_order"`
	AvgFrameRate   string `json:"avg_frame_rate"`
	RFrameRate     string `json:"r_frame_rate"`
	Channels       int    `json:"channels"`
	SampleRate     string `json:"sample_rate"`
	ColorTransfer  string `json:"color_transfer"`
	ColorPrimaries string `json:"color_primaries"`
	ColorSpace     string `json:"color_space"`
	Tags           struct {
		Language string `json:"language"`
	} `json:"tags"`
}

type probeOutput struct {
	Programs []struct {
		Streams []probeStream `json:"streams"`
	} `json:"programs"`
	Streams []probeStream `json:"streams"`
}

type timedCloser struct {
	timer *time.Timer
	fired chan struct{}
}

func closeAfter(rc io.Closer, budget time.Duration) *timedCloser {
	t := &timedCloser{fired: make(chan struct{})}
	t.timer = time.AfterFunc(budget, func() {
		close(t.fired)
		rc.Close()
	})
	return t
}

func (t *timedCloser) stop() bool {
	return t.timer.Stop()
}

func (t *timedCloser) expired() bool {
	select {
	case <-t.fired:
		return true
	default:
		return false
	}
}

func probeArgs() []string {
	return []string{
		"-v", "error",
		"-analyzeduration", "2000000",
		"-probesize", "3000000",
		"-f", "mpegts",
		"-format_whitelist", "mpegts",
		"-protocol_whitelist", "pipe",
		"-show_streams", "-show_programs",
		"-of", "json",
		"-i", "pipe:0",
	}
}

func probeReader(ctx context.Context, rc io.Reader, deadline time.Time) (ProbeInfo, []byte, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", probeArgs()...)
	cmd.WaitDelay = 5 * time.Second
	stdout := mediaexec.NewLimitedBuffer(4 << 20)
	stderr := mediaexec.NewLimitedBuffer(64 << 10)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ProbeInfo{}, nil, err
	}
	if err := cmd.Start(); err != nil {
		return ProbeInfo{}, nil, err
	}
	var head bytes.Buffer
	var readErr error
	chunk := make([]byte, probeChunk)
	for head.Len() < maxProbeBytes && time.Now().Before(deadline) {
		n, err := rc.Read(chunk)
		if n > 0 {
			head.Write(chunk[:n])
			if _, werr := stdin.Write(chunk[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	stdin.Close()
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ProbeInfo{}, nil, ctx.Err()
	}
	if head.Len() == 0 {
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return ProbeInfo{}, nil, fmt.Errorf("%w: %w", ErrNoData, readErr)
		}
		return ProbeInfo{}, nil, ErrNoData
	}
	var parsed probeOutput
	if waitErr != nil || json.Unmarshal(stdout.Bytes(), &parsed) != nil {
		return ProbeInfo{}, nil, fmt.Errorf("%w: stream is not MPEG-TS", ErrNoData)
	}
	info, err := interpret(parsed)
	if err != nil {
		return ProbeInfo{}, nil, err
	}
	info.ProbedBytes = head.Len()
	if readErr != nil {
		return info, head.Bytes(), fmt.Errorf("%w: %w", ErrSourceUnavailable, readErr)
	}
	return info, head.Bytes(), nil
}

func interpret(parsed probeOutput) (ProbeInfo, error) {
	streams := parsed.Streams
	programs := 0
	for _, program := range parsed.Programs {
		if len(program.Streams) > 0 {
			programs++
		}
	}
	for _, program := range parsed.Programs {
		if hasVideo(program.Streams) {
			streams = program.Streams
			break
		}
	}
	info := ProbeInfo{Programs: programs, SelectedAudio: -1}
	found := false
	for _, stream := range streams {
		pid, err := strconv.ParseInt(strings.TrimPrefix(stream.ID, "0x"), 16, 32)
		if err != nil {
			continue
		}
		switch stream.CodecType {
		case "video":
			if found || stream.Width == 0 || stream.Height == 0 {
				continue
			}
			found = true
			info.Video = VideoInfo{
				PID:         int(pid),
				Codec:       stream.CodecName,
				Profile:     stream.Profile,
				PixelFormat: stream.PixFmt,
				Width:       stream.Width,
				Height:      stream.Height,
				DisplayW:    displayWidth(stream.Width, stream.SAR),
				FrameRate:   frameRate(stream.AvgFrameRate, stream.RFrameRate),
				Interlaced:  interlaced(stream.FieldOrder),
				Transfer:    stream.ColorTransfer,
				Primaries:   stream.ColorPrimaries,
				Matrix:      stream.ColorSpace,
			}
		case "audio":
			rate, _ := strconv.Atoi(stream.SampleRate)
			if stream.Channels == 0 || rate == 0 {
				continue
			}
			info.Audios = append(info.Audios, AudioInfo{PID: int(pid), Codec: stream.CodecName, Language: stream.Tags.Language, Channels: stream.Channels, SampleRate: rate})
		}
	}
	if !found {
		if len(info.Audios) > 0 {
			return info, fmt.Errorf("%w: no video stream", ErrUnsupportedCodec)
		}
		return info, fmt.Errorf("%w: no decodable stream found", ErrNoData)
	}
	return info, nil
}

func hasVideo(streams []probeStream) bool {
	for _, stream := range streams {
		if stream.CodecType == "video" && stream.Width > 0 {
			return true
		}
	}
	return false
}

func interlaced(fieldOrder string) bool {
	switch fieldOrder {
	case "tt", "bb", "tb", "bt":
		return true
	}
	return false
}

func displayWidth(width int, sar string) int {
	var num, den int
	if _, err := fmt.Sscanf(sar, "%d:%d", &num, &den); err != nil || num <= 0 || den <= 0 {
		return width
	}
	return width * num / den
}

func frameRate(values ...string) float64 {
	for _, value := range values {
		var num, den float64
		if _, err := fmt.Sscanf(value, "%f/%f", &num, &den); err == nil && num > 0 && den > 0 {
			return num / den
		}
	}
	return 25
}
