package hwaccel

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/mediaexec"
)

const (
	validationWidth  = 320
	validationHeight = 180
)

func Detect(ctx context.Context) ([]domain.Accelerator, error) {
	hwaccels, err := detectHWAccels(ctx)
	if err != nil {
		return nil, err
	}

	encoders, err := detectEncoders(ctx)
	if err != nil {
		return nil, err
	}

	var available []domain.Accelerator

	if hwaccels["cuda"] && encoders["h264_nvenc"] {
		available = append(available, domain.AccelCUDA)
	}
	if hwaccels["videotoolbox"] && encoders["h264_videotoolbox"] {
		available = append(available, domain.AccelVideoToolbox)
	}
	if hwaccels["vaapi"] && encoders["h264_vaapi"] {
		available = append(available, domain.AccelVAAPI)
	}
	if hwaccels["qsv"] && encoders["h264_qsv"] {
		available = append(available, domain.AccelQSV)
	}

	available = append(available, domain.AccelNone)

	return available, nil
}

func Select(available []domain.Accelerator) domain.Accelerator {
	priority := []domain.Accelerator{domain.AccelCUDA, domain.AccelQSV, domain.AccelVideoToolbox, domain.AccelVAAPI}

	for _, accel := range priority {
		for _, a := range available {
			if a == accel {
				return accel
			}
		}
	}

	return domain.AccelNone
}

func DetectBest() *domain.HWAccelConfig {
	return DetectBestContext(context.Background())
}

func DetectBestContext(ctx context.Context) *domain.HWAccelConfig {
	detectCtx, detectCancel := context.WithTimeout(ctx, 20*time.Second)
	defer detectCancel()
	available, err := Detect(detectCtx)
	if err != nil {
		cfg := NewConfig(domain.AccelNone)
		cfg.Requested = domain.AccelNone
		cfg.Available = []domain.Accelerator{domain.AccelNone}
		cfg.Error = err.Error()
		return cfg
	}
	selected := Select(available)
	cfg := NewConfig(selected)
	cfg.Requested = selected
	cfg.Available = available
	if selected != domain.AccelNone {
		validateCtx, cancel := context.WithTimeout(detectCtx, 15*time.Second)
		err = Validate(validateCtx, cfg)
		cancel()
		if err != nil {
			fallback := NewConfig(domain.AccelNone)
			fallback.Requested = selected
			fallback.Available = available
			fallback.Error = err.Error()
			return fallback
		}
	}
	return cfg
}

func NewConfig(accel domain.Accelerator) *domain.HWAccelConfig {
	return NewConfigWithMode(accel, domain.DecodeSafe)
}

func NewConfigWithMode(accel domain.Accelerator, mode domain.DecodeMode) *domain.HWAccelConfig {
	if mode == "" {
		mode = domain.DecodeSafe
	}
	switch accel {
	case domain.AccelCUDA:
		decodeFlags := []string{"-hwaccel", "cuda"}
		scaleFilter := "scale=%d:%d,setsar=1,format=yuv420p"
		deviceFrames := false
		if mode == domain.DecodeDevice {
			decodeFlags = []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"}
			scaleFilter = "scale_cuda=%d:%d:format=nv12,setsar=1"
			deviceFrames = true
		}
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelCUDA,
			Requested:    domain.AccelCUDA,
			DecodeFlags:  decodeFlags,
			EncodeFlags:  []string{"-c:v", "h264_nvenc", "-preset", "p4", "-tune", "ll"},
			Encoder:      "h264_nvenc",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  scaleFilter,
			DecodeMode:   mode,
			DeviceFrames: deviceFrames,
		}
	case domain.AccelVideoToolbox:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelVideoToolbox,
			Requested:    domain.AccelVideoToolbox,
			DecodeFlags:  []string{"-hwaccel", "videotoolbox"},
			EncodeFlags:  []string{"-c:v", "h264_videotoolbox", "-realtime", "true", "-prio_speed", "true"},
			Encoder:      "h264_videotoolbox",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
			DecodeMode:   mode,
		}
	case domain.AccelVAAPI:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelVAAPI,
			Requested:    domain.AccelVAAPI,
			DecodeFlags:  []string{"-hwaccel", "vaapi", "-vaapi_device", "/dev/dri/renderD128"},
			EncodeFlags:  []string{"-c:v", "h264_vaapi"},
			Encoder:      "h264_vaapi",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale_vaapi=%d:%d:format=nv12,setsar=1",
			DecodeMode:   domain.DecodeDevice,
			DeviceFrames: true,
		}
	case domain.AccelQSV:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelQSV,
			Requested:    domain.AccelQSV,
			DecodeFlags:  []string{"-hwaccel", "qsv", "-hwaccel_output_format", "qsv"},
			EncodeFlags:  []string{"-c:v", "h264_qsv", "-preset", "veryfast"},
			Encoder:      "h264_qsv",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale_qsv=%d:%d:format=nv12,setsar=1",
			DecodeMode:   domain.DecodeDevice,
			DeviceFrames: true,
		}
	default:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelNone,
			Requested:    accel,
			DecodeFlags:  []string{},
			EncodeFlags:  []string{"-c:v", "libx264", "-preset", "ultrafast"},
			Encoder:      "libx264",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale=%d:%d,setsar=1,format=yuv420p",
			DecodeMode:   domain.DecodeSafe,
		}
	}
}

func RequireConfig(accel domain.Accelerator) (*domain.HWAccelConfig, error) {
	return RequireConfigWithMode(accel, domain.DecodeSafe)
}

func RequireConfigWithMode(accel domain.Accelerator, mode domain.DecodeMode) (*domain.HWAccelConfig, error) {
	switch accel {
	case domain.AccelNone, domain.AccelCUDA, domain.AccelVideoToolbox, domain.AccelVAAPI, domain.AccelQSV:
		return NewConfigWithMode(accel, mode), nil
	default:
		return nil, fmt.Errorf("unsupported hardware accelerator %q", accel)
	}
}

func Validate(ctx context.Context, config *domain.HWAccelConfig) error {
	if config == nil || config.Accelerator == domain.AccelNone {
		return nil
	}
	directory, err := os.MkdirTemp("", "goshl-hwaccel-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	input := filepath.Join(directory, "input.mp4")
	if err := mediaexec.Run(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=24", validationWidth, validationHeight), "-frames:v", "12", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-y", input); err != nil {
		return fmt.Errorf("create validation input: %w", err)
	}
	args := []string{"-v", "error"}
	args = append(args, config.DecodeFlags...)
	args = append(args, "-i", input, "-vf", fmt.Sprintf(config.ScaleFilter, validationWidth, validationHeight))
	args = append(args, config.EncodeFlags...)
	args = append(args, "-frames:v", "1", "-f", "null", "-")
	if err := mediaexec.Run(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("validate %s pipeline: %w", config.Accelerator, err)
	}
	return nil
}

func detectHWAccels(ctx context.Context) (map[string]bool, error) {
	output, err := mediaexec.Output(ctx, "ffmpeg", "-hwaccels")
	if err != nil {
		return nil, err
	}

	result := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && line != "Hardware acceleration methods:" {
			result[line] = true
		}
	}

	return result, nil
}

func detectEncoders(ctx context.Context) (map[string]bool, error) {
	output, err := mediaexec.Output(ctx, "ffmpeg", "-encoders")
	if err != nil {
		return nil, err
	}

	result := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "h264_nvenc") {
			result["h264_nvenc"] = true
		}
		if strings.Contains(line, "h264_videotoolbox") {
			result["h264_videotoolbox"] = true
		}
		if strings.Contains(line, "h264_vaapi") {
			result["h264_vaapi"] = true
		}
		if strings.Contains(line, "h264_qsv") {
			result["h264_qsv"] = true
		}
	}

	return result, nil
}
