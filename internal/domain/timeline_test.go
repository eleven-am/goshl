package domain

import "testing"

func TestAdjacentRequiresNextIndexAndContiguousTimes(t *testing.T) {
	previous := Segment{Index: 2, Start: 4, End: 6}
	cases := []struct {
		name string
		next Segment
		want bool
	}{
		{"contiguous", Segment{Index: 3, Start: 6, End: 8}, true},
		{"within tolerance", Segment{Index: 3, Start: 6 + BoundaryTolerance/2, End: 8}, true},
		{"beyond tolerance", Segment{Index: 3, Start: 6 + 2*BoundaryTolerance, End: 8}, false},
		{"same index", Segment{Index: 2, Start: 6, End: 8}, false},
		{"skipped index", Segment{Index: 4, Start: 6, End: 8}, false},
	}
	for _, c := range cases {
		if got := Adjacent(previous, c.next); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
