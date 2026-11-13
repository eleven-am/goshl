package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl"
	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/rendition"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func firstCueStart(vtt string) (float64, bool) {
	for _, line := range strings.Split(vtt, "\n") {
		start, _, found := strings.Cut(line, " --> ")
		if !found {
			continue
		}
		var seconds float64
		for _, part := range strings.Split(strings.TrimSpace(start), ":") {
			value, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return 0, false
			}
			seconds = seconds*60 + value
		}
		return seconds, true
	}
	return 0, false
}

const startOffset = 2.0

func TestControllerSubtitleCuesFollowVideoOnOffsetSource(t *testing.T) {
	testmedia.Require(t)
	for _, container := range []string{"mp4", "mkv"} {
		t.Run(container, func(t *testing.T) {
			path := testmedia.H264(t, testmedia.Spec{
				Container:      container,
				Width:          320,
				Height:         240,
				FPS:            "24000/1001",
				BFrames:        2,
				KeyframeFrames: []int{0, 48, 96, 144, 192, 240},
				Duration:       10,
				Audio:          []testmedia.AudioSpec{{Codec: "aac"}},
				Subtitles:      true,
				StartOffset:    startOffset,
			})
			start := testmedia.FormatStart(t, path)
			if start < 1.5 {
				t.Fatalf("fixture start_time is %.3f, want a nonzero offset", start)
			}
			var keyframes []float64
			for _, packet := range testmedia.Packets(t, path, "v:0") {
				if packet.Key {
					keyframes = append(keyframes, packet.PTS-start)
				}
			}
			sort.Float64s(keyframes)

			controller := goshl.NewController(goshl.Options{
				Storage:        newTimelineStorage(),
				Coordinator:    newClusterCoordinator(),
				PathGen:        stubPathGen{},
				TargetDuration: 2,
				SegmentsPerJob: 2,
				SegmentTimeout: 2 * time.Minute,
				JobTimeout:     2 * time.Minute,
			})
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if err := controller.Start(ctx); err != nil {
				t.Fatalf("start controller: %v", err)
			}
			defer controller.Stop()

			meta, err := controller.Metadata(context.Background(), path)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			if math.Abs(meta.StartTime-start) > 0.001 {
				t.Fatalf("metadata start time %.3f, want %.3f", meta.StartTime, start)
			}
			if len(meta.Subtitles) == 0 {
				t.Fatalf("metadata lists no subtitle track")
			}
			vtt, err := controller.SubtitleVTT(context.Background(), path, meta.Subtitles[0].ID)
			if err != nil {
				t.Fatalf("subtitle vtt: %v", err)
			}

			offset := outputTimelineOffset.Seconds()
			wantMap := "X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000"
			if !strings.Contains(string(vtt), wantMap) {
				t.Fatalf("subtitle VTT lacks %q:\n%s", wantMap, vtt)
			}
			local, ok := firstCueStart(string(vtt))
			if !ok {
				t.Fatalf("subtitle VTT has no cue:\n%s", vtt)
			}
			cueOutput := local + offset
			cueSource := 1.0 + startOffset - start
			if math.Abs(cueOutput-(cueSource+offset)) > 0.0015 {
				t.Errorf("cue lands at %.3f on the output timeline, the source cue at %.3f belongs at %.3f:\n%s", cueOutput, cueSource, cueSource+offset, vtt)
			}

			segments := playlist.CalculateSegments(keyframes, meta.Duration, 2)
			containing := -1
			for index := range segments {
				if segments[index].Start <= cueSource+0.0015 {
					containing = index
				}
			}
			if containing < 0 {
				t.Fatalf("no planned segment contains source cue at %.3f", cueSource)
			}
			videos, _, err := rendition.GenerateVideo(meta, 2)
			if err != nil || len(videos) == 0 {
				t.Fatalf("generate video renditions: %v (%d renditions)", err, len(videos))
			}
			data, err := controller.Segment(context.Background(), path, goshl.StreamVideo, videos[0].Name, containing)
			if err != nil {
				t.Fatalf("video segment %d: %v", containing, err)
			}
			segment := readTimelineSegment(t, t.TempDir(), "video", containing, "v:0", data)
			if len(segment.pts) == 0 {
				t.Fatalf("video segment %d has no packets", containing)
			}
			first, last := segment.pts[0], segment.pts[len(segment.pts)-1]+timelineVideoFrame
			if cueOutput < first-0.0015 || cueOutput > last+0.0015 {
				t.Errorf("cue at %.3f on the output timeline is outside video segment %d spanning %.3f-%.3f", cueOutput, containing, first, last)
			}
		})
	}
}

type trackPathGen struct{ stubPathGen }

func (trackPathGen) SubtitlePlaylist(sourceURL string, trackID string) string {
	return "/sub/" + trackID + ".m3u8"
}

func pgsDisplaySet(pts uint32, number uint16, show bool) []byte {
	var out bytes.Buffer
	segment := func(kind byte, payload []byte) {
		out.WriteString("PG")
		_ = binary.Write(&out, binary.BigEndian, pts)
		_ = binary.Write(&out, binary.BigEndian, uint32(0))
		out.WriteByte(kind)
		_ = binary.Write(&out, binary.BigEndian, uint16(len(payload)))
		out.Write(payload)
	}
	window := []byte{1, 0, 0, 10, 0, 10, 0, 8, 0, 8}
	if !show {
		segment(0x16, []byte{0, 160, 0, 90, 0x10, byte(number >> 8), byte(number), 0x00, 0, 0, 0})
		segment(0x17, window)
		segment(0x80, nil)
		return out.Bytes()
	}
	segment(0x16, []byte{0, 160, 0, 90, 0x10, byte(number >> 8), byte(number), 0x80, 0, 0, 1, 0, 0, 0, 0, 0, 10, 0, 10})
	segment(0x17, window)
	segment(0x14, []byte{0, 0, 1, 235, 128, 128, 255})
	rle := bytes.Repeat([]byte{0, 0x88, 1, 0, 0}, 8)
	object := []byte{0, 0, 0, 0xC0, 0, 0, byte(len(rle) + 4), 0, 8, 0, 8}
	segment(0x15, append(object, rle...))
	segment(0x80, nil)
	return out.Bytes()
}

func bitmapSubtitleFixture(t *testing.T) string {
	t.Helper()
	testmedia.Require(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "text.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\ntext cue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := filepath.Join(dir, "bitmap.sup")
	var pgs []byte
	pgs = append(pgs, pgsDisplaySet(90000, 0, true)...)
	pgs = append(pgs, pgsDisplaySet(180000, 1, false)...)
	if err := os.WriteFile(sup, pgs, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bitmap.mkv")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=d=6:s=160x90:r=10", "-i", srt, "-i", sup,
		"-map", "0:v", "-map", "1", "-map", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:s:0", "srt", "-c:s:1", "copy", out)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bitmap fixture: %v: %s", err, data)
	}
	return out
}

func TestRealBitmapSubtitleTrackIsNeitherAdvertisedNorServed(t *testing.T) {
	path := bitmapSubtitleFixture(t)
	controller := goshl.NewController(goshl.Options{Storage: newTimelineStorage(), Coordinator: newClusterCoordinator(), PathGen: trackPathGen{}})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer controller.Stop()

	meta, err := controller.Metadata(ctx, path)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if len(meta.Subtitles) != 2 || meta.Subtitles[1].Codec != "hdmv_pgs_subtitle" {
		t.Fatalf("fixture subtitles = %+v", meta.Subtitles)
	}
	text, bitmap := meta.Subtitles[0].ID, meta.Subtitles[1].ID

	master, err := controller.MasterPlaylist(ctx, path)
	if err != nil {
		t.Fatalf("master: %v", err)
	}
	if !strings.Contains(master, "/sub/"+text+".m3u8") || strings.Contains(master, "/sub/"+bitmap+".m3u8") {
		t.Fatalf("master must advertise %s and not %s:\n%s", text, bitmap, master)
	}
	if _, err := controller.SubtitlePlaylist(ctx, path, bitmap); !errors.Is(err, goshl.ErrRenditionNotFound) {
		t.Fatalf("bitmap playlist error = %v, want goshl.ErrRenditionNotFound", err)
	}
	results, err := controller.SubtitlesVTT(ctx, path, []string{text, bitmap})
	if err != nil {
		t.Fatalf("subtitles: %v", err)
	}
	if results[text].Err != nil || !strings.Contains(string(results[text].Data), "text cue") {
		t.Fatalf("text track: %q %v", results[text].Data, results[text].Err)
	}
	if !errors.Is(results[bitmap].Err, goshl.ErrRenditionNotFound) {
		t.Fatalf("bitmap track error = %v, want goshl.ErrRenditionNotFound", results[bitmap].Err)
	}
}
