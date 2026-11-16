package ffmpeg

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/eleven-am/goshl/internal/domain"
)

const (
	LiveSegmentPattern  = "seg-%d.ts"
	liveDeinterlace     = "bwdif=mode=send_frame:parity=auto:deint=all"
	liveAnalyzeDuration = "2000000"
	liveProbeSize       = "3000000"
	liveAudioBitrate    = 128000
)

type LiveParams struct {
	VideoPID        int
	AudioPID        int
	HasAudio        bool
	CopyVideo       bool
	CopyAudio       bool
	Interlaced      bool
	Rendition       domain.VideoRendition
	SegmentSeconds  float64
	StartNumber     int
	TimelineSeconds float64
	OutputDir       string
}

func LiveInputArgs() []string {
	return []string{
		"-fflags", "+discardcorrupt+genpts",
		"-analyzeduration", liveAnalyzeDuration,
		"-probesize", liveProbeSize,
		"-f", "mpegts",
		"-format_whitelist", "mpegts",
		"-protocol_whitelist", "pipe",
		"-i", "pipe:0",
	}
}

func (b *CommandBuilder) Live(p LiveParams) ([]string, error) {
	args := []string{"-nostats", "-hide_banner", "-loglevel", "warning"}
	var video []string
	if p.CopyVideo {
		video = []string{"-c:v", "copy"}
	} else {
		config, chain, err := b.pipelineFor(p.Rendition)
		if err != nil {
			return nil, err
		}
		tail := ""
		if p.Interlaced {
			if config.DeviceFrames {
				// TODO: select the device deinterlacer (bwdif_cuda, deinterlace_vaapi, vpp_qsv) once verified on that hardware
				return nil, fmt.Errorf("interlaced live source on %s device frames is not supported", config.Accelerator)
			}
			tail = liveDeinterlace
		}
		args = append(args, config.DecodeFlags...)
		video, err = transcodeVideoArgs(config, tail, chain, p.Rendition)
		if err != nil {
			return nil, err
		}
		video = append(video, config.KeyframeFlag, fmt.Sprintf("expr:gte(t,n_forced*%s)", formatSeconds(p.SegmentSeconds)))
		if config.Accelerator == domain.AccelCUDA {
			video = append(video, "-forced-idr", "1")
		}
	}
	args = append(args, LiveInputArgs()...)
	args = append(args, "-map", fmt.Sprintf("0:i:%d", p.VideoPID))
	if p.HasAudio {
		args = append(args, "-map", fmt.Sprintf("0:i:%d", p.AudioPID))
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-sn", "-dn")
	args = append(args, video...)
	if p.HasAudio {
		if p.CopyAudio {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-ac", "2", "-ar", strconv.Itoa(aacSampleRate), "-b:a", strconv.Itoa(liveAudioBitrate), "-af", "aresample=async=1")
		}
	}
	args = append(args,
		"-muxdelay", "0",
		"-output_ts_offset", formatSeconds(domain.OutputTimelineOffset.Seconds()+p.TimelineSeconds),
		"-max_muxing_queue_size", "4096",
		"-f", "segment",
		"-segment_time", formatSeconds(p.SegmentSeconds),
		"-segment_format", "mpegts",
		"-segment_format_options", "mpegts_flags=+initial_discontinuity",
		"-segment_list_type", "csv",
		"-segment_list", "pipe:1",
		"-segment_start_number", strconv.Itoa(p.StartNumber),
		filepath.Join(p.OutputDir, LiveSegmentPattern),
	)
	return args, nil
}
