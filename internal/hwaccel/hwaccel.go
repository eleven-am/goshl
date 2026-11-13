package hwaccel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/mediaexec"
)

const (
	validationWidth  = 320
	validationHeight = 180
)

const (
	sdrTags             = "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv"
	softwareScaleFilter = "scale=%d:%d:out_color_matrix=bt709:out_range=tv,setsar=1,format=yuv420p," + sdrTags
	tonemapFilter       = "zscale=w=%d:h=%d:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,setsar=1,format=yuv420p," + sdrTags
)

var tonemapFilters = []string{"zscale", "tonemap"}

var ErrTonemapUnavailable = errors.New("HDR source cannot be tone mapped")

var validationLevels = []string{"3.0", "5.2"}

var (
	detectionTimeout  = 10 * time.Second
	validationTimeout = 15 * time.Second
)

var priority = []domain.Accelerator{domain.AccelCUDA, domain.AccelQSV, domain.AccelVideoToolbox, domain.AccelVAAPI}

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

func Candidates(available []domain.Accelerator) []domain.Accelerator {
	candidates := make([]domain.Accelerator, 0, len(priority))
	for _, accel := range priority {
		if slices.Contains(available, accel) {
			candidates = append(candidates, accel)
		}
	}
	return candidates
}

func Select(available []domain.Accelerator) domain.Accelerator {
	if candidates := Candidates(available); len(candidates) > 0 {
		return candidates[0]
	}
	return domain.AccelNone
}

func DetectBestContext(ctx context.Context) *domain.HWAccelConfig {
	detectCtx, detectCancel := context.WithTimeout(ctx, detectionTimeout)
	available, err := Detect(detectCtx)
	detectCancel()
	if err != nil {
		cfg := NewConfig(domain.AccelNone)
		cfg.Requested = domain.AccelNone
		cfg.Available = []domain.Accelerator{domain.AccelNone}
		cfg.Error = err.Error()
		return cfg
	}
	requested := Select(available)
	var failures []error
	for _, accel := range Candidates(available) {
		if err := ctx.Err(); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", accel, err))
			break
		}
		cfg := NewConfig(accel)
		validateCtx, cancel := context.WithTimeout(ctx, validationTimeout)
		err := Validate(validateCtx, cfg)
		cancel()
		if err == nil {
			cfg.Requested = requested
			cfg.Available = available
			cfg.Error = joinFailures(failures)
			return cfg
		}
		failures = append(failures, fmt.Errorf("%s: %w", accel, err))
	}
	fallback := NewConfig(domain.AccelNone)
	fallback.Requested = requested
	fallback.Available = available
	fallback.Error = joinFailures(failures)
	return fallback
}

func joinFailures(failures []error) string {
	if len(failures) == 0 {
		return ""
	}
	return errors.Join(failures...).Error()
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
		scaleFilter := softwareScaleFilter
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
			EncodeFlags:  []string{"-c:v", "h264_videotoolbox", "-realtime", "true", "-prio_speed", "true", "-g", "250"},
			Encoder:      "h264_videotoolbox",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  softwareScaleFilter,
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
			EncodeFlags:  []string{"-c:v", "libx264", "-preset", "superfast"},
			Encoder:      "libx264",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  softwareScaleFilter,
			DecodeMode:   domain.DecodeSafe,
		}
	}
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
	for _, level := range validationLevels {
		args := []string{"-v", "error"}
		args = append(args, config.DecodeFlags...)
		args = append(args, "-i", input, "-vf", fmt.Sprintf(config.ScaleFilter, validationWidth, validationHeight))
		args = append(args, config.EncodeFlags...)
		args = append(args, "-profile:v", "high", "-level:v", level)
		args = append(args, "-frames:v", "1", "-f", "null", "-")
		if err := mediaexec.Run(ctx, "ffmpeg", args...); err != nil {
			return fmt.Errorf("validate %s pipeline at level %s: %w", config.Accelerator, level, err)
		}
	}
	return nil
}

func DetectTonemap(ctx context.Context) (string, error) {
	output, err := mediaexec.Output(ctx, "ffmpeg", "-hide_banner", "-filters")
	if err != nil {
		return "", fmt.Errorf("list ffmpeg filters: %w", err)
	}
	present := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) >= 3 {
			present[fields[1]] = true
		}
	}
	var missing []string
	for _, name := range tonemapFilters {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%w: ffmpeg has no %s filter", ErrTonemapUnavailable, strings.Join(missing, " or "))
	}
	return tonemapFilter, nil
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
