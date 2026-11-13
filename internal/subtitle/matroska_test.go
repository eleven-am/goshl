package subtitle

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eleven-am/goshl/internal/testmedia"
)

func compactFixture(t *testing.T, extra ...string) string {
	t.Helper()
	testmedia.Require(t)
	dir := t.TempDir()
	var script strings.Builder
	for cue := 0; cue < 40; cue++ {
		fmt.Fprintf(&script, "%d\n00:00:%02d,250 --> 00:00:%02d,900\nline %d\n\n", cue+1, cue, cue, cue+1)
	}
	srt := filepath.Join(dir, "a.srt")
	if err := os.WriteFile(srt, []byte(script.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "source.mkv")
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-t", "42", "-i", "testsrc2=size=320x180:rate=25", "-i", srt, "-i", srt, "-i", srt,
		"-map", "0:v", "-map", "1", "-map", "2", "-map", "3", "-c:v", "libx264", "-preset", "ultrafast", "-c:s:0", "srt", "-c:s:1", "ass", "-c:s:2", "webvtt"}
	if data, err := exec.Command("ffmpeg", append(append(args, extra...), out)...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, data)
	}
	return out
}

func TestCompactMatroskaCarriesTheSamePackets(t *testing.T) {
	for _, offset := range []string{"0", "7.3"} {
		t.Run("offset "+offset, func(t *testing.T) {
			source := compactFixture(t, "-output_ts_offset", offset)
			tracks := []Track{{ID: "ass", StreamIndex: 2, Codec: "ass"}, {ID: "vtt", StreamIndex: 3, Codec: "webvtt"}, {ID: "srt", StreamIndex: 1, Codec: "subrip"}, {ID: "video", StreamIndex: 0, Codec: "subrip"}}
			compact, err := CompactMatroska(context.Background(), source, tracks, t.TempDir())
			if err != nil {
				t.Fatalf("compact: %v", err)
			}
			if len(compact.Streams) != 2 || compact.Streams["ass"] != 0 || compact.Streams["srt"] != 1 {
				t.Fatalf("streams = %v, want ass at 0 and srt at 1 only", compact.Streams)
			}
			for id, original := range map[string]int{"ass": 2, "srt": 1} {
				want := testmedia.Packets(t, source, strconv.Itoa(original))
				got := testmedia.Packets(t, compact.Path, strconv.Itoa(compact.Streams[id]))
				if len(want) != 40 || len(got) != len(want) {
					t.Fatalf("%s: %d packets in the compact file, %d in the source", id, len(got), len(want))
				}
				for i := range want {
					if math.Abs(got[i].PTS-want[i].PTS) > 0.0005 || math.Abs(got[i].Duration-want[i].Duration) > 0.0005 {
						t.Fatalf("%s packet %d: %v+%v, source has %v+%v", id, i, got[i].PTS, got[i].Duration, want[i].PTS, want[i].Duration)
					}
				}
			}
			sourceInfo, _ := os.Stat(source)
			compactInfo, _ := os.Stat(compact.Path)
			if compactInfo.Size()*20 > sourceInfo.Size() {
				t.Fatalf("compact file is %d bytes for a %d byte source", compactInfo.Size(), sourceInfo.Size())
			}
		})
	}
}

func TestCompactMatroskaRejectsSourcesWithoutIndexedSubtitles(t *testing.T) {
	testmedia.Require(t)
	mp4 := filepath.Join(t.TempDir(), "source.mp4")
	if data, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-t", "2", "-i", "testsrc2=size=160x90:rate=10", "-c:v", "libx264", mp4).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, data)
	}
	if _, err := CompactMatroska(context.Background(), mp4, []Track{{ID: "a", StreamIndex: 1, Codec: "subrip"}}, t.TempDir()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("mp4 source returned %v, want ErrUnsupported", err)
	}
	mkv := compactFixture(t)
	if _, err := CompactMatroska(context.Background(), mkv, []Track{{ID: "vtt", StreamIndex: 3, Codec: "webvtt"}}, t.TempDir()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("webvtt-only request returned %v, want ErrUnsupported", err)
	}
	if _, err := CompactMatroska(context.Background(), filepath.Join(t.TempDir(), "missing.mkv"), []Track{{ID: "a", StreamIndex: 1, Codec: "subrip"}}, t.TempDir()); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing source returned %v, want a failure", err)
	}
}
