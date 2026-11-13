package mp4index

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func ffprobeGroupBytes(t *testing.T, path string, streamIndex int) ([]int64, int64) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=size,flags",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var groups []int64
	var total int64
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ",")
		if len(parts) < 2 {
			continue
		}
		size, _ := strconv.ParseInt(parts[0], 10, 64)
		total += size
		if strings.Contains(parts[1], "K") {
			groups = append(groups, 0)
		}
		if len(groups) > 0 {
			groups[len(groups)-1] += size
		}
	}
	return groups, total
}

func TestIndexFromGroupBytesMatchFFProbePacketSizes(t *testing.T) {
	paths := makeFixtures(t)
	specs := buildFixtures(t)
	for key, path := range paths {
		key, path := key, path
		t.Run(key, func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			index, err := IndexFrom(file, info.Size(), specs[key].videoStream)
			if err != nil {
				t.Fatal(err)
			}
			want, total := ffprobeGroupBytes(t, path, specs[key].videoStream)
			if len(index.GroupBytes) != len(index.Keyframes) || len(index.GroupBytes) != len(want) {
				t.Fatalf("groups %d keyframes %d want %d", len(index.GroupBytes), len(index.Keyframes), len(want))
			}
			var sum int64
			for i := range want {
				if index.GroupBytes[i] != want[i] {
					t.Fatalf("group %d: got %d want %d", i, index.GroupBytes[i], want[i])
				}
				sum += index.GroupBytes[i]
			}
			if sum != total {
				t.Fatalf("group sum %d, packet sum %d", sum, total)
			}
		})
	}
}

func TestIndexFromReadsMoovOnce(t *testing.T) {
	paths := makeFixtures(t)
	file, err := os.Open(paths["moov_at_end"])
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	keyframesOnly := &countingReaderAt{inner: file}
	if _, err := KeyframesFrom(keyframesOnly, info.Size(), 0); err != nil {
		t.Fatal(err)
	}
	both := &countingReaderAt{inner: file}
	if _, err := IndexFrom(both, info.Size(), 0); err != nil {
		t.Fatal(err)
	}
	if both.bytes.Load() != keyframesOnly.bytes.Load() {
		t.Fatalf("IndexFrom read %d bytes, KeyframesFrom %d", both.bytes.Load(), keyframesOnly.bytes.Load())
	}
}
