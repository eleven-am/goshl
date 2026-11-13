package mp4index

import (
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func TestSampleTimesMatchDecodeTimeTableInOnePass(t *testing.T) {
	stts := &mp4.SttsBox{
		SampleCount:     []uint32{3, 0, 1, 5, 2},
		SampleTimeDelta: []uint32{10, 99, 7, 1, 1000},
	}
	times := sampleTimes{stts: stts, runStart: 1}
	previousRun := 0
	for sample := uint32(1); sample <= 11; sample++ {
		wantTime, wantDuration, err := stts.GetDecodeTime(sample)
		if err != nil {
			t.Fatal(err)
		}
		gotTime, gotDuration, ok := times.at(sample)
		if !ok || gotTime != wantTime || gotDuration != wantDuration {
			t.Fatalf("sample %d: got %d/%d ok=%v, want %d/%d", sample, gotTime, gotDuration, ok, wantTime, wantDuration)
		}
		if times.run < previousRun {
			t.Fatalf("sample %d rewound the table cursor from run %d to %d", sample, previousRun, times.run)
		}
		previousRun = times.run
	}
	if _, _, ok := times.at(12); ok {
		t.Fatal("a sample beyond the table was given a time")
	}
}
