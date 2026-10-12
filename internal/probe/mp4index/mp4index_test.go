package mp4index

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

type fixtureSpec struct {
	name        string
	args        []string
	videoStream int
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is unavailable", tool)
		}
	}
}

func runFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if data, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v: %s", args, err, data)
	}
}

func video(extra ...string) []string {
	return append([]string{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25"}, extra...)
}

func ffprobeKeyframes(t *testing.T, path string, streamIndex int) []float64 {
	t.Helper()
	cmd := exec.Command("ffprobe", "-v", "error",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var keyframes []float64
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ",")
		if len(parts) < 2 || !strings.Contains(parts[1], "K") {
			continue
		}
		pts, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			continue
		}
		keyframes = append(keyframes, pts)
	}
	return keyframes
}

func buildFixtures(t *testing.T) map[string]fixtureSpec {
	return map[string]fixtureSpec{
		"faststart": {
			name: "faststart.mp4",
			args: video("-t", "6", "-c:v", "libx264", "-g", "25", "-sc_threshold", "0", "-movflags", "+faststart"),
		},
		"moov_at_end": {
			name: "moov_end.mp4",
			args: video("-t", "6", "-c:v", "libx264", "-g", "40", "-sc_threshold", "0"),
		},
		"bframes": {
			name: "bframes.mp4",
			args: video("-t", "6", "-c:v", "libx264", "-bf", "2", "-g", "30", "-movflags", "+faststart"),
		},
		"audio_first": {
			name:        "audio_first.mp4",
			args:        video("-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6", "-map", "1:a", "-map", "0:v", "-c:v", "libx264", "-bf", "2", "-g", "50", "-c:a", "aac"),
			videoStream: 1,
		},
		"empty_edit_odd_timescale": {
			name:        "empty_edit_odd.mp4",
			args:        []string{"-itsoffset", "0.084", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6", "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-g", "24", "-sc_threshold", "0", "-c:a", "aac", "-video_track_timescale", "11988"},
			videoStream: 0,
		},
		"empty_edit": {
			name:        "empty_edit.mp4",
			args:        []string{"-itsoffset", "0.5", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6", "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-g", "25", "-sc_threshold", "0", "-c:a", "aac"},
			videoStream: 0,
		},
	}
}

func makeFixtures(t *testing.T) map[string]string {
	t.Helper()
	requireTools(t)
	dir := t.TempDir()
	paths := map[string]string{}
	for key, spec := range buildFixtures(t) {
		path := filepath.Join(dir, spec.name)
		runFFmpeg(t, append(spec.args, path)...)
		paths[key] = path
	}
	patchEmptyEditDuration(t, paths["empty_edit_odd_timescale"], 84)
	return paths
}

func assertSame(t *testing.T, path string, streamIndex int) []float64 {
	t.Helper()
	got, err := Keyframes(path, streamIndex)
	if err != nil {
		t.Fatalf("fast path: %v", err)
	}
	want := ffprobeKeyframes(t, path, streamIndex)
	if len(want) == 0 {
		t.Fatalf("fixture has no keyframes")
	}
	if len(got) != len(want) {
		t.Fatalf("keyframe count: got %d want %d\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keyframe %d: got %v want %v\n got=%v\nwant=%v", i, got[i], want[i], got, want)
		}
	}
	return got
}

func TestKeyframesMatchFFProbe(t *testing.T) {
	paths := makeFixtures(t)
	specs := buildFixtures(t)
	for key, path := range paths {
		key, path := key, path
		t.Run(key, func(t *testing.T) {
			assertSame(t, path, specs[key].videoStream)
		})
	}
}

func TestKeyframesMatchFFProbeWithMultipleMdat(t *testing.T) {
	paths := makeFixtures(t)
	path := filepath.Join(t.TempDir(), "multi.mp4")
	data, err := os.ReadFile(paths["faststart"])
	if err != nil {
		t.Fatal(err)
	}
	extra := make([]byte, 16)
	binary.BigEndian.PutUint32(extra[0:4], 16)
	copy(extra[4:8], "mdat")
	if err := os.WriteFile(path, append(data, extra...), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSame(t, path, 0)
}

type countingReaderAt struct {
	inner io.ReaderAt
	bytes atomic.Int64
}

func TestKeyframesReadOnlyTheHeader(t *testing.T) {
	paths := makeFixtures(t)
	for _, key := range []string{"faststart", "moov_at_end"} {
		key := key
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "padded.mp4")
			data, err := os.ReadFile(paths[key])
			if err != nil {
				t.Fatal(err)
			}
			moovSize := moovBoxSize(t, data)
			pad := int64(256 << 20)
			header := make([]byte, 16)
			binary.BigEndian.PutUint32(header[0:4], 1)
			copy(header[4:8], "free")
			binary.BigEndian.PutUint64(header[8:16], uint64(16+pad))
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(data); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(header); err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(int64(len(data)) + 16 + pad); err != nil {
				t.Fatal(err)
			}
			file.Close()

			reader, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			info, _ := reader.Stat()
			counter := &countingReaderAt{inner: reader}
			got, err := KeyframesFrom(counter, info.Size(), 0)
			if err != nil {
				t.Fatalf("fast path: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("no keyframes")
			}
			if limit := moovSize + 4096; counter.bytes.Load() > limit {
				t.Fatalf("read %d bytes, moov is %d (limit %d)", counter.bytes.Load(), moovSize, limit)
			}
		})
	}
}

func moovBoxSize(t *testing.T, data []byte) int64 {
	t.Helper()
	pos := 0
	for pos+8 <= len(data) {
		size := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if string(data[pos+4:pos+8]) == "moov" {
			return size
		}
		if size < 8 {
			break
		}
		pos += int(size)
	}
	t.Fatal("moov not found")
	return 0
}

func TestKeyframesIgnoreCorruptMediaData(t *testing.T) {
	paths := makeFixtures(t)
	data, err := os.ReadFile(paths["faststart"])
	if err != nil {
		t.Fatal(err)
	}
	want, err := Keyframes(paths["faststart"], 0)
	if err != nil {
		t.Fatal(err)
	}
	mdat := bytes.Index(data, []byte("mdat"))
	for i := mdat + 4; i < len(data); i++ {
		data[i] = 0xAB
	}
	path := filepath.Join(t.TempDir(), "garbage_mdat.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Keyframes(path, 0)
	if err != nil {
		t.Fatalf("fast path: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestKeyframesRejectUnsupportedInputs(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	mkv := filepath.Join(dir, "a.mkv")
	runFFmpeg(t, append(video("-t", "3", "-c:v", "libx264"), mkv)...)
	frag := filepath.Join(dir, "frag.mp4")
	runFFmpeg(t, append(video("-t", "3", "-c:v", "libx264", "-g", "25", "-movflags", "frag_keyframe+empty_moov"), frag)...)
	garbage := filepath.Join(dir, "garbage.mp4")
	if err := os.WriteFile(garbage, append([]byte{0, 0, 0, 12, 'm', 'o', 'o', 'v'}, bytes.Repeat([]byte{0xFF}, 64)...), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(dir, "valid.mp4")
	runFFmpeg(t, append(video("-t", "3", "-c:v", "libx264", "-g", "25"), valid)...)

	trimmed := filepath.Join(dir, "trimmed.mp4")
	runFFmpeg(t, "-ss", "1.3", "-i", valid, "-c", "copy", trimmed)

	cases := map[string]struct {
		path   string
		stream int
	}{
		"unaligned edit": {trimmed, 0},
		"mkv":            {mkv, 0},
		"fragmented":     {frag, 0},
		"garbage moov":   {garbage, 0},
		"empty file":     {empty, 0},
		"missing file":   {filepath.Join(dir, "nope.mp4"), 0},
		"stream missing": {valid, 5},
		"negative index": {valid, -1},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			got, err := Keyframes(tc.path, tc.stream)
			if err == nil {
				t.Fatalf("expected an error, got keyframes %v", got)
			}
		})
	}
}

func TestKeyframesRejectAudioTrackAsVideo(t *testing.T) {
	paths := makeFixtures(t)
	if _, err := Keyframes(paths["audio_first"], 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for an audio trak, got %v", err)
	}
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.inner.ReadAt(p, off)
	c.bytes.Add(int64(n))
	return n, err
}

func patchEmptyEditDuration(t *testing.T, path string, duration uint32) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(data, []byte("elst"))
	if at < 0 || data[at+4] != 0 {
		t.Fatalf("version 0 elst not found")
	}
	binary.BigEndian.PutUint32(data[at+12:], duration)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKeyframesRejectFragmentationMarkers(t *testing.T) {
	paths := makeFixtures(t)
	data, err := os.ReadFile(paths["faststart"])
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := KeyframesFrom(bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("baseline must be indexable: %v", err)
	}

	parsed.Moov.AddChild(mp4.NewMvexBox())
	var withMvex bytes.Buffer
	if err := parsed.Ftyp.Encode(&withMvex); err != nil {
		t.Fatal(err)
	}
	if err := parsed.Moov.Encode(&withMvex); err != nil {
		t.Fatal(err)
	}
	if _, err := KeyframesFrom(bytes.NewReader(withMvex.Bytes()), int64(withMvex.Len()), 0); err == nil || !strings.Contains(err.Error(), "fragmented") {
		t.Fatalf("expected a fragmented rejection for mvex with sample tables, got %v", err)
	}

	moofHeader := []byte{0, 0, 0, 8, 'm', 'o', 'o', 'f'}
	withMoof := append(append([]byte{}, data[:ftypSize(data)]...), moofHeader...)
	withMoof = append(withMoof, data[ftypSize(data):]...)
	if _, err := KeyframesFrom(bytes.NewReader(withMoof), int64(len(withMoof)), 0); err == nil || !strings.Contains(err.Error(), "fragmented") {
		t.Fatalf("expected a fragmented rejection for a leading moof, got %v", err)
	}
}

func ftypSize(data []byte) int {
	return int(binary.BigEndian.Uint32(data[0:4]))
}
