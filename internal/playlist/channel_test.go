package playlist

import (
	"math"
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func channelProfile(t *testing.T, profile domain.ChannelProfile) domain.ChannelProfile {
	t.Helper()
	normalised, err := profile.Normalised()
	if err != nil {
		t.Fatal(err)
	}
	return normalised
}

func TestCalculateChannelSegmentsTailRules(t *testing.T) {
	film := channelProfile(t, domain.ChannelProfile{})
	pal := channelProfile(t, domain.ChannelProfile{FrameRateNum: 25, SegmentFrames: 50})
	cases := []struct {
		name       string
		profile    domain.ChannelProfile
		duration   float64
		frames     int
		count      int
		lastFrames int
	}{
		{"exact multiple", pal, 20, 500, 10, 50},
		{"remainder of half a second is its own segment", pal, 20.52, 513, 11, 13},
		{"remainder under half a second merges", pal, 20.48, 512, 10, 62},
		{"shorter than one segment", pal, 1.2, 30, 1, 30},
		{"shorter than half a second", pal, 0.2, 5, 1, 5},
		{"shorter than a frame", pal, 0.001, 1, 1, 1},
		{"film rate, short tail kept", film, 6082.73, 145840, 3039, 16},
		{"film rate, longer tail kept", film, 4161.6, 99779, 2079, 35},
		{"one hundred hours, sliver merges", film, 360000, 8631369, 179820, 57},
	}
	for _, c := range cases {
		segments := CalculateChannelSegments(c.duration, c.profile)
		if got := ChannelFrames(c.duration, c.profile); got != c.frames {
			t.Errorf("%s: %d frames, want %d", c.name, got, c.frames)
		}
		if len(segments) != c.count {
			t.Errorf("%s: %d segments, want %d", c.name, len(segments), c.count)
			continue
		}
		total := 0.0
		for index, segment := range segments {
			total += segment.Duration
			if segment.Index != index {
				t.Fatalf("%s: segment %d has index %d", c.name, index, segment.Index)
			}
			if want := domain.ChannelFrameTime(index*c.profile.SegmentFrames, c.profile.FrameRateNum, c.profile.FrameRateDen); segment.Start != want {
				t.Fatalf("%s: segment %d starts at %v, want %v", c.name, index, segment.Start, want)
			}
			if index > 0 && !domain.Adjacent(segments[index-1], segment) {
				t.Fatalf("%s: segments %d and %d are not adjacent", c.name, index-1, index)
			}
		}
		last := segments[len(segments)-1]
		if frames := int(math.Round(last.Duration * c.profile.FrameRate())); frames != c.lastFrames {
			t.Errorf("%s: last segment holds %d frames, want %d", c.name, frames, c.lastFrames)
		}
		if want := domain.ChannelFrameTime(c.frames, c.profile.FrameRateNum, c.profile.FrameRateDen); math.Abs(total-want) > 1e-6 || last.End != want {
			t.Errorf("%s: durations sum to %v and end at %v, want %v", c.name, total, last.End, want)
		}
	}
	if CalculateChannelSegments(0, film) != nil || CalculateChannelSegments(math.NaN(), film) != nil || CalculateChannelSegments(-3, film) != nil {
		t.Error("a source without a duration has no plan")
	}
}

func TestChannelBatchesSitOnAFixedGrid(t *testing.T) {
	cases := []struct{ index, perJob, count, start, end int }{
		{0, 10, 50, 0, 9},
		{9, 10, 50, 0, 9},
		{10, 10, 50, 10, 19},
		{47, 10, 50, 40, 49},
		{47, 10, 48, 40, 47},
		{5, 1, 50, 5, 5},
		{3, 1000, 50, 0, 49},
	}
	for _, c := range cases {
		if start, end := ChannelBatch(c.index, c.perJob, c.count); start != c.start || end != c.end {
			t.Errorf("ChannelBatch(%d, %d, %d) = %d-%d, want %d-%d", c.index, c.perJob, c.count, start, end, c.start, c.end)
		}
	}
}
