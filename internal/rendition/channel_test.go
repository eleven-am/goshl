package rendition

import (
	"errors"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func defaultChannelProfile(t *testing.T) domain.ChannelProfile {
	t.Helper()
	profile, err := domain.ChannelProfile{}.Normalised()
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestChannelNameIsStableAndReserved(t *testing.T) {
	profile := defaultChannelProfile(t)
	if got := ChannelName(profile, 0, 10); got != "channel-720p-a0-1a18aabd6d89" {
		t.Fatalf("channel name = %s", got)
	}
	if got := ChannelName(profile, 1, 10); got != "channel-720p-a1-f9b4c6d43e8a" {
		t.Fatalf("second track name = %s", got)
	}
	if got := ChannelName(profile, 0, 4); got != "channel-720p-a0-a063673b5979" {
		t.Fatalf("name at four segments per job = %s", got)
	}
	seen := map[string]string{}
	variants := map[string]domain.ChannelProfile{"default": profile}
	for name, change := range map[string]func(*domain.ChannelProfile){
		"height": func(p *domain.ChannelProfile) { p.Height = 480 },
		"rate":   func(p *domain.ChannelProfile) { p.FrameRateNum, p.FrameRateDen = 25, 1 },
		"frames": func(p *domain.ChannelProfile) { p.SegmentFrames = 96 },
		"video":  func(p *domain.ChannelProfile) { p.VideoBitrate = 3_000_000 },
		"audio":  func(p *domain.ChannelProfile) { p.AudioBitrate = 128_000 },
	} {
		changed := profile
		change(&changed)
		variants[name] = changed
	}
	for label, variant := range variants {
		name := ChannelName(variant, 0, 10)
		if !ValidName(name) || !IsChannelName(name) {
			t.Errorf("%s: %q is not a valid channel name", label, name)
		}
		if other, taken := seen[name]; taken {
			t.Errorf("%s and %s share %q", label, other, name)
		}
		seen[name] = label
	}
	meta := &domain.Metadata{Duration: 60, Video: domain.VideoStream{Codec: "h264", Width: 1920, Height: 1080, FrameRate: 24, Bitrate: 5_000_000}, Audios: []domain.AudioStream{{Index: 1, Codec: "eac3", Channels: 6}, {Index: 2, Codec: "aac", Channels: 2}}}
	videos, _, err := GenerateVideo(meta, 6)
	if err != nil {
		t.Fatal(err)
	}
	for _, video := range videos {
		if IsChannelName(video.Name) {
			t.Errorf("VOD video rendition %q uses the reserved prefix", video.Name)
		}
	}
	for _, audio := range GenerateAudioTracks(meta.Audios) {
		if IsChannelName(audio.Name) {
			t.Errorf("VOD audio rendition %q uses the reserved prefix", audio.Name)
		}
	}
}

func TestChannelFitsThePictureAndAlignsThePhase(t *testing.T) {
	profile := defaultChannelProfile(t)
	cases := []struct {
		name          string
		video         domain.VideoStream
		width, height int
		phase         float64
	}{
		{"16:9", domain.VideoStream{Width: 1920, Height: 1080}, 1280, 720, 0},
		{"4K", domain.VideoStream{Width: 3840, Height: 2160}, 1280, 720, 0},
		{"narrow", domain.VideoStream{Width: 1584, Height: 1080}, 1056, 720, 0},
		{"scope", domain.VideoStream{Width: 1920, Height: 800}, 1280, 534, 0},
		{"standard definition", domain.VideoStream{Width: 512, Height: 384}, 960, 720, 0},
		{"anamorphic", domain.VideoStream{Width: 1920, Height: 1080, SampleAspect: "90:67"}, 1280, 536, 0},
		{"odd fit", domain.VideoStream{Width: 1330, Height: 720}, 1280, 692, 0},
		{"portrait by rotation", domain.VideoStream{Width: 1920, Height: 1080, Rotation: 90}, 406, 720, 0},
		{"square pixels spelled out", domain.VideoStream{Width: 1280, Height: 702, SampleAspect: "1:1"}, 1280, 702, 0},
		{"matroska half a frame late", domain.VideoStream{Width: 1920, Height: 960, Start: 0.021}, 1280, 640, -0.020708},
		{"two frames late on the grid", domain.VideoStream{Width: 1920, Height: 800, Start: 0.083417}, 1280, 534, 0},
		{"a millisecond late", domain.VideoStream{Width: 1920, Height: 1080, Start: 0.001}, 1280, 720, 0.001},
	}
	for _, c := range cases {
		meta := &domain.Metadata{Duration: 60, Video: c.video, Audios: []domain.AudioStream{{Index: 1, Codec: "aac", Channels: 2, SampleRate: 44100, End: 59.5}}}
		video, audio, spec, err := Channel(meta, profile, 0, 10)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if spec.PictureWidth != c.width || spec.PictureHeight != c.height || spec.Width != 1280 || spec.Height != 720 {
			t.Errorf("%s: picture %dx%d in %dx%d, want %dx%d", c.name, spec.PictureWidth, spec.PictureHeight, spec.Width, spec.Height, c.width, c.height)
		}
		if spec.Phase != c.phase {
			t.Errorf("%s: phase %v, want %v", c.name, spec.Phase, c.phase)
		}
		if video.Name != audio.Name || video.Name != ChannelName(profile, 0, 10) || video.Level != 31 || video.Codec != "avc1.64001f" || video.Bitrate != 2_500_000 || video.PeakBitrate != 2_500_000 || video.BufferSize != 5_000_000 || video.Method != domain.Transcode {
			t.Errorf("%s: unexpected video rendition %+v", c.name, video)
		}
		if audio.StreamIndex != 1 || audio.Channels != 2 || audio.SampleRate != 48000 || audio.Bitrate != 160_000 || audio.SourceEnd != 59.5 || audio.CodecString != "mp4a.40.2" {
			t.Errorf("%s: unexpected audio rendition %+v", c.name, audio)
		}
		if spec.TotalFrames != 1439 || spec.SegmentsPerJob != 10 || spec.FormatVersion != domain.ChannelFormatVersion || spec.Silent {
			t.Errorf("%s: unexpected spec %+v", c.name, spec)
		}
	}
}

func TestChannelRejectsUnusableSources(t *testing.T) {
	profile := defaultChannelProfile(t)
	audioOnly := &domain.Metadata{Duration: 60, Audios: []domain.AudioStream{{Index: 0}}}
	if _, _, _, err := Channel(audioOnly, profile, 0, 10); !errors.Is(err, ErrChannelNoVideo) {
		t.Fatalf("audio-only source: %v", err)
	}
	video := &domain.Metadata{Duration: 60, Video: domain.VideoStream{Width: 1920, Height: 1080}, Audios: []domain.AudioStream{{Index: 1}, {Index: 2}}}
	if _, _, _, err := Channel(video, profile, 2, 10); !errors.Is(err, ErrChannelAudioTrack) {
		t.Fatalf("audio track past the end: %v", err)
	}
	if _, audio, _, err := Channel(video, profile, 1, 10); err != nil || audio.StreamIndex != 2 {
		t.Fatalf("second audio track: %+v, %v", audio, err)
	}
	silent := &domain.Metadata{Duration: 60, Video: domain.VideoStream{Width: 1920, Height: 1080}}
	if _, _, spec, err := Channel(silent, profile, 0, 10); err != nil || !spec.Silent {
		t.Fatalf("source without audio: %+v, %v", spec, err)
	}
	if _, _, _, err := Channel(silent, profile, 1, 10); !errors.Is(err, ErrChannelAudioTrack) {
		t.Fatalf("second track of a silent source: %v", err)
	}
}
