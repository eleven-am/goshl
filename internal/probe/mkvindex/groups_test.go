package mkvindex

import (
	"bytes"
	"errors"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"testing"

	"github.com/at-wat/ebml-go"
	"github.com/eleven-am/goshl/internal/testmedia"
)

func packetGroups(t *testing.T, path string, keyframes []float64) ([]int64, int64) {
	t.Helper()
	groups := make([]int64, len(keyframes))
	current := -1
	var total int64
	for _, packet := range videoPacketSizes(t, path) {
		if packet.key {
			at := sort.SearchFloat64s(keyframes, packet.pts-0.0005)
			if at >= len(keyframes) || math.Abs(keyframes[at]-packet.pts) > 0.001 {
				t.Fatalf("keyframe at %v is not in the index %v", packet.pts, keyframes)
			}
			current = at
		}
		if current < 0 {
			t.Fatalf("packet at %v precedes the first keyframe", packet.pts)
		}
		groups[current] += packet.size
		total += packet.size
	}
	return groups, total
}

type sizedPacket struct {
	pts  float64
	size int64
	key  bool
}

func videoPacketSizes(t *testing.T, path string) []sizedPacket {
	t.Helper()
	out := runProbe(t, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,size,flags", "-of", "csv=p=0", path)
	var packets []sizedPacket
	for _, line := range bytes.Split(out, []byte("\n")) {
		fields := bytes.Split(bytes.TrimSpace(line), []byte(","))
		if len(fields) < 3 {
			continue
		}
		pts, ptsErr := strconv.ParseFloat(string(fields[0]), 64)
		size, sizeErr := strconv.ParseInt(string(fields[1]), 10, 64)
		if ptsErr != nil || sizeErr != nil {
			continue
		}
		packets = append(packets, sizedPacket{pts: pts, size: size, key: bytes.Contains(bytes.Join(fields[2:], nil), []byte("K"))})
	}
	if len(packets) == 0 {
		t.Fatalf("no video packets in %s", path)
	}
	return packets
}

func runProbe(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("ffprobe", args...).Output()
	if err != nil {
		t.Fatalf("ffprobe %v: %v", args, err)
	}
	return out
}

func noisyFixture(t *testing.T, name string, extra ...string) string {
	t.Helper()
	video := []string{"-f", "lavfi", "-t", "40", "-i", "testsrc2=size=320x180:rate=25,noise=alls=60:allf=t:enable='between(t,12,20)'"}
	return encode(t, name, join(video, tone(40), []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-sc_threshold", "0", "-bf", "2", "-c:a", "ac3", "-b:a", "384k"}, extra)...)
}

func TestKeyframeGroupsBoundTheVideoBytesBetweenCues(t *testing.T) {
	testmedia.Require(t)
	cases := map[string][]string{
		"audio interleaved":      nil,
		"second audio track":     {"-map", "0:v", "-map", "1:a", "-map", "1:a"},
		"start offset":           {"-output_ts_offset", "7.3"},
		"thirty second clusters": {"-cluster_time_limit", "30000", "-cluster_size_limit", "50000000"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			path := noisyFixture(t, "noisy.mkv", extra...)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			keyframes, groups, err := KeyframeGroupsFrom(file, info.Size(), 0)
			if err != nil {
				t.Fatalf("groups: %v", err)
			}
			plain, err := KeyframesFrom(file, info.Size(), 0)
			if err != nil || len(plain) != len(keyframes) || len(groups) != len(keyframes) || len(keyframes) != 20 {
				t.Fatalf("%d keyframes, %d groups, %d plain keyframes (%v)", len(keyframes), len(groups), len(plain), err)
			}
			actual, videoBytes := packetGroups(t, path, keyframes)
			var bound int64
			for i := range groups {
				if groups[i] < actual[i] {
					t.Fatalf("group %d: bound %d is below its %d video bytes", i, groups[i], actual[i])
				}
				bound += groups[i]
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if clusters := int64(bytes.LastIndex(data, cuesElement)); bound > clusters+int64(len(groups))*clusterHeaderSlack || bound < videoBytes {
				t.Fatalf("bounds sum to %d for %d video bytes; the clusters end at %d", bound, videoBytes, clusters)
			}
			heavy, light := groups[7], groups[2]
			if heavy < 4*light {
				t.Fatalf("the noisy group is bounded at %d and a quiet one at %d; the burst is not visible", heavy, light)
			}
		})
	}
}

func TestKeyframeGroupsNeedCuesThatLocateBlocksInOrder(t *testing.T) {
	testmedia.Require(t)
	source := noisyFixture(t, "noisy.mkv")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	cuesAt := bytes.LastIndex(data, cuesElement)
	marker := []byte{0xF7, 0x81, 0x01, 0xF1}
	value := func(from int) (int, int) {
		at := from + bytes.Index(data[from:], marker) + len(marker)
		return at + 1, int(data[at] & 0x7F)
	}
	first, firstLength := value(cuesAt)
	second, secondLength := value(first + firstLength)
	third, thirdLength := value(second + secondLength)
	if secondLength != thirdLength {
		t.Fatalf("cluster positions differ in length: %d and %d", secondLength, thirdLength)
	}
	swapped := append([]byte{}, data...)
	copy(swapped[second:second+secondLength], data[third:third+thirdLength])
	copy(swapped[third:third+thirdLength], data[second:second+secondLength])

	if _, _, err := KeyframeGroupsFrom(bytes.NewReader(swapped), int64(len(swapped)), 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("cue positions out of file order returned %v, want ErrUnsupported", err)
	}
	if keyframes, err := KeyframesFrom(bytes.NewReader(swapped), int64(len(swapped)), 0); err != nil || len(keyframes) != 20 {
		t.Fatalf("keyframe times must still be readable: %d %v", len(keyframes), err)
	}
}

func TestKeyframeGroupsFollowTheRelativePosition(t *testing.T) {
	testmedia.Require(t)
	data, err := os.ReadFile(noisyFixture(t, "noisy.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := KeyframeGroupsFrom(bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	cuesAt := bytes.LastIndex(data, cuesElement)
	marker := []byte{0xF7, 0x81, 0x01, 0xF1}
	at := cuesAt + bytes.Index(data[cuesAt:], marker) + len(marker)
	at += bytes.Index(data[at:], marker) + len(marker)
	at += 1 + int(data[at]&0x7F)
	last := at + 1 + int(data[at+1]&0x7F)
	if data[at] != 0xF0 || data[last] > 0xC0 {
		t.Fatalf("expected a relative position with room to grow, found % X", data[at:last+1])
	}
	data[last] += 50
	_, after, err := KeyframeGroupsFrom(bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if after[0] != before[0]+50 || after[1] != before[1]-50 {
		t.Fatalf("moving the second keyframe 50 bytes changed the groups from %v to %v", before[:2], after[:2])
	}
}

func TestGroupBytesSpanToTheNextCueOrTheEndOfTheClusters(t *testing.T) {
	positions := map[ebml.ElementType]int64{ebml.ElementInfo: 40, ebml.ElementCues: 900, ebml.ElementTags: 950}
	for after, want := range map[int64]int64{500: 900, 920: 950, 960: 1000} {
		if got := clusterEnd(positions, after, 1000); got != want {
			t.Fatalf("clusters after %d end at %d, want %d", after, got, want)
		}
	}
	located := []cuedKeyframe{{offset: 100, relative: true}, {offset: 400, relative: true}}
	if got := groupBytes(located, 900); len(got) != 2 || got[0] != 300+clusterHeaderSlack || got[1] != 500+clusterHeaderSlack || clusterHeaderSlack != 7 {
		t.Fatalf("group bytes = %v, want the spans 300 and 500 plus seven bytes for a longer cluster header", got)
	}
	if got := groupBytes([]cuedKeyframe{{offset: 100, relative: true}, {offset: 400}}, 900); got != nil {
		t.Fatalf("a cue without a relative position produced %v", got)
	}
	if got := groupBytes(located, 400); got != nil {
		t.Fatalf("a last cue at the end of the clusters produced %v", got)
	}
}
