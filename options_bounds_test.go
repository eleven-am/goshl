package goshl

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestOptionsRejectDegenerateSizes(t *testing.T) {
	cases := map[string]func(*Options){
		"TargetDuration 1e-9":     func(o *Options) { o.TargetDuration = 1e-9 },
		"TargetDuration 0.5":      func(o *Options) { o.TargetDuration = 0.5 },
		"TargetDuration 61":       func(o *Options) { o.TargetDuration = 61 },
		"TargetDuration MaxFloat": func(o *Options) { o.TargetDuration = math.MaxFloat64 },
		"SegmentsPerJob 1001":     func(o *Options) { o.SegmentsPerJob = 1001 },
		"VideoPoolSize 1025":      func(o *Options) { o.VideoPoolSize = 1025 },
		"AudioPoolSize 1e9":       func(o *Options) { o.AudioPoolSize = 1_000_000_000 },
	}
	for name, mutate := range cases {
		opts := Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}}
		mutate(&opts)
		controller, err := NewControllerWithContext(context.Background(), opts)
		if controller != nil {
			controller.Stop()
		}
		if !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("%s: err = %v, want ErrInvalidOptions", name, err)
		}
	}
	for name, mutate := range map[string]func(*Options){
		"TargetDuration 1":    func(o *Options) { o.TargetDuration = 1 },
		"TargetDuration 60":   func(o *Options) { o.TargetDuration = 60 },
		"SegmentsPerJob 1000": func(o *Options) { o.SegmentsPerJob = 1000 },
		"VideoPoolSize 1024":  func(o *Options) { o.VideoPoolSize = 1024 },
	} {
		opts := Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}}
		mutate(&opts)
		controller, err := NewControllerWithContext(context.Background(), opts)
		if err != nil {
			t.Errorf("%s: rejected: %v", name, err)
			continue
		}
		controller.Stop()
	}
}

func TestUnsupportedSourceMatchesPublicSentinel(t *testing.T) {
	controller, err := NewControllerWithContext(context.Background(), Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}})
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	for _, sourceURL := range []string{"", "rtmp://host/live", "concat:a|b", "http://", "file://remote/x.mp4"} {
		if _, err := controller.ProbeMetadata(context.Background(), sourceURL); !errors.Is(err, ErrUnsupportedSource) {
			t.Errorf("ProbeMetadata(%q) = %v, want ErrUnsupportedSource", sourceURL, err)
		}
	}
}
