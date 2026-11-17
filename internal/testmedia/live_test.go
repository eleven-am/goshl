package testmedia

import (
	"encoding/json"
	"os/exec"
	"testing"
)

type liveStream struct {
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	FieldOrder string `json:"field_order"`
	Channels   int    `json:"channels"`
}

func probeLive(t *testing.T, path string) []liveStream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-read_intervals", "%+2", "-show_entries", "stream=codec_type,codec_name,field_order,channels", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	var parsed struct {
		Streams []liveStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("decode probe of %s: %v", path, err)
	}
	return parsed.Streams
}

func TestLiveFixtures(t *testing.T) {
	Require(t)
	cases := []struct {
		kind       LiveKind
		video      string
		audio      string
		interlaced bool
		channels   int
	}{
		{LiveH264AAC, "h264", "aac", false, 2},
		{LiveH264MP2, "h264", "mp2", false, 2},
		{LiveMPEG2MP2, "mpeg2video", "mp2", true, 2},
		{LiveHEVCAC3, "hevc", "ac3", false, 6},
		{LiveH264IAAC, "h264", "aac", true, 2},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			path := Live(t, tc.kind)
			if again := Live(t, tc.kind); again != path {
				t.Fatalf("cache returned %s then %s", path, again)
			}
			var video, audio *liveStream
			streams := probeLive(t, path)
			for index := range streams {
				switch streams[index].CodecType {
				case "video":
					video = &streams[index]
				case "audio":
					audio = &streams[index]
				}
			}
			if video == nil || audio == nil {
				t.Fatalf("missing stream: %+v", streams)
			}
			if video.CodecName != tc.video || audio.CodecName != tc.audio {
				t.Fatalf("codecs %s+%s, want %s+%s", video.CodecName, audio.CodecName, tc.video, tc.audio)
			}
			if audio.Channels != tc.channels {
				t.Fatalf("channels %d, want %d", audio.Channels, tc.channels)
			}
			interlaced := video.FieldOrder == "tt" || video.FieldOrder == "bb" || video.FieldOrder == "tb" || video.FieldOrder == "bt"
			if interlaced != tc.interlaced {
				t.Fatalf("field order %q, interlaced want %v", video.FieldOrder, tc.interlaced)
			}
		})
	}
}

func TestLiveUnknownKind(t *testing.T) {
	if _, ok := liveArgs("nope"); ok {
		t.Fatal("unknown kind accepted")
	}
}
