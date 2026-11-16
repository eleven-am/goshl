package live

import (
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	var config Config
	config.setDefaults()
	if config.MaxSessions != 16 || config.ProbeBudget != 10*time.Second || config.StallTimeout != 10*time.Second || config.ReconnectAttempts != 5 || config.ReconnectInitial != 500*time.Millisecond || config.ReconnectMax != 5*time.Second {
		t.Fatalf("defaults = %+v", config)
	}
}

func TestReconnectorBacksOffAndIsBounded(t *testing.T) {
	retry := newReconnector(500*time.Millisecond, 2*time.Second, 5)
	want := []time.Duration{0, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second}
	for index, expected := range want {
		delay, ok := retry.next(0)
		if !ok || delay != expected {
			t.Fatalf("attempt %d = %v %v, want %v", index+1, delay, ok, expected)
		}
	}
	if _, ok := retry.next(0); ok {
		t.Fatal("sixth attempt allowed")
	}
	retry.reset()
	if delay, ok := retry.next(100 * time.Millisecond); !ok || delay != 100*time.Millisecond {
		t.Fatalf("after reset = %v %v", delay, ok)
	}
}
