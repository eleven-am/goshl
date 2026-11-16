package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/testmedia"
)

func parseProbe(t *testing.T, raw string) probeOutput {
	t.Helper()
	var parsed probeOutput
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestInterpretPicksFirstProgramWithVideo(t *testing.T) {
	parsed := parseProbe(t, `{"programs":[
		{"streams":[{"id":"0x100","codec_type":"audio","codec_name":"mp2","channels":2,"sample_rate":"48000"}]},
		{"streams":[
			{"id":"0x200","codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1280,"height":720,"sample_aspect_ratio":"1:1","avg_frame_rate":"25/1","field_order":"progressive"},
			{"id":"0x201","codec_type":"audio","codec_name":"aac","channels":2,"sample_rate":"48000","tags":{"language":"eng"}},
			{"id":"0x202","codec_type":"audio","codec_name":"ac3","channels":6,"sample_rate":"48000","tags":{"language":"deu"}}]}]}`)
	info, err := interpret(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Programs != 2 || info.Video.PID != 0x200 || info.Video.Codec != "h264" || info.Video.Width != 1280 || info.Video.FrameRate != 25 || info.Video.Interlaced {
		t.Fatalf("info = %+v", info)
	}
	if len(info.Audios) != 2 || info.Audios[0].PID != 0x201 || info.Audios[1].Language != "deu" || info.Audios[1].Channels != 6 {
		t.Fatalf("audios = %+v", info.Audios)
	}
}

func TestInterpretDisplayWidthAndInterlace(t *testing.T) {
	parsed := parseProbe(t, `{"streams":[{"id":"0x100","codec_type":"video","codec_name":"h264","width":720,"height":576,"sample_aspect_ratio":"64:45","avg_frame_rate":"0/0","r_frame_rate":"50/1","field_order":"tt"}]}`)
	info, err := interpret(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Video.DisplayW != 1024 || !info.Video.Interlaced || info.Video.FrameRate != 50 {
		t.Fatalf("video = %+v", info.Video)
	}
}

func TestInterpretDefaultsWhenRatesAndAspectAreMissing(t *testing.T) {
	parsed := parseProbe(t, `{"streams":[{"id":"0x100","codec_type":"video","codec_name":"h264","width":640,"height":360,"sample_aspect_ratio":"N/A","avg_frame_rate":"0/0","r_frame_rate":"0/0"}]}`)
	info, err := interpret(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Video.DisplayW != 640 || info.Video.FrameRate != 25 {
		t.Fatalf("video = %+v", info.Video)
	}
}

func TestInterpretIgnoresStreamsWithoutUsableData(t *testing.T) {
	parsed := parseProbe(t, `{"streams":[
		{"id":"0x100","codec_type":"video","codec_name":"h264","width":0,"height":0},
		{"id":"zz","codec_type":"audio","codec_name":"aac","channels":2,"sample_rate":"48000"},
		{"id":"0x101","codec_type":"audio","codec_name":"aac","channels":0,"sample_rate":"48000"},
		{"id":"0x102","codec_type":"video","codec_name":"h264","width":640,"height":360},
		{"id":"0x103","codec_type":"video","codec_name":"h264","width":1920,"height":1080}]}`)
	info, err := interpret(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Video.PID != 0x102 || len(info.Audios) != 0 {
		t.Fatalf("info = %+v", info)
	}
}

func TestInterpretAudioOnlyIsUnsupported(t *testing.T) {
	parsed := parseProbe(t, `{"streams":[{"id":"0x100","codec_type":"audio","codec_name":"mp2","channels":2,"sample_rate":"48000"}]}`)
	if _, err := interpret(parsed); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("err = %v, want ErrUnsupportedCodec", err)
	}
}

func TestInterpretNothingIsNoData(t *testing.T) {
	if _, err := interpret(parseProbe(t, `{}`)); !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v, want ErrNoData", err)
	}
}

func TestProbeArgsReadFromPipeOnly(t *testing.T) {
	args := probeArgs()
	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	for _, want := range []string{"-f mpegts", "-protocol_whitelist pipe", "-format_whitelist mpegts", "-analyzeduration 2000000", "-probesize 3000000", "-i pipe:0"} {
		if !bytes.Contains([]byte(joined), []byte(want)) {
			t.Fatalf("probe args missing %q: %s", want, joined)
		}
	}
}

func TestTimedCloser(t *testing.T) {
	closed := make(chan struct{})
	tc := closeAfter(closerFunc(func() error { close(closed); return nil }), 20*time.Millisecond)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closer never fired")
	}
	if !tc.expired() || tc.stop() {
		t.Fatal("expected expired closer")
	}
	tc = closeAfter(closerFunc(func() error { t.Error("closed after stop"); return nil }), time.Hour)
	if !tc.stop() || tc.expired() {
		t.Fatal("stop before firing must report true and not expire")
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func probeFixture(t *testing.T, kind testmedia.LiveKind) (ProbeInfo, []byte, error) {
	t.Helper()
	file, err := os.Open(testmedia.Live(t, kind))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	return probeReader(context.Background(), file, time.Now().Add(20*time.Second))
}

func TestProbeReaderFixtures(t *testing.T) {
	cases := []struct {
		kind       testmedia.LiveKind
		video      string
		audio      string
		interlaced bool
		height     int
	}{
		{testmedia.LiveH264AAC, "h264", "aac", false, 720},
		{testmedia.LiveH264MP2, "h264", "mp2", false, 0},
		{testmedia.LiveMPEG2MP2, "mpeg2video", "mp2", true, 0},
		{testmedia.LiveHEVCAC3, "hevc", "ac3", false, 0},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			info, head, err := probeFixture(t, tc.kind)
			if err != nil && !errors.Is(err, ErrSourceUnavailable) {
				t.Fatal(err)
			}
			if info.Video.Codec != tc.video || info.Video.Interlaced != tc.interlaced {
				t.Fatalf("video = %+v", info.Video)
			}
			if tc.height != 0 && info.Video.Height != tc.height {
				t.Fatalf("height = %d", info.Video.Height)
			}
			if len(info.Audios) != 1 || info.Audios[0].Codec != tc.audio {
				t.Fatalf("audios = %+v", info.Audios)
			}
			if info.ProbedBytes == 0 || info.ProbedBytes != len(head) {
				t.Fatalf("probed %d bytes, head %d", info.ProbedBytes, len(head))
			}
		})
	}
}

func TestProbeReaderThenDecideMatchesCodecCases(t *testing.T) {
	cases := []struct {
		kind  testmedia.LiveKind
		video Action
		audio Action
	}{
		{testmedia.LiveH264AAC, ActionCopy, ActionCopy},
		{testmedia.LiveH264MP2, ActionCopy, ActionTranscode},
		{testmedia.LiveMPEG2MP2, ActionTranscode, ActionTranscode},
		{testmedia.LiveHEVCAC3, ActionTranscode, ActionTranscode},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			info, _, err := probeFixture(t, tc.kind)
			if err != nil && !errors.Is(err, ErrSourceUnavailable) {
				t.Fatal(err)
			}
			var opts Options
			opts.setDefaults()
			params, err := decide(&info, opts)
			if err != nil {
				t.Fatal(err)
			}
			if info.VideoAction != tc.video || info.AudioAction != tc.audio || !params.HasAudio {
				t.Fatalf("actions %s/%s, params %+v", info.VideoAction, info.AudioAction, params)
			}
			if tc.video == ActionTranscode && (params.Rendition.Height > 720 || math.Mod(float64(params.Rendition.Width), 2) != 0) {
				t.Fatalf("rendition = %dx%d", params.Rendition.Width, params.Rendition.Height)
			}
		})
	}
}

func TestProbeReaderEmptyIsNoData(t *testing.T) {
	testmedia.Require(t)
	_, _, err := probeReader(context.Background(), bytes.NewReader(nil), time.Now().Add(5*time.Second))
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v, want ErrNoData", err)
	}
}

func TestProbeReaderEmptyWithReadErrorWrapsCause(t *testing.T) {
	testmedia.Require(t)
	cause := errors.New("connection reset")
	_, _, err := probeReader(context.Background(), errReader{cause}, time.Now().Add(5*time.Second))
	if !errors.Is(err, ErrNoData) || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want ErrNoData wrapping cause", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestProbeReaderGarbageIsNoData(t *testing.T) {
	testmedia.Require(t)
	garbage := bytes.Repeat([]byte("this is not an mpeg transport stream "), 4000)
	_, _, err := probeReader(context.Background(), bytes.NewReader(garbage), time.Now().Add(5*time.Second))
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v, want ErrNoData", err)
	}
}

func TestProbeReaderTruncatedByErrorKeepsInfo(t *testing.T) {
	data, err := os.ReadFile(testmedia.Live(t, testmedia.LiveH264AAC))
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("connection reset")
	reader := io.MultiReader(bytes.NewReader(data[:150<<10]), errReader{cause})
	info, head, err := probeReader(context.Background(), reader, time.Now().Add(10*time.Second))
	if !errors.Is(err, ErrSourceUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want ErrSourceUnavailable wrapping cause", err)
	}
	if info.Video.Codec != "h264" || len(head) != 150<<10 {
		t.Fatalf("info %+v head %d", info.Video, len(head))
	}
}

func TestProbeReaderCancelled(t *testing.T) {
	testmedia.Require(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data, err := os.ReadFile(testmedia.Live(t, testmedia.LiveH264AAC))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := probeReader(ctx, bytes.NewReader(data), time.Now().Add(5*time.Second)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
