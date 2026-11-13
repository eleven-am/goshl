package mkvindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/testmedia"
)

const subtitleFixtureCues = 60

func subtitleScript(t *testing.T, label string) string {
	t.Helper()
	var script strings.Builder
	for cue := 0; cue < subtitleFixtureCues; cue++ {
		start := 1000 + cue*1900
		fmt.Fprintf(&script, "%d\n%s --> %s\n%s %d\n\n", cue+1, srtClock(start), srtClock(start+1200), label, cue+1)
	}
	path := filepath.Join(t.TempDir(), label+".srt")
	if err := os.WriteFile(path, []byte(script.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func srtClock(milliseconds int) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", milliseconds/3600000, milliseconds/60000%60, milliseconds/1000%60, milliseconds%1000)
}

func subtitledFixture(t *testing.T, extra ...string) string {
	t.Helper()
	args := join(picture(120), tone(120),
		[]string{"-i", subtitleScript(t, "first"), "-i", subtitleScript(t, "second"), "-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3"},
		h264(50), []string{"-c:s:0", "srt", "-c:s:1", "ass"}, extra)
	return encode(t, "subtitled.mkv", args...)
}

func openLayout(t *testing.T, path string) (*SubtitleLayout, *meteredReaderAt, int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := &meteredReaderAt{inner: bytes.NewReader(data), failAfter: math.MaxInt64}
	layout, err := ReadSubtitleLayout(reader, int64(len(data)))
	if err != nil {
		t.Fatalf("read layout: %v", err)
	}
	return layout, reader, int64(len(data))
}

func TestSubtitlePacketsMatchPacketScan(t *testing.T) {
	path := subtitledFixture(t)
	layout, reader, size := openLayout(t, path)
	if len(layout.Tracks) != 2 {
		t.Fatalf("layout lists %d subtitle tracks, want streams 2 and 3", len(layout.Tracks))
	}
	packets, err := ReadSubtitlePackets(context.Background(), reader, size, layout.Tracks)
	if err != nil {
		t.Fatalf("read packets: %v", err)
	}
	for stream, codec := range map[int]string{2: "S_TEXT/UTF8", 3: "S_TEXT/ASS"} {
		track := layout.Tracks[stream]
		if track.CodecID != codec || !track.Complete(layout.MuxingApp) {
			t.Fatalf("stream %d: codec %q, complete %v", stream, track.CodecID, track.Complete(layout.MuxingApp))
		}
		want := testmedia.Packets(t, path, strconv.Itoa(stream))
		got := packets[stream]
		if len(want) != subtitleFixtureCues || len(got) != len(want) {
			t.Fatalf("stream %d: %d packets from cues, %d from the packet scan", stream, len(got), len(want))
		}
		for i := range want {
			scale := float64(layout.TimestampScale) / 1e9
			if math.Abs(float64(got[i].Time)*scale-want[i].PTS) > 0.0005 || math.Abs(float64(got[i].Duration)*scale-want[i].Duration) > 0.0005 {
				t.Fatalf("stream %d packet %d: %d+%d ticks, packet scan %v+%v s", stream, i, got[i].Time, got[i].Duration, want[i].PTS, want[i].Duration)
			}
		}
	}
	if text := string(packets[2][4].Data); text != "first 5" {
		t.Fatalf("fifth subrip payload = %q", text)
	}
	if text := string(packets[3][4].Data); !strings.HasSuffix(text, ",second 5") {
		t.Fatalf("fifth ass payload = %q", text)
	}
	if len(layout.Tracks[3].CodecPrivate) == 0 {
		t.Fatal("ass track lost its codec private header")
	}
}

func TestSubtitlePacketsReadOnlyTheCuedBlocks(t *testing.T) {
	path := subtitledFixture(t)
	layout, reader, size := openLayout(t, path)
	if _, err := ReadSubtitlePackets(context.Background(), reader, size, layout.Tracks); err != nil {
		t.Fatal(err)
	}
	if read, limit := reader.bytes.Load(), size/5; read > limit {
		t.Fatalf("read %d of %d bytes, limit %d", read, size, limit)
	}
	if calls := reader.calls.Load(); calls > 3+2*subtitleFixtureCues {
		t.Fatalf("%d reads for %d cue times", calls, subtitleFixtureCues)
	}
}

func TestSubtitleTrackIsCompleteOnlyWhenEveryPacketIsCued(t *testing.T) {
	lavf := []byte("Lavf")
	other := []byte("Xavf")
	firstCue := []byte{0xF7, 0x81, 0x03, 0xF1}
	absent := []byte{0xF7, 0x81, 0x7E, 0xF1}
	counted := subtitledFixture(t, "-metadata:s:s:0", "NUMBER_OF_FRAMES="+strconv.Itoa(subtitleFixtureCues), "-metadata:s:s:1", "NUMBER_OF_FRAMES-eng="+strconv.Itoa(subtitleFixtureCues+1))
	plain := subtitledFixture(t)

	thinned, err := os.ReadFile(patched(t, counted, lavf, other))
	if err != nil {
		t.Fatal(err)
	}
	cuesAt := bytes.LastIndex(thinned, cuesElement)
	at := bytes.Index(thinned[cuesAt:], firstCue)
	if cuesAt < 0 || at < 0 {
		t.Fatal("fixture has no cue for the first subtitle track")
	}
	copy(thinned[cuesAt+at:], absent)
	thinnedPath := filepath.Join(t.TempDir(), "thinned.mkv")
	if err := os.WriteFile(thinnedPath, thinned, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		path   string
		stream int
		cues   int
		want   bool
	}{
		{"libavformat cues every subtitle packet", plain, 2, subtitleFixtureCues, true},
		{"unknown writer without a frame count", patched(t, plain, lavf, other), 2, subtitleFixtureCues, false},
		{"unknown writer, frame count equals the cues", patched(t, counted, lavf, other), 2, subtitleFixtureCues, true},
		{"unknown writer, language-suffixed frame count is one higher", patched(t, counted, lavf, other), 3, subtitleFixtureCues, false},
		{"unknown writer, one packet not cued", thinnedPath, 2, subtitleFixtureCues - 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			layout, _, _ := openLayout(t, c.path)
			track := layout.Tracks[c.stream]
			if len(track.Cues) != c.cues {
				t.Fatalf("%d cues, want %d", len(track.Cues), c.cues)
			}
			if got := track.Complete(layout.MuxingApp); got != c.want {
				t.Fatalf("complete = %v with %d cues, frame count %d, writer %q", got, len(track.Cues), track.FrameCount, layout.MuxingApp)
			}
		})
	}
}

func TestSubtitlePacketsDropATrackWhoseCueMissesItsBlock(t *testing.T) {
	data, err := os.ReadFile(subtitledFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cuesAt := bytes.LastIndex(data, cuesElement)
	at := cuesAt + bytes.Index(data[cuesAt:], []byte{0xF7, 0x81, 0x03, 0xF1})
	at += 4
	at += 1 + int(data[at]&0x7F)
	if data[at] != 0xF0 {
		t.Fatalf("expected a relative position after the cluster position, found %#x", data[at])
	}
	data[at+1+int(data[at+1]&0x7F)] ^= 0x01

	layout, err := ReadSubtitleLayout(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	packets, err := ReadSubtitlePackets(context.Background(), bytes.NewReader(data), int64(len(data)), layout.Tracks)
	if err != nil {
		t.Fatalf("a misplaced cue must not fail the read: %v", err)
	}
	if _, kept := packets[2]; kept {
		t.Fatal("track with a misplaced cue was returned")
	}
	if len(packets[3]) != subtitleFixtureCues {
		t.Fatalf("healthy track returned %d packets", len(packets[3]))
	}
}

func TestSubtitlePacketsReportReadErrorsAsFailures(t *testing.T) {
	data, err := os.ReadFile(subtitledFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ReadSubtitleLayout(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("storage offline")
	for _, failAfter := range []int64{0, 5, 70} {
		reader := &meteredReaderAt{inner: bytes.NewReader(data), failAfter: failAfter, failure: failure}
		_, err := ReadSubtitlePackets(context.Background(), reader, int64(len(data)), layout.Tracks)
		if !errors.Is(err, failure) || errors.Is(err, ErrUnsupported) {
			t.Fatalf("read failure after %d calls surfaced as %v", failAfter, err)
		}
	}
	reader := &meteredReaderAt{inner: bytes.NewReader(data), failAfter: 1, failure: failure}
	if _, err := ReadSubtitleLayout(reader, int64(len(data))); !errors.Is(err, failure) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("layout read failure surfaced as %v", err)
	}
}

func TestSubtitlePacketsKeepFileOrderWhenCuesAreListedOutOfOrder(t *testing.T) {
	script := filepath.Join(t.TempDir(), "same.srt")
	if err := os.WriteFile(script, []byte("1\n00:00:02,000 --> 00:00:03,000\nupper\n\n2\n00:00:02,000 --> 00:00:03,000\nlower\n\n3\n00:00:05,000 --> 00:00:06,000\nlater\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := encode(t, "same.mkv", join(picture(8), []string{"-i", script, "-map", "0:v", "-map", "1"}, h264(50), []string{"-c:s", "srt"})...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cuesAt := bytes.LastIndex(data, cuesElement)
	marker := []byte{0xF7, 0x81, 0x02, 0xF1}
	relative := func(from int) (int, int) {
		at := from + bytes.Index(data[from:], marker) + len(marker)
		at += 1 + int(data[at]&0x7F)
		if data[at] != 0xF0 {
			t.Fatalf("expected a relative position, found %#x", data[at])
		}
		return at + 2, int(data[at+1] & 0x7F)
	}
	first, firstLength := relative(cuesAt)
	second, secondLength := relative(first)
	if firstLength != secondLength {
		t.Fatalf("relative positions differ in length: %d and %d", firstLength, secondLength)
	}
	swapped := append([]byte{}, data...)
	copy(swapped[first:first+firstLength], data[second:second+secondLength])
	copy(swapped[second:second+secondLength], data[first:first+firstLength])

	layout, err := ReadSubtitleLayout(bytes.NewReader(swapped), int64(len(swapped)))
	if err != nil {
		t.Fatal(err)
	}
	if cues := layout.Tracks[1].Cues; len(cues) != 3 || cues[0].Relative <= cues[1].Relative {
		t.Fatalf("patch did not reverse the first two cues: %+v", cues)
	}
	packets, err := ReadSubtitlePackets(context.Background(), bytes.NewReader(swapped), int64(len(swapped)), layout.Tracks)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, packet := range packets[1] {
		order = append(order, string(packet.Data))
	}
	if strings.Join(order, ",") != "upper,lower,later" {
		t.Fatalf("packets in order %v, the file holds upper, lower, later", order)
	}
}
