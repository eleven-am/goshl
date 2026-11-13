package mp4index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func bFrameFixture(t *testing.T) []byte {
	t.Helper()
	requireTools(t)
	path := filepath.Join(t.TempDir(), "bframes.mp4")
	runFFmpeg(t, append(video("-t", "4", "-c:v", "libx264", "-g", "25", "-bf", "2", "-movflags", "+faststart"), path)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := KeyframesFrom(bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	return data
}

func TestKeyframesDeclineSampleCountLargerThanSource(t *testing.T) {
	file, err := mp4.DecodeFile(bytes.NewReader(bFrameFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	const claimed = 50_000_000
	trak := file.Moov.Traks[0]
	stbl := trak.Mdia.Minf.Stbl
	keep := func(children []mp4.Box, drop ...string) []mp4.Box {
		kept := children[:0]
		for _, child := range children {
			dropped := false
			for _, name := range drop {
				dropped = dropped || child.Type() == name
			}
			if !dropped {
				kept = append(kept, child)
			}
		}
		return kept
	}
	stbl.Children = keep(stbl.Children, "stss", "ctts")
	stbl.Stss, stbl.Ctts = nil, nil
	trak.Children = keep(trak.Children, "edts")
	trak.Edts = nil
	stbl.Stsz.SampleUniformSize = 1
	stbl.Stsz.SampleNumber = claimed
	stbl.Stsz.SampleSize = nil
	stbl.Stts.SampleCount = []uint32{claimed}
	stbl.Stts.SampleTimeDelta = []uint32{1}
	var encoded bytes.Buffer
	if err := file.Encode(&encoded); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = KeyframesFrom(bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), 0)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("a %d byte source claiming %d samples: got err=%v, want ErrUnsupported", encoded.Len(), claimed, err)
	}
	if allocated := (after.TotalAlloc - before.TotalAlloc) >> 20; allocated > 16 {
		t.Errorf("indexing a %d byte source allocated %d MiB", encoded.Len(), allocated)
	}
}

func TestKeyframesDeclineEditThatStartsBeforeFirstCompositionTime(t *testing.T) {
	data := bFrameFixture(t)
	elst := bytes.Index(data, []byte("elst"))
	if elst < 0 {
		t.Fatal("fixture has no edit list")
	}
	mediaTime := data[elst+16 : elst+20]
	if binary.BigEndian.Uint32(mediaTime) == 0 {
		t.Fatal("fixture edit already starts at zero")
	}
	binary.BigEndian.PutUint32(mediaTime, 0)
	path := filepath.Join(t.TempDir(), "edit_at_zero.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	want := ffprobeKeyframes(t, path, 0)
	got, err := KeyframesFrom(bytes.NewReader(data), int64(len(data)), 0)
	if err == nil {
		if len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("index disagrees with ffprobe: got %v want %v", got, want)
		}
		return
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

func TestKeyframesDeclineTruncatedFastStartSource(t *testing.T) {
	data := bFrameFixture(t)
	truncated := data[:len(data)*6/10]
	if bytes.Index(truncated, []byte("mdat")) < 0 {
		t.Fatal("truncation removed the media data header")
	}
	_, err := KeyframesFrom(bytes.NewReader(truncated), int64(len(truncated)), 0)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("source cut to %d of %d bytes: got err=%v, want ErrUnsupported", len(truncated), len(data), err)
	}
}
