package live

import (
	"errors"
	"testing"
	"time"
)

func TestOptionsDefaults(t *testing.T) {
	var opts Options
	opts.setDefaults()
	if opts.SegmentDuration != 4*time.Second || opts.WindowSegments != 6 || opts.IdleTimeout != 60*time.Second || opts.MaxHeight != 720 || opts.ReadySegments != 3 {
		t.Fatalf("defaults = %+v", opts)
	}
	if err := opts.validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestOptionsDefaultsKeepExplicitValues(t *testing.T) {
	opts := Options{SegmentDuration: 2 * time.Second, WindowSegments: 10, IdleTimeout: 5 * time.Second, MaxHeight: 480, ReadySegments: 2}
	opts.setDefaults()
	if opts.SegmentDuration != 2*time.Second || opts.WindowSegments != 10 || opts.IdleTimeout != 5*time.Second || opts.MaxHeight != 480 || opts.ReadySegments != 2 {
		t.Fatalf("explicit values changed: %+v", opts)
	}
}

func TestOptionsValidate(t *testing.T) {
	cases := map[string]func(*Options){
		"segment too short":  func(o *Options) { o.SegmentDuration = 500 * time.Millisecond },
		"segment too long":   func(o *Options) { o.SegmentDuration = 31 * time.Second },
		"window too small":   func(o *Options) { o.WindowSegments = 2 },
		"window too large":   func(o *Options) { o.WindowSegments = 101 },
		"idle too small":     func(o *Options) { o.IdleTimeout = 500 * time.Millisecond },
		"height too small":   func(o *Options) { o.MaxHeight = 143 },
		"ready negative":     func(o *Options) { o.ReadySegments = -1 },
		"ready above window": func(o *Options) { o.ReadySegments = 7 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var opts Options
			opts.setDefaults()
			mutate(&opts)
			if err := opts.validate(); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("validate() = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestOptionsValidateBoundaries(t *testing.T) {
	opts := Options{SegmentDuration: time.Second, WindowSegments: 3, IdleTimeout: time.Second, MaxHeight: 144, ReadySegments: 3}
	if err := opts.validate(); err != nil {
		t.Fatalf("lower bounds: %v", err)
	}
	opts = Options{SegmentDuration: 30 * time.Second, WindowSegments: 100, IdleTimeout: time.Second, MaxHeight: 144, ReadySegments: 100}
	if err := opts.validate(); err != nil {
		t.Fatalf("upper bounds: %v", err)
	}
}

func h264Info(pixfmt string) ProbeInfo {
	return ProbeInfo{
		Video:         VideoInfo{PID: 256, Codec: "h264", PixelFormat: pixfmt, Width: 1280, Height: 720, DisplayW: 1280, FrameRate: 25},
		Audios:        []AudioInfo{{PID: 257, Codec: "aac", Language: "eng", Channels: 2, SampleRate: 48000}},
		SelectedAudio: -1,
	}
}

func TestDecideCopiesH264AndAAC(t *testing.T) {
	info := h264Info("yuv420p")
	var opts Options
	opts.setDefaults()
	params, err := decide(&info, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !params.CopyVideo || !params.CopyAudio || !params.HasAudio || params.VideoPID != 256 || params.AudioPID != 257 {
		t.Fatalf("params = %+v", params)
	}
	if info.VideoAction != ActionCopy || info.AudioAction != ActionCopy || info.SelectedAudio != 0 {
		t.Fatalf("info = %+v", info)
	}
	if params.SegmentSeconds != 4 {
		t.Fatalf("segment seconds = %v", params.SegmentSeconds)
	}
}

func TestDecideTranscodesNonCopyableVideo(t *testing.T) {
	var opts Options
	opts.setDefaults()
	cases := map[string]ProbeInfo{
		"h264 10 bit": h264Info("yuv420p10le"),
		"h264 422":    h264Info("yuv422p"),
	}
	hevc := h264Info("yuv420p")
	hevc.Video.Codec = "hevc"
	hevc.Video.Width, hevc.Video.Height, hevc.Video.DisplayW = 1920, 1080, 1920
	cases["hevc"] = hevc
	mpeg2 := h264Info("yuv420p")
	mpeg2.Video.Codec = "mpeg2video"
	mpeg2.Video.Interlaced = true
	cases["mpeg2"] = mpeg2
	for name, info := range cases {
		t.Run(name, func(t *testing.T) {
			params, err := decide(&info, opts)
			if err != nil {
				t.Fatal(err)
			}
			if params.CopyVideo || info.VideoAction != ActionTranscode {
				t.Fatalf("expected transcode, params %+v info %+v", params, info)
			}
			if params.Rendition.Height > 720 || params.Rendition.Height%2 != 0 || params.Rendition.Width%2 != 0 {
				t.Fatalf("rendition = %dx%d", params.Rendition.Width, params.Rendition.Height)
			}
		})
	}
}

func TestDecideCapsHeightAtMaxHeight(t *testing.T) {
	info := h264Info("yuv420p")
	info.Video.Codec = "hevc"
	info.Video.Width, info.Video.Height, info.Video.DisplayW = 1920, 1080, 1920
	opts := Options{MaxHeight: 360}
	opts.setDefaults()
	params, err := decide(&info, opts)
	if err != nil {
		t.Fatal(err)
	}
	if params.Rendition.Height != 360 || params.Rendition.Width != 640 {
		t.Fatalf("rendition = %dx%d", params.Rendition.Width, params.Rendition.Height)
	}
}

func TestDecideInterlacedFlagPropagates(t *testing.T) {
	info := h264Info("yuv420p")
	info.Video.Codec = "mpeg2video"
	info.Video.Interlaced = true
	var opts Options
	opts.setDefaults()
	params, err := decide(&info, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !params.Interlaced {
		t.Fatal("interlaced flag not propagated")
	}
}

func TestDecideInterlacedH264(t *testing.T) {
	cases := []struct {
		name        string
		interlaced  bool
		option      bool
		wantTransco bool
	}{
		{"interlaced option on", true, true, true},
		{"interlaced option off", true, false, false},
		{"progressive option on", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := h264Info("yuv420p")
			info.Video.Width, info.Video.Height, info.Video.DisplayW = 1920, 1080, 1920
			info.Video.Interlaced = tc.interlaced
			opts := Options{TranscodeInterlaced: tc.option}
			opts.setDefaults()
			params, err := decide(&info, opts)
			if err != nil {
				t.Fatal(err)
			}
			if params.CopyVideo == tc.wantTransco {
				t.Fatalf("CopyVideo = %v, want %v", params.CopyVideo, !tc.wantTransco)
			}
			if tc.wantTransco {
				if info.VideoAction != ActionTranscode || !params.Interlaced || params.Rendition.Height != 720 {
					t.Fatalf("params %+v info %+v", params, info)
				}
			} else if info.VideoAction != ActionCopy {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

func TestDecideRejectsUnsupportedVideo(t *testing.T) {
	info := h264Info("yuv420p")
	info.Video.Codec = "vp9"
	var opts Options
	opts.setDefaults()
	if _, err := decide(&info, opts); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("err = %v, want ErrUnsupportedCodec", err)
	}
}

func TestDecideTranscodesNonAACAudio(t *testing.T) {
	var opts Options
	opts.setDefaults()
	for _, codec := range []string{"mp2", "mp3", "ac3", "eac3", "aac_latm"} {
		t.Run(codec, func(t *testing.T) {
			info := h264Info("yuv420p")
			info.Audios[0].Codec = codec
			params, err := decide(&info, opts)
			if err != nil {
				t.Fatal(err)
			}
			if params.CopyAudio || !params.HasAudio || info.AudioAction != ActionTranscode {
				t.Fatalf("params %+v info %+v", params, info)
			}
		})
	}
}

func TestDecideWithoutDecodableAudio(t *testing.T) {
	var opts Options
	opts.setDefaults()
	none := h264Info("yuv420p")
	none.Audios = nil
	odd := h264Info("yuv420p")
	odd.Audios[0].Codec = "opus"
	for name, info := range map[string]ProbeInfo{"none": none, "undecodable": odd} {
		t.Run(name, func(t *testing.T) {
			params, err := decide(&info, opts)
			if err != nil {
				t.Fatal(err)
			}
			if params.HasAudio || info.AudioAction != ActionNone || info.SelectedAudio != -1 {
				t.Fatalf("params %+v info %+v", params, info)
			}
		})
	}
}

func TestSelectAudioPrefersLanguage(t *testing.T) {
	audios := []AudioInfo{
		{PID: 1, Codec: "opus", Language: "fra"},
		{PID: 2, Codec: "aac", Language: "eng"},
		{PID: 3, Codec: "ac3", Language: "DEU"},
	}
	if got := selectAudio(audios, ""); got != 1 {
		t.Fatalf("no preference = %d, want first decodable (1)", got)
	}
	if got := selectAudio(audios, "deu"); got != 2 {
		t.Fatalf("case-insensitive match = %d, want 2", got)
	}
	if got := selectAudio(audios, "fra"); got != 1 {
		t.Fatalf("undecodable preferred language = %d, want fallback 1", got)
	}
	if got := selectAudio(audios, "spa"); got != 1 {
		t.Fatalf("missing language = %d, want fallback 1", got)
	}
	if got := selectAudio(nil, "eng"); got != -1 {
		t.Fatalf("empty = %d, want -1", got)
	}
}
