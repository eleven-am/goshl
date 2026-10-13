package h264level

import "testing"

func TestMinimumAccountsForScaledFrameSizeAndRate(t *testing.T) {
	if got := Minimum(864, 360, 23.976, 622_713); got != 30 {
		t.Fatalf("Minimum() = %d, want 30", got)
	}
}

func TestMinimumUsesAConservativeRateWhenUnknown(t *testing.T) {
	if got := Minimum(1920, 1080, 0, 7_500_000); got != 42 {
		t.Fatalf("Minimum() = %d, want 42", got)
	}
}
