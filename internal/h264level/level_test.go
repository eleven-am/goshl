package h264level

import "testing"

func TestMinimumAccountsForScaledFrameSizeAndRate(t *testing.T) {
	if got, ok := Minimum(864, 360, 23.976, 622_713); !ok || got != 30 {
		t.Fatalf("Minimum() = %d, want 30", got)
	}
}

func TestMinimumUsesAConservativeRateWhenUnknown(t *testing.T) {
	if got, ok := Minimum(1920, 1080, 0, 7_500_000); !ok || got != 42 {
		t.Fatalf("Minimum() = %d, want 42", got)
	}
}

func TestMinimumFitsFrameAtThirtyFramesPerSecond(t *testing.T) {
	cases := []struct {
		width, height int
		frameRate     float64
		want          int
	}{
		{854, 480, 23.976, 31},
		{854, 480, 24, 31},
		{854, 480, 25, 31},
		{720, 576, 25, 31},
		{4096, 2160, 24, 52},
		{720, 480, 24, 30},
		{640, 360, 24, 30},
		{1280, 720, 24, 31},
		{1920, 1080, 24, 40},
		{1920, 1080, 60, 42},
		{3840, 2160, 24, 51},
		{3840, 2160, 60, 52},
	}
	for _, c := range cases {
		if got, ok := Minimum(c.width, c.height, c.frameRate, 0); !ok || got != c.want {
			t.Errorf("Minimum(%dx%d at %.3f) = %d, %v; want %d", c.width, c.height, c.frameRate, got, ok, c.want)
		}
	}
}

func TestMinimumStaysWithinPolicyRange(t *testing.T) {
	for _, shape := range [][2]int{{0, 0}, {160, 90}, {320, 180}, {426, 240}, {480, 270}} {
		if got, ok := Minimum(shape[0], shape[1], 24, 300_000); !ok || got != Floor {
			t.Errorf("Minimum(%dx%d) = %d, %v; want the floor %d", shape[0], shape[1], got, ok, Floor)
		}
	}
	rejected := []struct {
		width, height int
		frameRate     float64
		peak          int
	}{
		{7680, 4320, 24, 0},
		{3840, 2160, 120, 0},
		{4096, 2304, 60, 0},
		{1920, 1080, 24, 300_000_001},
		{8704, 16, 24, 0},
	}
	for _, c := range rejected {
		if got, ok := Minimum(c.width, c.height, c.frameRate, c.peak); ok || got != 0 {
			t.Errorf("Minimum(%dx%d at %.0f, peak %d) = %d, %v; want no level at or below %d", c.width, c.height, c.frameRate, c.peak, got, ok, Ceiling)
		}
	}
	if got, ok := Minimum(4096, 2304, 30, 300_000_000); !ok || got != Ceiling {
		t.Errorf("Minimum(4096x2304 at 30, peak 300 Mb/s) = %d, %v; want the ceiling %d", got, ok, Ceiling)
	}
	if Floor != 30 || Ceiling != 52 {
		t.Fatalf("policy range = %d to %d, want 30 to 52", Floor, Ceiling)
	}
}

func TestMinimumRaisesLevelForPeakBitrate(t *testing.T) {
	cases := []struct {
		peak int
		want int
	}{{12_500_000, 30}, {12_500_001, 31}, {17_500_001, 32}, {25_000_001, 41}, {62_500_001, 50}, {168_750_001, 51}}
	for _, c := range cases {
		if got, ok := Minimum(640, 360, 24, c.peak); !ok || got != c.want {
			t.Errorf("Minimum(640x360, peak %d) = %d, %v; want %d", c.peak, got, ok, c.want)
		}
	}
}

func TestMaxCPBFollowsHighProfileTable(t *testing.T) {
	want := map[int]int{30: 12_500_000, 31: 17_500_000, 32: 25_000_000, 40: 31_250_000, 41: 78_125_000, 42: 78_125_000, 50: 168_750_000, 51: 300_000_000, 52: 300_000_000}
	for level, cpb := range want {
		if got := MaxCPB(level); got != cpb {
			t.Errorf("MaxCPB(%d) = %d, want %d", level, got, cpb)
		}
	}
	for _, level := range []int{0, 21, 22, 60, 62} {
		if got := MaxCPB(level); got != 0 {
			t.Errorf("MaxCPB(%d) = %d, want 0 for a level outside the policy", level, got)
		}
	}
}
