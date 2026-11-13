package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func fakeFFprobe(t *testing.T, streamsJSON, packets string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "streams.json"), []byte(streamsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "packets.csv"), []byte(packets), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif printf '%s' \"$*\" | grep -q show_format; then cat " + filepath.Join(dir, "streams.json") + "; exit 0; fi\ncat " + filepath.Join(dir, "packets.csv") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func ffprobeField(t *testing.T, path, selector, entries string) string {
	t.Helper()
	args := []string{"-v", "error"}
	if selector != "" {
		args = append(args, "-select_streams", selector)
	}
	args = append(args, "-show_entries", entries, "-of", "default=nw=1:nk=1", path)
	out, err := exec.Command("ffprobe", args...).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", entries, err)
	}
	return strings.TrimSpace(string(out))
}

func measuredEnd(t *testing.T, path, selector string) float64 {
	t.Helper()
	end := math.Inf(-1)
	for _, packet := range testmedia.Packets(t, path, selector) {
		end = math.Max(end, packet.PTS+packet.Duration)
	}
	return end - testmedia.FormatStart(t, path)
}

func TestProbeUsesAverageFrameRateForVariableFrameRate(t *testing.T) {
	testmedia.Require(t)
	path := filepath.Join(t.TempDir(), "vfr.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-t", "6", "-i", "testsrc2=size=320x180:rate=30",
		"-vf", "settb=1/1000,setpts='N*33+mod(N*7,17)'", "-fps_mode", "passthrough", "-enc_time_base", "1/1000",
		"-c:v", "libx264", "-video_track_timescale", "1000", "-movflags", "+faststart", path}
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create vfr fixture: %v: %s", err, data)
	}
	real := parseFrameRate(ffprobeField(t, path, "v:0", "stream=r_frame_rate"))
	average := parseFrameRate(ffprobeField(t, path, "v:0", "stream=avg_frame_rate"))
	if real < average+10 || average < 25 || average > 35 {
		t.Fatalf("fixture is not variable frame rate: r_frame_rate=%v avg_frame_rate=%v", real, average)
	}

	meta, err := NewProber().Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if math.Abs(meta.Video.FrameRate-average) > 0.001 {
		t.Fatalf("FrameRate = %v, want the average rate %v (r_frame_rate is %v)", meta.Video.FrameRate, average, real)
	}
}

func TestProbeFallsBackToRealFrameRateWhenAverageIsUnknown(t *testing.T) {
	fakeFFprobe(t, `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1280,"height":720,"r_frame_rate":"25/1","avg_frame_rate":"0/0"}],"format":{"format_name":"mpegts","start_time":"0.000000","duration":"4.000000"}}`, "0.000000,K_\n")
	meta, err := NewProber().Probe(context.Background(), "/input.ts")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.Video.FrameRate != 25 {
		t.Fatalf("FrameRate = %v, want 25 from r_frame_rate", meta.Video.FrameRate)
	}
}

func TestProbeRecordsFormatBitrate(t *testing.T) {
	testmedia.Require(t)
	path := testmedia.H264(t, testmedia.Spec{Container: "mkv", Duration: 4, Audio: []testmedia.AudioSpec{{Codec: "aac"}}})
	want, err := strconv.Atoi(ffprobeField(t, path, "", "format=bit_rate"))
	if err != nil || want <= 0 {
		t.Fatalf("fixture has no format bit rate: %v", err)
	}
	meta, err := NewProber().Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.Video.Bitrate != 0 {
		t.Fatalf("fixture must lack a video stream bit rate, got %d", meta.Video.Bitrate)
	}
	if meta.Bitrate != want {
		t.Fatalf("Bitrate = %d, want format bit_rate %d", meta.Bitrate, want)
	}
}

func TestProbeReadsLanguageSuffixedStatisticsTags(t *testing.T) {
	fakeFFprobe(t, `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1920,"height":1080,"start_time":"0.000000","tags":{"BPS-eng":"4940302","DURATION-eng":"00:21:01.511000000","_STATISTICS_WRITING_APP-eng":"mkvmerge v55.0.0"}},{"index":1,"codec_name":"aac","codec_type":"audio","channels":2,"start_time":"0.000000","tags":{"BPS-eng":"125588","DURATION-eng":"00:21:01.516000000","_STATISTICS_WRITING_APP-eng":"mkvmerge v55.0.0"}}],"format":{"format_name":"matroska,webm","start_time":"0.000000","duration":"1261.516000","bit_rate":"5068415","tags":{"encoder":"libebml v1.4.2 + libmatroska v1.6.4"}}}`, "0.000000,0.041000,K_\n1261.470000,0.041000,__\n")
	input := filepath.Join(t.TempDir(), "input.mkv")
	if err := os.WriteFile(input, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := NewProber().Probe(context.Background(), input)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.Video.Bitrate != 4940302 || meta.Audios[0].Bitrate != 125588 {
		t.Fatalf("bit rates = %d / %d, want 4940302 / 125588", meta.Video.Bitrate, meta.Audios[0].Bitrate)
	}
	if meta.Audios[0].End != 1261.516 {
		t.Fatalf("audio End = %v, want 1261.516", meta.Audios[0].End)
	}
}

func TestProbeRecordsPerStreamEnd(t *testing.T) {
	testmedia.Require(t)
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			path := testmedia.H264(t, testmedia.Spec{Container: container, Duration: 10, Audio: []testmedia.AudioSpec{{Codec: "aac", Duration: 7.5}}})
			meta, err := NewProber().Probe(context.Background(), path)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			wantVideo := measuredEnd(t, path, "v:0")
			wantAudio := measuredEnd(t, path, "a:0")
			if wantAudio > 7.6 || wantVideo < 9.9 {
				t.Fatalf("fixture streams end at video %v audio %v", wantVideo, wantAudio)
			}
			if math.Abs(meta.Video.End-wantVideo) > 0.045 {
				t.Fatalf("Video.End = %v, want %v", meta.Video.End, wantVideo)
			}
			if len(meta.Audios) != 1 || math.Abs(meta.Audios[0].End-wantAudio) > 0.045 {
				t.Fatalf("Audios = %+v, want End %v", meta.Audios, wantAudio)
			}
		})
	}
}

func TestProbeStreamEndIsMeasuredFromFormatStart(t *testing.T) {
	testmedia.Require(t)
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			path := testmedia.H264(t, testmedia.Spec{Container: container, Duration: 10, StartOffset: 5, Audio: []testmedia.AudioSpec{{Codec: "aac", Delay: 2, Duration: 5.5}}})
			meta, err := NewProber().Probe(context.Background(), path)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if meta.StartTime < 4.9 {
				t.Fatalf("fixture start time = %v, want about 5", meta.StartTime)
			}
			wantVideo := measuredEnd(t, path, "v:0")
			wantAudio := measuredEnd(t, path, "a:0")
			if math.Abs(meta.Video.End-wantVideo) > 0.045 {
				t.Fatalf("Video.End = %v, want %v", meta.Video.End, wantVideo)
			}
			if math.Abs(meta.Audios[0].End-wantAudio) > 0.045 {
				t.Fatalf("Audio End = %v, want %v", meta.Audios[0].End, wantAudio)
			}
		})
	}
}

func TestStreamEndReadsEachDeclaredShape(t *testing.T) {
	statistics := "_STATISTICS_WRITING_APP"
	cases := map[string]struct {
		stream ffprobeStream
		reader endReader
		want   float64
	}{
		"stream duration":                  {ffprobeStream{StartTime: "1.978000", Duration: "5.521333"}, endReader{}, 7.499333},
		"stream duration, late format":     {ffprobeStream{StartTime: "4.978000", Duration: "7.521333"}, endReader{formatStart: 4.978}, 7.521333},
		"duration tag":                     {ffprobeStream{StartTime: "0.024000", Tags: map[string]string{"DURATION": "02:02:36.640000000", statistics: "mkvmerge v102.0"}}, endReader{}, 7356.664},
		"language suffixed duration tag":   {ffprobeStream{StartTime: "0.012000", Tags: map[string]string{"DURATION-eng": "00:21:01.516000000"}}, endReader{}, 1261.528},
		"libavformat end timestamp tag":    {ffprobeStream{StartTime: "1.979000", Tags: map[string]string{"DURATION": "00:00:07.521000000"}}, endReader{timestampTags: true}, 7.521},
		"remux keeps stale statistics":     {ffprobeStream{StartTime: "0.024000", Tags: map[string]string{"DURATION": "00:00:20.024000000", statistics: "mkvmerge v102.0"}}, endReader{timestampTags: true}, 20.024},
		"libavformat ignores stale suffix": {ffprobeStream{StartTime: "0.000000", Tags: map[string]string{"DURATION-eng": "00:21:01.516000000"}}, endReader{timestampTags: true}, 0},
		"libavformat prefers its own tag":  {ffprobeStream{StartTime: "0.000000", Tags: map[string]string{"DURATION-eng": "00:21:01.516000000", "DURATION": "00:00:20.014000000"}}, endReader{timestampTags: true}, 20.014},
		"end timestamp, late format":       {ffprobeStream{StartTime: "5.000000", Tags: map[string]string{"DURATION": "00:00:15.010000000"}}, endReader{formatStart: 4.979, timestampTags: true}, 10.031},
		"nothing declared":                 {ffprobeStream{StartTime: "0.000000"}, endReader{}, 0},
		"nothing declared, negative start": {ffprobeStream{StartTime: "-0.021000"}, endReader{formatStart: -0.021}, 0},
		"no tag, negative start, lavf":     {ffprobeStream{StartTime: "-0.021000"}, endReader{formatStart: -0.021, timestampTags: true}, 0},
		"unparsable":                       {ffprobeStream{StartTime: "0.000000", Duration: "N/A", Tags: map[string]string{"DURATION": "soon"}}, endReader{}, 0},
		"ends before format start":         {ffprobeStream{StartTime: "0.000000", Duration: "1.000000"}, endReader{formatStart: 3}, 0},
	}
	for name, tc := range cases {
		if got := tc.reader.streamEnd(tc.stream); math.Abs(got-tc.want) > 0.000001 {
			t.Errorf("%s: streamEnd = %v, want %v", name, got, tc.want)
		}
	}
	if !writtenByLibavformat(map[string]string{"ENCODER": "Lavf62.3.100"}) || !writtenByLibavformat(map[string]string{"encoder": "Lavf"}) {
		t.Error("libavformat encoder tag not recognised")
	}
	if writtenByLibavformat(map[string]string{"encoder": "libebml v1.4.7 + libmatroska v1.7.2"}) || writtenByLibavformat(nil) {
		t.Error("foreign or absent encoder tag read as libavformat")
	}
}

func TestProbeUsesStreamDurationWhenFormatDurationMissing(t *testing.T) {
	fakeFFprobe(t, `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1280,"height":720,"start_time":"0.500000","duration":"12.000000"},{"index":1,"codec_name":"aac","codec_type":"audio","channels":2,"start_time":"0.500000","duration":"9.000000"}],"format":{"format_name":"mpegts","start_time":"0.500000"}}`, "0.500000,0.040000,K_\n6.500000,0.040000,K_\n12.460000,0.040000,__\n")
	meta, err := NewProber().Probe(context.Background(), "/input.ts")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.Duration != 12 || meta.Audios[0].End != 9 {
		t.Fatalf("Duration = %v, audio End = %v, want 12 and 9", meta.Duration, meta.Audios[0].End)
	}
	if len(meta.Keyframes) != 2 || meta.Keyframes[1] != 6 {
		t.Fatalf("keyframes = %v", meta.Keyframes)
	}
}

func TestProbeDerivesDurationFromPacketScanWhenNothingDeclaresIt(t *testing.T) {
	testmedia.Require(t)
	path := filepath.Join(t.TempDir(), "piped.mkv")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-t", "6", "-i", "testsrc2=size=320x180:rate=25",
		"-f", "lavfi", "-t", "6", "-i", "sine=frequency=440:sample_rate=48000", "-c:v", "libx264", "-g", "25", "-c:a", "aac", "-f", "matroska", "pipe:1")
	data, err := command.Output()
	if err != nil {
		t.Fatalf("create piped fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if declared := ffprobeField(t, path, "", "format=duration"); declared != "N/A" && declared != "" {
		t.Fatalf("fixture declares a format duration %q", declared)
	}
	meta, err := NewProber().Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if want := measuredEnd(t, path, "v:0"); math.Abs(meta.Duration-want) > 0.001 || len(meta.Keyframes) != 6 {
		t.Fatalf("Duration = %v, want %v; keyframes %v", meta.Duration, want, meta.Keyframes)
	}
}

type rangeServer struct {
	*httptest.Server
	served   atomic.Int64
	requests atomic.Int64
	goReads  atomic.Int64
}

type countingResponse struct {
	http.ResponseWriter
	served *atomic.Int64
}

func (w countingResponse) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.served.Add(int64(n))
	return n, err
}

func serveRanges(t *testing.T, path string, dropGoReadsAfter int64) *rangeServer {
	t.Helper()
	server := &rangeServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server.requests.Add(1)
		if strings.HasPrefix(request.UserAgent(), "Go-http-client") && server.goReads.Add(1) > dropGoReadsAfter {
			if hijacker, ok := writer.(http.Hijacker); ok {
				if connection, _, err := hijacker.Hijack(); err == nil {
					connection.Close()
				}
			}
			return
		}
		file, err := os.Open(path)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusNotFound)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		http.ServeContent(countingResponse{writer, &server.served}, request, filepath.Base(path), info.ModTime(), io.ReadSeeker(file))
	}))
	t.Cleanup(server.Close)
	return server
}

func largeMP4(t *testing.T, name string, extra ...string) string {
	t.Helper()
	testmedia.Require(t)
	path := filepath.Join(t.TempDir(), name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-t", "60", "-i", "testsrc2=size=640x360:rate=25,noise=alls=40:allf=t",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-b:v", "4M", "-minrate", "4M", "-maxrate", "4M", "-bufsize", "2M", "-x264-params", "nal-hrd=cbr"}
	args = append(append(args, extra...), path)
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create %s: %v: %s", name, err, data)
	}
	return path
}

func TestProbeIndexesRemoteMP4WithoutFullDownload(t *testing.T) {
	layouts := map[string][]string{"moov first": {"-movflags", "+faststart"}, "moov last": nil}
	for name, extra := range layouts {
		t.Run(name, func(t *testing.T) {
			path := largeMP4(t, "remote.mp4", extra...)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			local, err := NewProber().Probe(context.Background(), path)
			if err != nil {
				t.Fatalf("local probe: %v", err)
			}
			server := serveRanges(t, path, math.MaxInt64)
			remote, err := NewProber().Probe(context.Background(), server.URL+"/remote.mp4")
			if err != nil {
				t.Fatalf("remote probe: %v", err)
			}
			if len(remote.Keyframes) != 30 || fmt.Sprint(remote.Keyframes) != fmt.Sprint(local.Keyframes) {
				t.Fatalf("remote keyframes %v, local %v", remote.Keyframes, local.Keyframes)
			}
			if served, limit := server.served.Load(), info.Size()/5; served > limit {
				t.Fatalf("probe downloaded %d of %d bytes in %d requests, limit %d", served, info.Size(), server.requests.Load(), limit)
			}
			if reads := server.goReads.Load(); reads > 8 {
				t.Fatalf("header index issued %d range requests", reads)
			}
		})
	}
}

func TestProbeFailsWhenHeaderIndexCannotBeRead(t *testing.T) {
	path := largeMP4(t, "offline.mp4", "-movflags", "+faststart")
	server := serveRanges(t, path, 1)
	_, err := NewProber().Probe(context.Background(), server.URL+"/offline.mp4")
	if err == nil {
		t.Fatal("probe hid a failed header read behind the packet scan")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProbeFallsBackToPacketScanWhenRemoteHasNoRangeSupport(t *testing.T) {
	testmedia.Require(t)
	path := makeMedia(t, "plain.mp4", "-movflags", "+faststart")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	}))
	defer server.Close()
	meta, err := NewProber().Probe(context.Background(), server.URL+"/plain.mp4")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(meta.Keyframes) != 6 {
		t.Fatalf("keyframes = %v", meta.Keyframes)
	}
}

func TestKeyframeScanReturnsPromptlyOnReaderError(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nhead -c 70000 /dev/zero | tr '\\0' 'x'\nwhile :; do echo 0.000000,0.040000,K__; done\n"
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	_, err := NewProber().probeKeyframes(ctx, "/input.mkv", 0)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("expected the scanner error, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("scan held the caller for %s after a reader error", elapsed.Round(10*time.Millisecond))
	}
}

func TestProbeDurationNeverExceedsKnownStreamEnds(t *testing.T) {
	testmedia.Require(t)
	for _, container := range []string{"mp4", "mkv", "webm", "ts"} {
		for _, offset := range []float64{0, 5} {
			t.Run(fmt.Sprintf("%s offset %v", container, offset), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "source."+container)
				video, audio := "libx264", "aac"
				if container == "webm" {
					video, audio = "libvpx", "libopus"
				}
				args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-t", "10", "-i", "testsrc2=size=320x180:rate=25",
					"-f", "lavfi", "-t", "7.5", "-i", "sine=frequency=440:sample_rate=48000", "-c:v", video, "-g", "25", "-c:a", audio}
				if offset != 0 {
					args = append(args, "-output_ts_offset", strconv.FormatFloat(offset, 'f', 3, 64))
				}
				if data, err := exec.Command("ffmpeg", append(args, path)...).CombinedOutput(); err != nil {
					t.Fatalf("create fixture: %v: %s", err, data)
				}
				meta, err := NewProber().Probe(context.Background(), path)
				if err != nil {
					t.Fatalf("probe: %v", err)
				}
				content := math.Max(measuredEnd(t, path, "v:0"), measuredEnd(t, path, "a:0"))
				if content < 9.9 || content > 10.2 {
					t.Fatalf("fixture content ends at %v", content)
				}
				if meta.Duration > content+0.25 || meta.Duration < content-0.25 {
					t.Fatalf("Duration = %v, content ends at %v (start %v)", meta.Duration, content, meta.StartTime)
				}
				if last := meta.Keyframes[len(meta.Keyframes)-1]; len(meta.Keyframes) != 10 || last > content {
					t.Fatalf("keyframes %v for content ending at %v", meta.Keyframes, content)
				}
			})
		}
	}
}

func TestBoundedDurationOnlyClampsWhenEveryStreamEndIsKnown(t *testing.T) {
	video := func(end float64) domain.VideoStream { return domain.VideoStream{Codec: "h264", End: end} }
	cases := map[string]struct {
		metadata domain.Metadata
		want     float64
	}{
		"offset included in duration":     {domain.Metadata{Duration: 15.01, Video: video(10.03), Audios: []domain.AudioStream{{End: 7.5}}}, 10.03},
		"audio outlasts video":            {domain.Metadata{Duration: 15.01, Video: video(10.03), Audios: []domain.AudioStream{{End: 12}}}, 12},
		"within slack":                    {domain.Metadata{Duration: 10.2, Video: video(10.03), Audios: []domain.AudioStream{{End: 9.8}}}, 10.2},
		"duration shorter than streams":   {domain.Metadata{Duration: 10.01, Video: video(10.03)}, 10.01},
		"one audio end unknown":           {domain.Metadata{Duration: 15.01, Video: video(10.03), Audios: []domain.AudioStream{{End: 7.5}, {}}}, 15.01},
		"video end unknown":               {domain.Metadata{Duration: 15.01, Video: video(0), Audios: []domain.AudioStream{{End: 7.5}}}, 15.01},
		"audio only":                      {domain.Metadata{Duration: 15.01, Audios: []domain.AudioStream{{End: 7.5}, {End: 9}}}, 9},
		"missing duration, partial ends":  {domain.Metadata{Video: video(10.03), Audios: []domain.AudioStream{{}}}, 10.03},
		"mkvmerge film, staggered tracks": {domain.Metadata{Duration: 7356.664, Video: video(7355.625), Audios: []domain.AudioStream{{End: 7356.664}, {End: 7356.524}, {End: 7355.448}}}, 7356.664},
	}
	for name, tc := range cases {
		if got := boundedDuration(&tc.metadata); got != tc.want {
			t.Errorf("%s: duration = %v, want %v", name, got, tc.want)
		}
	}
}

func TestProbeFailsWhenRemoteHeaderReadIsRefused(t *testing.T) {
	testmedia.Require(t)
	path := makeMedia(t, "refused.mp4", "-movflags", "+faststart")
	var goRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.UserAgent(), "Go-http-client") && goRequests.Add(1) > 1 {
			http.Error(writer, "storage offline", http.StatusServiceUnavailable)
			return
		}
		http.ServeFile(writer, request, path)
	}))
	defer server.Close()
	logPath := ffprobeCallLog(t)
	_, err := NewProber().Probe(context.Background(), server.URL+"/refused.mp4")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("probe error = %v, want the refused header read", err)
	}
	if got := packetPassCount(t, logPath); got != 0 {
		t.Fatalf("packet scan ran %d times after a refused header read", got)
	}
}
