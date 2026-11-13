package mkvindex

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestSubtitlePacketsDropATrackWhoseCuesPointAtOtherBlocks(t *testing.T) {
	data, err := os.ReadFile(subtitledFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	read := func(candidate SubtitleCandidate) int {
		t.Helper()
		layout, err := ReadSubtitleLayout(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Cues == nil {
			candidate = layout.Tracks[2]
		}
		packets, err := ReadSubtitlePackets(context.Background(), bytes.NewReader(data), int64(len(data)), map[int]SubtitleCandidate{2: candidate})
		if err != nil {
			t.Fatalf("a cue on the wrong block must not fail the read: %v", err)
		}
		return len(packets[2])
	}
	if got := read(SubtitleCandidate{}); got != subtitleFixtureCues {
		t.Fatalf("untouched track returned %d packets, want %d", got, subtitleFixtureCues)
	}
	layout, err := ReadSubtitleLayout(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	otherTrack := layout.Tracks[2]
	otherTrack.Number = layout.Tracks[3].Number
	if got := read(otherTrack); got != 0 {
		t.Fatalf("cues that land on another track's blocks returned %d packets", got)
	}
	otherTime := layout.Tracks[2]
	otherTime.Cues = append([]SubtitleCue(nil), otherTime.Cues...)
	otherTime.Cues[len(otherTime.Cues)/2].Time++
	if got := read(otherTime); got != 0 {
		t.Fatalf("a cue whose time differs from its block returned %d packets", got)
	}
}

func TestSubtitleTrackWithoutRelativeCuePositionsIsNeverComplete(t *testing.T) {
	located := SubtitleCandidate{FrameCount: -1, Cues: []SubtitleCue{{Time: 10, Cluster: 4096, Relative: 12}, {Time: 20, Cluster: 4096, Relative: 90}}}
	if !located.Complete("Lavf60.16.100") {
		t.Fatal("a libavformat track whose cues all locate their block must be complete")
	}
	unlocated := located
	unlocated.Cues = []SubtitleCue{{Time: 10, Cluster: 4096, Relative: 12}, {Time: 20, Cluster: 4096}}
	if unlocated.Complete("Lavf54.63.104") {
		t.Fatal("a cue without a relative position cannot be read, the track must not be complete")
	}
	unlocated.FrameCount = 2
	if unlocated.Complete("mkvmerge") {
		t.Fatal("a counted track with an unlocated cue must not be complete")
	}
}

func TestLongestGapCountsTheStretchAfterTheLastCue(t *testing.T) {
	index := &cueIndex{Keyframes: []float64{0, 2, 4}, Duration: 30}
	if gap := index.longestGap(); gap != 26 {
		t.Fatalf("longest gap = %v, the last cue is 26 s before the end", gap)
	}
	index.Duration = 5
	if gap := index.longestGap(); gap != 2 {
		t.Fatalf("longest gap = %v, want the 2 s between cues", gap)
	}
}
