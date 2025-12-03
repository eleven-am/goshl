package hwaccel

import (
	"bufio"
	"context"
	"os/exec"
	"strings"

	"github.com/eleven-am/goshl/internal/domain"
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
	available, err := Detect(context.Background())
	if err != nil {
		return NewConfig(domain.AccelNone)
	}
	return NewConfig(Select(available))
}

func NewConfig(accel domain.Accelerator) *domain.HWAccelConfig {
	switch accel {
	case domain.AccelCUDA:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelCUDA,
			DecodeFlags:  []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"},
			EncodeFlags:  []string{"-c:v", "h264_nvenc", "-preset", "p4", "-tune", "ll"},
			Encoder:      "h264_nvenc",
			KeyframeFlag: "-force_idr",
			ScaleFilter:  "scale_cuda=%d:%d:format=nv12",
		}
	case domain.AccelVideoToolbox:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelVideoToolbox,
			DecodeFlags:  []string{"-hwaccel", "videotoolbox"},
			EncodeFlags:  []string{"-c:v", "h264_videotoolbox", "-realtime", "true", "-prio_speed", "true"},
			Encoder:      "h264_videotoolbox",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale=%d:%d",
		}
	case domain.AccelVAAPI:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelVAAPI,
			DecodeFlags:  []string{"-hwaccel", "vaapi", "-vaapi_device", "/dev/dri/renderD128"},
			EncodeFlags:  []string{"-c:v", "h264_vaapi"},
			Encoder:      "h264_vaapi",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale_vaapi=%d:%d:format=nv12",
		}
	case domain.AccelQSV:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelQSV,
			DecodeFlags:  []string{"-hwaccel", "qsv", "-hwaccel_output_format", "qsv"},
			EncodeFlags:  []string{"-c:v", "h264_qsv", "-preset", "veryfast"},
			Encoder:      "h264_qsv",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale_qsv=%d:%d:format=nv12",
		}
	default:
		return &domain.HWAccelConfig{
			Accelerator:  domain.AccelNone,
			DecodeFlags:  []string{},
			EncodeFlags:  []string{"-c:v", "libx264", "-preset", "ultrafast"},
			Encoder:      "libx264",
			KeyframeFlag: "-force_key_frames",
			ScaleFilter:  "scale=%d:%d",
		}
	}
}

func detectHWAccels(ctx context.Context) (map[string]bool, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hwaccels")
	output, err := cmd.Output()
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
	cmd := exec.CommandContext(ctx, "ffmpeg", "-encoders")
	output, err := cmd.Output()
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
