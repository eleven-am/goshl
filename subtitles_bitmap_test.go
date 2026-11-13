package goshl

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/testmedia"
)

type trackPathGen struct{ stubPathGen }

func (trackPathGen) SubtitlePlaylist(sourceURL string, trackID string) string {
	return "/sub/" + trackID + ".m3u8"
}

func mixedSubtitleController(t *testing.T) *Controller {
	t.Helper()
	meta := &domain.Metadata{
		Duration: 12,
		Video:    domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Audios:   []domain.AudioStream{{Codec: "aac", Channels: 2, Bitrate: 128_000, Language: "eng"}},
		Subtitles: []domain.SubtitleStream{
			{Index: 2, ID: "subtitle-2", Codec: "subrip", Language: "eng"},
			{Index: 3, ID: "subtitle-3", Codec: "hdmv_pgs_subtitle", Language: "eng"},
			{Index: 4, ID: "subtitle-4", Codec: "dvd_subtitle", Language: "fra"},
			{Index: 5, ID: "subtitle-5", Codec: "dvb_subtitle", Language: "deu"},
			{Index: 6, ID: "subtitle-6", Codec: "ttml", Language: "spa"},
			{Index: 7, ID: "subtitle-7", Codec: "ass", Language: "jpn"},
		},
	}
	metaBytes, _ := json.Marshal(meta)
	return NewController(Options{
		Storage:     &keyedSubtitleStorage{stubStorage: &stubStorage{metaData: metaBytes, metaExists: true}},
		Coordinator: &stubCoordinator{},
		PathGen:     trackPathGen{},
	})
}

func TestMasterOmitsBitmapSubtitleTracks(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	svc := mixedSubtitleController(t)

	master, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master: %v", err)
	}
	for _, id := range []string{"subtitle-2", "subtitle-7"} {
		if !strings.Contains(master, "/sub/"+id+".m3u8") {
			t.Fatalf("text track %s missing from master:\n%s", id, master)
		}
	}
	for _, id := range []string{"subtitle-3", "subtitle-4", "subtitle-5", "subtitle-6"} {
		if strings.Contains(master, "/sub/"+id+".m3u8") {
			t.Fatalf("unconvertible track %s advertised in master:\n%s", id, master)
		}
	}
	if got := strings.Count(master, "TYPE=SUBTITLES"); got != 2 {
		t.Fatalf("master has %d subtitle renditions, want 2:\n%s", got, master)
	}
}

func TestMasterWithOnlyBitmapSubtitlesHasNoSubtitleGroup(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	meta := &domain.Metadata{
		Video:     domain.VideoStream{Width: 1920, Height: 1080, Bitrate: 5_000_000},
		Audios:    []domain.AudioStream{{Codec: "aac", Channels: 2, Bitrate: 128_000, Language: "eng"}},
		Subtitles: []domain.SubtitleStream{{Index: 3, ID: "subtitle-3", Codec: "hdmv_pgs_subtitle", Language: "eng"}},
	}
	metaBytes, _ := json.Marshal(meta)
	svc := NewController(Options{Storage: &stubStorage{metaData: metaBytes, metaExists: true}, Coordinator: &stubCoordinator{}, PathGen: trackPathGen{}})

	master, err := svc.MasterPlaylist(context.Background(), "file:///media")
	if err != nil {
		t.Fatalf("master: %v", err)
	}
	if strings.Contains(master, "SUBTITLES") {
		t.Fatalf("master references a subtitle group with no convertible track:\n%s", master)
	}
}

func TestSubtitlePlaylistRejectsBitmapTrack(t *testing.T) {
	cleanup := installFakeFFmpeg(t)
	defer cleanup()
	svc := mixedSubtitleController(t)

	if _, err := svc.SubtitlePlaylist(context.Background(), "file:///media", "subtitle-2"); err != nil {
		t.Fatalf("text track playlist: %v", err)
	}
	for _, id := range []string{"subtitle-3", "subtitle-4", "subtitle-5", "subtitle-6", "missing"} {
		if _, err := svc.SubtitlePlaylist(context.Background(), "file:///media", id); !errors.Is(err, ErrRenditionNotFound) {
			t.Fatalf("track %s: error = %v, want ErrRenditionNotFound", id, err)
		}
	}
}

func TestSubtitlesVTTRejectsUnconvertibleTracksAsRenditionNotFound(t *testing.T) {
	logPath, _ := installSubtitleFFmpeg(t)
	svc := mixedSubtitleController(t)

	got, err := svc.SubtitlesVTT(context.Background(), nonMP4Source(t), []string{"subtitle-2", "subtitle-3", "subtitle-6", "missing"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got["subtitle-2"].Err != nil || len(got["subtitle-2"].Data) == 0 {
		t.Fatalf("text track: %+v", got["subtitle-2"])
	}
	for _, id := range []string{"subtitle-3", "subtitle-6", "missing"} {
		if !errors.Is(got[id].Err, ErrRenditionNotFound) {
			t.Fatalf("track %s: error = %v, want ErrRenditionNotFound", id, got[id].Err)
		}
	}
	if calls := ffmpegCalls(t, logPath); len(calls) != 1 || strings.Contains(calls[0], "0:3") || strings.Contains(calls[0], "0:6") {
		t.Fatalf("unconvertible tracks reached ffmpeg: %v", calls)
	}
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
	controller := NewController(Options{Storage: newTimelineStorage(), Coordinator: newClusterCoordinator(), PathGen: trackPathGen{}})
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
	if _, err := controller.SubtitlePlaylist(ctx, path, bitmap); !errors.Is(err, ErrRenditionNotFound) {
		t.Fatalf("bitmap playlist error = %v, want ErrRenditionNotFound", err)
	}
	results, err := controller.SubtitlesVTT(ctx, path, []string{text, bitmap})
	if err != nil {
		t.Fatalf("subtitles: %v", err)
	}
	if results[text].Err != nil || !strings.Contains(string(results[text].Data), "text cue") {
		t.Fatalf("text track: %q %v", results[text].Data, results[text].Err)
	}
	if !errors.Is(results[bitmap].Err, ErrRenditionNotFound) {
		t.Fatalf("bitmap track error = %v, want ErrRenditionNotFound", results[bitmap].Err)
	}
}
