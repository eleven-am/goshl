package mp4index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestMP4IndexTreatsMissingStssAsAllSync(t *testing.T) {
	requireTools(t)
	path := filepath.Join(t.TempDir(), "all_intra.mp4")
	runFFmpeg(t, append(video("-t", "4", "-c:v", "libx264", "-g", "1", "-movflags", "+faststart"), path)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("stss")) {
		t.Fatal("fixture carries a sync sample table")
	}
	if got := assertSame(t, path, 0); len(got) != 100 {
		t.Fatalf("expected every one of 100 samples to be sync, got %d", len(got))
	}
}

func TestMP4IndexAcceptsSizeZeroLastBox(t *testing.T) {
	requireTools(t)
	path := filepath.Join(t.TempDir(), "moov_last.mp4")
	runFFmpeg(t, append(video("-t", "4", "-c:v", "libx264", "-g", "25", "-bf", "2"), path)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := KeyframesFrom(bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	moov := bytes.LastIndex(data, []byte("moov")) - 4
	if moov < 0 || int(binary.BigEndian.Uint32(data[moov:]))+moov != len(data) {
		t.Fatal("fixture does not end with the moov box")
	}
	binary.BigEndian.PutUint32(data[moov:], 0)
	patched := filepath.Join(t.TempDir(), "moov_to_eof.mp4")
	if err := os.WriteFile(patched, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got := assertSame(t, patched, 0)
	if len(got) != len(want) {
		t.Fatalf("keyframes %v, want %v", got, want)
	}
}

func TestMP4IndexSkipsLargeSizeBoxBeforeMoov(t *testing.T) {
	paths := makeFixtures(t)
	data, err := os.ReadFile(paths["faststart"])
	if err != nil {
		t.Fatal(err)
	}
	want, err := KeyframesFrom(bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	free := make([]byte, 32)
	binary.BigEndian.PutUint32(free[0:4], 1)
	copy(free[4:8], "free")
	binary.BigEndian.PutUint64(free[8:16], uint64(len(free)))
	head := ftypSize(data)
	padded := append(append(append([]byte{}, data[:head]...), free...), data[head:]...)
	got, err := KeyframesFrom(bytes.NewReader(padded), int64(len(padded)), 0)
	if err != nil {
		t.Fatalf("large-size free box before moov: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("keyframes %v, want %v", got, want)
	}
}

type failingReaderAt struct {
	inner     io.ReaderAt
	failAfter int64
	calls     atomic.Int64
	failure   error
}

func (r *failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.calls.Add(1) > r.failAfter {
		return 0, r.failure
	}
	return r.inner.ReadAt(p, off)
}

func TestKeyframesFromReportsReadErrorsAsFailures(t *testing.T) {
	paths := makeFixtures(t)
	data, err := os.ReadFile(paths["moov_at_end"])
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("storage offline")
	for _, failAfter := range []int64{0, 1, 2, 3} {
		reader := &failingReaderAt{inner: bytes.NewReader(data), failAfter: failAfter, failure: failure}
		_, err := KeyframesFrom(reader, int64(len(data)), 0)
		if !errors.Is(err, failure) || errors.Is(err, ErrUnsupported) {
			t.Fatalf("read failure after %d calls surfaced as %v", failAfter, err)
		}
	}
}

func TestKeyframesFromReadsEachBoxHeaderAndTheMoovOnce(t *testing.T) {
	paths := makeFixtures(t)
	for _, key := range []string{"faststart", "moov_at_end", "bframes"} {
		data, err := os.ReadFile(paths[key])
		if err != nil {
			t.Fatal(err)
		}
		reader := &failingReaderAt{inner: bytes.NewReader(data), failAfter: 1 << 30}
		if _, err := KeyframesFrom(reader, int64(len(data)), 0); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if calls := reader.calls.Load(); calls > 5 {
			t.Fatalf("%s: %d reads for a file with at most four top-level boxes", key, calls)
		}
	}
}
