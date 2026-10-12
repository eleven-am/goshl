package mp4index

import (
	"errors"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func trakWithEdits(entries ...mp4.ElstEntry) *mp4.TrakBox {
	return &mp4.TrakBox{Edts: &mp4.EdtsBox{Elst: []*mp4.ElstBox{{Entries: entries}}}}
}

func TestEditTimelineAcceptsSupportedLists(t *testing.T) {
	empty := mp4.ElstEntry{SegmentDuration: 500, MediaTime: -1, MediaRateInteger: 1}
	media := mp4.ElstEntry{SegmentDuration: 5000, MediaTime: 1024, MediaRateInteger: 1}

	line, err := editTimeline(trakWithEdits(empty, empty, media), 1000, 12800)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line.emptyDur != 1000 || line.mediaTime != 1024 {
		t.Fatalf("unexpected timeline: %+v", line)
	}

	line, err = editTimeline(&mp4.TrakBox{}, 1000, 12800)
	if err != nil || line.mediaTime != 0 || line.emptyDur != 0 {
		t.Fatalf("missing edit list must be a no-op: %+v %v", line, err)
	}
}

func TestEditTimelineRejectsComplexLists(t *testing.T) {
	media := mp4.ElstEntry{SegmentDuration: 5000, MediaTime: 1024, MediaRateInteger: 1}
	empty := mp4.ElstEntry{SegmentDuration: 500, MediaTime: -1, MediaRateInteger: 1}
	cases := map[string]*mp4.TrakBox{
		"two media edits":    trakWithEdits(media, media),
		"empty edit in tail": trakWithEdits(media, empty),
		"only empty edits":   trakWithEdits(empty),
		"slow rate":          trakWithEdits(mp4.ElstEntry{SegmentDuration: 5000, MediaTime: 0, MediaRateInteger: 0, MediaRateFraction: 1}),
		"double rate":        trakWithEdits(mp4.ElstEntry{SegmentDuration: 5000, MediaTime: 0, MediaRateInteger: 2}),
	}
	for name, trak := range cases {
		trak := trak
		t.Run(name, func(t *testing.T) {
			if _, err := editTimeline(trak, 1000, 12800); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("expected ErrUnsupported, got %v", err)
			}
		})
	}
}

func TestSyncDecodeTimesWalksRunsAndRejectsBadSamples(t *testing.T) {
	stts := &mp4.SttsBox{SampleCount: []uint32{3, 2}, SampleTimeDelta: []uint32{10, 20}}
	got, err := syncDecodeTimes(stts, []uint32{1, 3, 4, 5}, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{0, 20, 30, 50}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	for name, samples := range map[string][]uint32{"zero": {0}, "beyond": {6}, "unordered": {3, 2}, "duplicate": {2, 2}} {
		if _, err := syncDecodeTimes(stts, samples, 5); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s: expected ErrUnsupported, got %v", name, err)
		}
	}
}
