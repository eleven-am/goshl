package domain

import (
	"encoding/json"
	"testing"
)

func TestJobJSONRoundTripKeepsTrailing(t *testing.T) {
	job := Job{
		ID:               "job",
		Segments:         []Segment{{Index: 4, Start: 8, End: 10, Duration: 2}},
		Overlap:          &Segment{Index: 3, Start: 6, End: 8, Duration: 2},
		Trailing:         &Segment{Index: 5, Start: 10, End: 12, Duration: 2},
		VideoStreamIndex: 2,
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	var decoded Job
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	if decoded.Trailing == nil || *decoded.Trailing != *job.Trailing {
		t.Fatalf("trailing segment was lost: %+v", decoded.Trailing)
	}
	if decoded.Overlap == nil || *decoded.Overlap != *job.Overlap {
		t.Fatalf("overlap segment was lost: %+v", decoded.Overlap)
	}
	if decoded.VideoStreamIndex != 2 {
		t.Fatalf("video stream index was lost: %d", decoded.VideoStreamIndex)
	}
}
