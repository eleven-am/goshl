package mkvindex

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/at-wat/ebml-go"
)

func TestLibavformatInferenceNeedsNonNegativeLeadingBlocks(t *testing.T) {
	plain, _, _ := openLayout(t, subtitledFixture(t))
	if !plain.CuedFromStart || !plain.Complete(2) || !plain.Complete(3) {
		t.Fatalf("ordinary libavformat source: cued from start %v, complete %v %v", plain.CuedFromStart, plain.Complete(2), plain.Complete(3))
	}
	negative, _, _ := openLayout(t, subtitledFixture(t, "-output_ts_offset", "-5", "-avoid_negative_ts", "disabled"))
	if len(negative.Tracks[2].Cues) >= subtitleFixtureCues {
		t.Fatalf("fixture must leave packets before time zero without a cue, it has %d cues", len(negative.Tracks[2].Cues))
	}
	if negative.CuedFromStart || negative.Complete(2) || negative.Complete(3) {
		t.Fatalf("source with blocks before time zero: cued from start %v, complete %v %v", negative.CuedFromStart, negative.Complete(2), negative.Complete(3))
	}
	dir := t.TempDir()
	shifted := filepath.Join(dir, "shifted.mkv")
	build := []string{"-v", "error", "-y", "-f", "lavfi", "-t", "20", "-i", "testsrc2=size=160x90:rate=25", "-itsoffset", "-3.5", "-i", subtitleScript(t, "early"),
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:s", "srt", "-avoid_negative_ts", "disabled", shifted}
	if data, err := exec.Command("ffmpeg", build...).CombinedOutput(); err != nil {
		t.Fatalf("build shifted fixture: %v: %s", err, data)
	}
	early, _, _ := openLayout(t, shifted)
	if len(early.Tracks[1].Cues) == 0 || len(early.Tracks[1].Cues) >= subtitleFixtureCues {
		t.Fatalf("fixture must cue only the packets from time zero on, it has %d cues", len(early.Tracks[1].Cues))
	}
	if early.CuedFromStart || early.Complete(1) {
		t.Fatalf("subtitle blocks before time zero ahead of the first video cue: cued from start %v, complete %v", early.CuedFromStart, early.Complete(1))
	}
	if plain.Complete(0) || plain.Complete(99) {
		t.Fatal("a stream that is not a subtitle track must not be complete")
	}
}

func leadFixture(t *testing.T, leading ebml.Block) ([]byte, map[uint64][]SubtitleCue) {
	t.Helper()
	type content struct {
		Timestamp uint64       `ebml:"Timestamp"`
		Simple    []ebml.Block `ebml:"SimpleBlock"`
	}
	cued := ebml.Block{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: [][]byte{{1, 2, 3}}}
	var lead, whole bytes.Buffer
	if err := ebml.Marshal(&content{Simple: []ebml.Block{leading}}, &lead); err != nil {
		t.Fatal(err)
	}
	if err := ebml.Marshal(&content{Simple: []ebml.Block{leading, cued}}, &whole); err != nil {
		t.Fatal(err)
	}
	source := append(append([]byte{}, ebml.ElementCluster.Bytes()...), 0x80|byte(whole.Len()))
	source = append(source, whole.Bytes()...)
	return source, map[uint64][]SubtitleCue{1: {{Time: 0, Cluster: 0, Relative: int64(lead.Len())}}}
}

func leadCheck(source []byte, cues map[uint64][]SubtitleCue) bool {
	cluster, data, found := firstCluster(&cursor{source: bytes.NewReader(source), size: int64(len(source))}, 0)
	return found && cuedFromStart(bytes.NewReader(source), int64(len(source)), cluster, data, cues)
}

func TestLeadingBlocksBeforeTimeZeroAreNoticed(t *testing.T) {
	early := ebml.Block{TrackNumber: 2, Timecode: 4, Data: [][]byte{{9}}}
	source, cues := leadFixture(t, early)
	if !leadCheck(source, cues) {
		t.Fatal("a cluster whose leading block is at 4 ms holds nothing before time zero")
	}
	early.Timecode = -4
	source, cues = leadFixture(t, early)
	if leadCheck(source, cues) {
		t.Fatal("a block 4 ms before time zero precedes the first cue and was not noticed")
	}
	cues[1][0].Cluster = 64
	if leadCheck(source, cues) {
		t.Fatal("no cue points into the first cluster, its blocks cannot be vouched for")
	}
}
