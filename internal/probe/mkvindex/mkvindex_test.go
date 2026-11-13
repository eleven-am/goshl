package mkvindex

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/eleven-am/goshl/internal/playlist"
	"github.com/eleven-am/goshl/internal/testmedia"
)

var (
	timestampScaleMillisecond = []byte{0x2A, 0xD7, 0xB1, 0x83, 0x0F, 0x42, 0x40}
	timestampScale400Micro    = []byte{0x2A, 0xD7, 0xB1, 0x83, 0x06, 0x1A, 0x80}
	languageUndetermined      = []byte{0x22, 0xB5, 0x9C, 0x83, 0x75, 0x6E, 0x64}
	codecDelay80Millisecond   = []byte{0x56, 0xAA, 0x84, 0x04, 0xC4, 0xB4, 0x00}
	segmentDuration           = []byte{0x44, 0x89, 0x88}
	mislabelledDuration       = []byte{0x4D, 0x80, 0x88}
	cuesElement               = []byte{0x1C, 0x53, 0xBB, 0x6B}
	cueOnTrackOne             = []byte{0xF7, 0x81, 0x01, 0xF1}
	cueOnAbsentTrack          = []byte{0xF7, 0x81, 0x7E, 0xF1}
)

func encode(t *testing.T, name string, args ...string) string {
	t.Helper()
	testmedia.Require(t)
	path := filepath.Join(t.TempDir(), name)
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if data, err := exec.Command("ffmpeg", append(full, path)...).CombinedOutput(); err != nil {
		t.Fatalf("create %s: %v: %s", name, err, data)
	}
	return path
}

func picture(seconds int) []string {
	return []string{"-f", "lavfi", "-t", strconv.Itoa(seconds), "-i", "testsrc2=size=320x180:rate=25"}
}

func tone(seconds int) []string {
	return []string{"-f", "lavfi", "-t", strconv.Itoa(seconds), "-i", "sine=frequency=440:sample_rate=48000"}
}

func h264(gop int) []string {
	return []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", strconv.Itoa(gop), "-sc_threshold", "0", "-bf", "2", "-c:a", "aac"}
}

func join(parts ...[]string) []string {
	var all []string
	for _, part := range parts {
		all = append(all, part...)
	}
	return all
}

func scannedKeyframes(t *testing.T, path string, streamIndex int) []float64 {
	t.Helper()
	var keyframes []float64
	for _, packet := range testmedia.Packets(t, path, strconv.Itoa(streamIndex)) {
		if packet.Key {
			keyframes = append(keyframes, packet.PTS)
		}
	}
	if len(keyframes) == 0 {
		t.Fatalf("packet scan of %s stream %d found no keyframes", path, streamIndex)
	}
	return keyframes
}

func requireScanMatch(t *testing.T, path string, streamIndex int) []float64 {
	t.Helper()
	got, err := Keyframes(path, streamIndex)
	if err != nil {
		t.Fatalf("index %s stream %d: %v", path, streamIndex, err)
	}
	want := scannedKeyframes(t, path, streamIndex)
	if len(got) != len(want) {
		t.Fatalf("stream %d: %d cued keyframes %v, packet scan has %d %v", streamIndex, len(got), got, len(want), want)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.000001 {
			t.Fatalf("stream %d keyframe %d: cue %v, packet scan %v", streamIndex, i, got[i], want[i])
		}
	}
	return got
}

func patched(t *testing.T, path string, replacements ...[]byte) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(replacements); i += 2 {
		at := bytes.Index(data, replacements[i])
		if at < 0 {
			t.Fatalf("fixture has no % X to patch", replacements[i])
		}
		copy(data[at:], replacements[i+1])
	}
	out := filepath.Join(t.TempDir(), "patched"+filepath.Ext(path))
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

type meteredReaderAt struct {
	inner     io.ReaderAt
	failAfter int64
	failure   error
	calls     atomic.Int64
	bytes     atomic.Int64
}

func (r *meteredReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.calls.Add(1) > r.failAfter {
		return 0, r.failure
	}
	n, err := r.inner.ReadAt(p, off)
	r.bytes.Add(int64(n))
	return n, err
}

func TestKeyframesMatchPacketScan(t *testing.T) {
	cases := map[string]struct {
		name   string
		args   []string
		stream int
		count  int
	}{
		"matroska b-frames":    {"a.mkv", join(picture(20), tone(20), h264(50)), 0, 10},
		"matroska short gop":   {"b.mkv", join(picture(20), tone(20), h264(7)), 0, 72},
		"matroska all intra":   {"c.mkv", join(picture(4), tone(4), h264(1)), 0, 100},
		"matroska late start":  {"d.mkv", join(picture(20), tone(20), h264(50), []string{"-output_ts_offset", "7.3"}), 0, 10},
		"matroska audio first": {"e.mkv", join(tone(20), picture(20), []string{"-map", "0:a", "-map", "1:v"}, h264(50)), 1, 10},
		"matroska cues first":  {"f.mkv", join(picture(20), tone(20), h264(50), []string{"-cues_to_front", "1"}), 0, 10},
		"webm vp9 opus":        {"g.webm", join(picture(20), tone(20), []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-g", "40", "-c:a", "libopus"}), 0, 13},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := encode(t, c.name, c.args...)
			if got := requireScanMatch(t, path, c.stream); len(got) != c.count {
				t.Fatalf("%d keyframes %v, fixture was built with %d", len(got), got, c.count)
			}
		})
	}
}

func TestKeyframesFollowTheProbeStreamIndex(t *testing.T) {
	second := []string{"-f", "lavfi", "-t", "20", "-i", "testsrc2=size=160x90:rate=30"}
	path := encode(t, "two.mkv", join(picture(20), tone(20), second, []string{"-map", "0:v", "-map", "1:a", "-map", "2:v", "-g:v:1", "13"}, h264(50))...)
	first := requireScanMatch(t, path, 0)
	other := requireScanMatch(t, path, 2)
	if len(first) == len(other) {
		t.Fatalf("fixture tracks must differ: %d and %d keyframes", len(first), len(other))
	}
	if _, err := Keyframes(path, 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("audio stream index returned %v, want ErrUnsupported", err)
	}
	if _, err := Keyframes(path, 3); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing stream index returned %v, want ErrUnsupported", err)
	}
}

func TestKeyframesHonourTimestampScaleAndCodecDelay(t *testing.T) {
	source := encode(t, "plain.mkv", join(picture(20), tone(20), h264(50))...)
	plain := requireScanMatch(t, source, 0)

	scaled := requireScanMatch(t, patched(t, source, timestampScaleMillisecond, timestampScale400Micro), 0)
	if math.Abs(scaled[1]-plain[1]*0.4) > 0.000001 {
		t.Fatalf("second keyframe at %v after rescaling %v by 0.4", scaled[1], plain[1])
	}

	delayed := requireScanMatch(t, patched(t, source, languageUndetermined, codecDelay80Millisecond), 0)
	if math.Abs(delayed[0]-(plain[0]-0.08)) > 0.000001 {
		t.Fatalf("first keyframe at %v with an 80 ms codec delay, plain file has %v", delayed[0], plain[0])
	}

	requireScanMatch(t, patched(t, source, timestampScaleMillisecond, timestampScale400Micro, languageUndetermined, codecDelay80Millisecond), 0)
}

func TestKeyframesReadOnlyTheHeadAndTheCues(t *testing.T) {
	path := encode(t, "large.mkv", join([]string{"-f", "lavfi", "-t", "60", "-i", "testsrc2=size=640x360:rate=25"}, tone(60), h264(50))...)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	reader := &meteredReaderAt{inner: file, failAfter: math.MaxInt64}
	keyframes, err := KeyframesFrom(reader, info.Size(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyframes) != 30 {
		t.Fatalf("%d keyframes, want 30", len(keyframes))
	}
	if calls := reader.calls.Load(); calls > 2 {
		t.Fatalf("%d reads, want one for the head and one for the cues", calls)
	}
	if read, limit := reader.bytes.Load(), info.Size()/20; read > limit {
		t.Fatalf("read %d of %d bytes, limit %d", read, info.Size(), limit)
	}
}

func TestKeyframesRejectLayoutsWithoutAnIndex(t *testing.T) {
	testmedia.Require(t)
	dir := t.TempDir()
	indexed := encode(t, "indexed.mkv", join(picture(6), tone(6), h264(50))...)
	data, err := os.ReadFile(indexed)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, content []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	piped, err := exec.Command("ffmpeg", join([]string{"-hide_banner", "-loglevel", "error"}, picture(6), h264(50), []string{"-f", "matroska", "-"})...).Output()
	if err != nil {
		t.Fatalf("pipe fixture: %v", err)
	}
	cases := map[string]string{
		"written to a pipe": write("piped.mkv", piped),
		"live":              encode(t, "live.mkv", join(picture(6), h264(50), []string{"-live", "1"})...),
		"truncated":         write("truncated.mkv", data[:len(data)/2]),
		"cues cut off":      write("tail.mkv", data[:len(data)-64]),
		"no duration":       write("endless.mkv", bytes.Replace(data, segmentDuration, mislabelledDuration, 1)),
		"empty":             write("empty.mkv", nil),
		"not matroska":      encode(t, "other.mp4", join(picture(2), h264(50))...),
		"noise":             write("noise.mkv", bytes.Repeat([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xFF}, 40000)),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			if keyframes, err := Keyframes(path, 0); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("returned %v, %v; want ErrUnsupported", keyframes, err)
			}
		})
	}
}

func TestKeyframesRejectSparseCues(t *testing.T) {
	source := encode(t, "dense.mkv", join(picture(60), h264(50))...)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	cuesAt := bytes.LastIndex(data, cuesElement)
	if cuesAt < 0 {
		t.Fatal("fixture has no cues")
	}
	at := cuesAt + bytes.Index(data[cuesAt:], cueOnTrackOne) + len(cueOnTrackOne)
	for removed := 0; removed < 8; removed++ {
		next := bytes.Index(data[at:], cueOnTrackOne)
		if next < 0 {
			t.Fatal("fixture has too few cue points")
		}
		copy(data[at+next:], cueOnAbsentTrack)
		at += next + len(cueOnAbsentTrack)
	}
	sparse := filepath.Join(t.TempDir(), "sparse.mkv")
	if err := os.WriteFile(sparse, data, 0o644); err != nil {
		t.Fatal(err)
	}

	index, err := readIndex(bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatalf("read sparse cues: %v", err)
	}
	scan := scannedKeyframes(t, sparse, 0)
	if len(scan) != 30 || len(index.Keyframes) != 22 {
		t.Fatalf("%d cued keyframes of %d, want 22 of 30", len(index.Keyframes), len(scan))
	}
	for _, cue := range index.Keyframes {
		onKeyframe := false
		for _, keyframe := range scan {
			onKeyframe = onKeyframe || math.Abs(cue-keyframe) <= 0.000001
		}
		if !onKeyframe {
			t.Fatalf("cue at %v is not one of the keyframes %v", cue, scan)
		}
	}
	if gap := index.longestGap(); math.Abs(gap-18) > 0.001 {
		t.Fatalf("longest gap %v, want 18 s across the eight missing cues", gap)
	}
	if _, err := Keyframes(sparse, 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("sparse cues returned %v, want ErrUnsupported so the packet scan runs", err)
	}
}

func TestKeyframesReportReadErrorsAsFailures(t *testing.T) {
	path := encode(t, "source.mkv", join(picture(6), tone(6), h264(50))...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("storage offline")
	for _, failAfter := range []int64{0, 1} {
		reader := &meteredReaderAt{inner: bytes.NewReader(data), failAfter: failAfter, failure: failure}
		_, err := KeyframesFrom(reader, int64(len(data)), 0)
		if !errors.Is(err, failure) || errors.Is(err, ErrUnsupported) {
			t.Fatalf("read failure after %d calls surfaced as %v", failAfter, err)
		}
	}
}

func TestAcceptedCueGapsKeepThePlanKeyframeAligned(t *testing.T) {
	random := rand.New(rand.NewSource(28))
	for round := 0; round < 5000; round++ {
		keyframes := []float64{0}
		for len(keyframes) < 40 {
			gap := maxCueGap
			if random.Intn(4) != 0 {
				gap = 0.04 + random.Float64()*(maxCueGap-0.04)
			}
			keyframes = append(keyframes, microseconds(keyframes[len(keyframes)-1]+gap))
		}
		index := cueIndex{Keyframes: keyframes, Duration: keyframes[len(keyframes)-1] + random.Float64()*maxCueGap}
		if index.longestGap() > maxCueGap {
			continue
		}
		for _, target := range []float64{2, 4, 6, 10} {
			if !playlist.KeyframeAligned(index.Keyframes, index.Duration, target) {
				t.Fatalf("round %d: cue gaps up to %.3f s lose keyframe alignment at target %v: %v", round, index.longestGap(), target, index.Keyframes)
			}
		}
	}
	sparse := []float64{0, maxCueGap + 0.6}
	if playlist.KeyframeAligned(sparse, sparse[1]+1, 6) {
		t.Fatalf("a %.1f s gap is expected to break keyframe alignment; maxCueGap is out of date", sparse[1])
	}
}

func TestKeyframesBoundAllocationsFromDeclaredSizes(t *testing.T) {
	source := encode(t, "source.mkv", join(picture(6), tone(6), h264(50))...)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	cuesAt := bytes.LastIndex(data, cuesElement)
	if cuesAt < 0 {
		t.Fatal("fixture has no cues")
	}
	unknownSize := []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	hugeLeaf := append(append([]byte{}, cuesElement...), unknownSize...)
	hugeLeaf = append(hugeLeaf, 0xEC, 0x01, 0x00, 0x00, 0x00, 0x06, 0x40, 0x00, 0x00)
	emptyPoints := append(append([]byte{}, cuesElement...), unknownSize...)
	emptyPoints = append(emptyPoints, bytes.Repeat([]byte{0xBB, 0x80}, 7000000)...)
	cases := map[string]struct {
		tail       []byte
		allocation uint64
		read       int64
	}{
		"leaf declaring 100 MiB":   {hugeLeaf, 8 << 20, 1 << 20},
		"seven million cue points": {emptyPoints, 2 << 30, 6 << 20},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			crafted := append(append([]byte{}, data[:cuesAt]...), c.tail...)
			reader := &meteredReaderAt{inner: bytes.NewReader(crafted), failAfter: math.MaxInt64}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := KeyframesFrom(reader, int64(len(crafted)), 0)
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("returned %v, want ErrUnsupported", err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > c.allocation {
				t.Fatalf("allocated %d bytes for a %d byte file, limit %d", allocated, len(crafted), c.allocation)
			}
			if read := reader.bytes.Load(); read > c.read {
				t.Fatalf("read %d of %d bytes before giving up, limit %d", read, len(crafted), c.read)
			}
		})
	}
}
