package ffmpeg

import (
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/rendition"
)

func liveRendition(t *testing.T, codec string, width, height, display int, rate float64) domain.VideoRendition {
	t.Helper()
	planned, err := rendition.Live(domain.VideoStream{Codec: codec, Width: width, Height: height, FrameRate: rate}, display, 720)
	if err != nil {
		t.Fatal(err)
	}
	return planned
}

func TestLiveArgs(t *testing.T) {
	cases := []struct {
		name   string
		accel  domain.Accelerator
		params LiveParams
		want   []string
		reject []string
	}{
		{"h264 aac copy", domain.AccelNone, LiveParams{VideoPID: 256, AudioPID: 257, HasAudio: true, CopyVideo: true, CopyAudio: true}, []string{"-c:v copy", "-c:a copy", "-protocol_whitelist pipe -i pipe:0", "-map 0:i:256 -map 0:i:257"}, []string{"-vf", "libx264", "-af"}},
		{"h264 mp2", domain.AccelNone, LiveParams{VideoPID: 256, AudioPID: 257, HasAudio: true, CopyVideo: true}, []string{"-c:v copy", "-c:a aac -ac 2 -ar 48000 -b:a 128000 -af aresample=async=1"}, []string{"-vf"}},
		{"mpeg2 interlaced software", domain.AccelNone, LiveParams{VideoPID: 256, AudioPID: 257, HasAudio: true, Interlaced: true, Rendition: liveRendition(t, "mpeg2video", 720, 576, 1024, 25)}, []string{"-c:v libx264", "-vf bwdif=mode=send_frame:parity=auto:deint=all,", "scale=1024:576:out_color_matrix=bt709", "-force_key_frames expr:gte(t,n_forced*4.000000)", "-c:a aac"}, nil},
		{"hevc 1080p50 software", domain.AccelNone, LiveParams{VideoPID: 256, AudioPID: 257, HasAudio: true, Rendition: liveRendition(t, "hevc", 1920, 1080, 1920, 50)}, []string{"-c:v libx264", "scale=1280:720", "-level:v 3.2"}, []string{"bwdif"}},
		{"hevc cuda", domain.AccelCUDA, LiveParams{VideoPID: 256, Rendition: liveRendition(t, "hevc", 1920, 1080, 1920, 50)}, []string{"-hwaccel cuda", "-c:v h264_nvenc", "-forced-idr 1", "-an"}, []string{"-c:a"}},
		{"mpeg2 interlaced cuda", domain.AccelCUDA, LiveParams{VideoPID: 256, AudioPID: 257, HasAudio: true, Interlaced: true, Rendition: liveRendition(t, "mpeg2video", 720, 576, 1024, 25)}, []string{"-hwaccel cuda", "bwdif=mode=send_frame", "h264_nvenc"}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.params.SegmentSeconds = 4
			test.params.StartNumber = 17
			test.params.TimelineSeconds = 68.5
			test.params.OutputDir = "/work"
			args, err := NewCommandBuilder(hwaccel.NewConfig(test.accel)).Live(test.params)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			t.Logf("ffmpeg %s", joined)
			for _, want := range append(test.want, "-output_ts_offset 78.500000", "-segment_start_number 17", "-segment_list pipe:1", "-f mpegts -format_whitelist mpegts") {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, reject := range test.reject {
				if strings.Contains(joined, reject) {
					t.Errorf("unexpected %q", reject)
				}
			}
			if strings.Contains(joined, "://") {
				t.Errorf("command contains a URL")
			}
		})
	}
}

func TestLiveInterlacedDeviceFramesRejected(t *testing.T) {
	params := LiveParams{VideoPID: 256, Interlaced: true, SegmentSeconds: 4, OutputDir: "/work", Rendition: liveRendition(t, "mpeg2video", 720, 576, 1024, 25)}
	config := hwaccel.NewConfigWithMode(domain.AccelCUDA, domain.DecodeDevice)
	if _, err := NewCommandBuilder(config).Live(params); err == nil {
		t.Fatal("expected error for interlaced source on device frames")
	}
	params.Interlaced = false
	if _, err := NewCommandBuilder(config).Live(params); err != nil {
		t.Fatalf("progressive on device frames: %v", err)
	}
}
