package mp4index

import (
	"fmt"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func TestGroupBytesFoldLeadingSamplesIntoFirstGroup(t *testing.T) {
	stsz := &mp4.StszBox{SampleSize: []uint32{5, 6, 10, 1, 20, 2}}
	got := groupBytes(stsz, []uint32{3, 5})
	if fmt.Sprint(got) != "[22 22]" {
		t.Fatalf("got %v", got)
	}
}
