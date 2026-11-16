package engine

import (
	"errors"
	"testing"
	"time"
)

func TestOptionsSetDefaults(t *testing.T) {
	opts := Config{}
	opts.setDefaults()

	if opts.SegmentTimeout != 30*time.Second {
		t.Errorf("expected SegmentTimeout 30s, got %v", opts.SegmentTimeout)
	}
	if opts.TargetDuration != 6.0 {
		t.Errorf("expected TargetDuration 6.0, got %v", opts.TargetDuration)
	}
	if opts.SegmentsPerJob != 4 {
		t.Errorf("expected SegmentsPerJob 4, got %v", opts.SegmentsPerJob)
	}
	if opts.VideoPoolSize != 2 {
		t.Errorf("expected VideoPoolSize 2, got %v", opts.VideoPoolSize)
	}
	if opts.AudioPoolSize != 4 {
		t.Errorf("expected AudioPoolSize 4, got %v", opts.AudioPoolSize)
	}
	if opts.DefaultPreviewConfig.MaxFrames != 40 {
		t.Errorf("expected MaxFrames 40, got %v", opts.DefaultPreviewConfig.MaxFrames)
	}
	if opts.DefaultPreviewConfig.Frameduration != 250 {
		t.Errorf("expected Frameduration 250, got %v", opts.DefaultPreviewConfig.Frameduration)
	}
	if opts.DefaultPreviewConfig.Width != 480 {
		t.Errorf("expected Width 480, got %v", opts.DefaultPreviewConfig.Width)
	}
}

func TestOptionsValidateRejectsMissingStorage(t *testing.T) {
	opts := Config{Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}}
	if err := opts.validateError(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("missing Storage: got %v, want ErrInvalidOptions", err)
	}
}

func TestOptionsValidateRejectsMissingCoordinator(t *testing.T) {
	opts := Config{Storage: &stubStorage{}, PathGen: stubPathGen{}}
	if err := opts.validateError(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("missing Coordinator: got %v, want ErrInvalidOptions", err)
	}
}

func TestOptionsValidateRejectsMissingPathGen(t *testing.T) {
	opts := Config{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}}
	if err := opts.validateError(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("missing PathGen: got %v, want ErrInvalidOptions", err)
	}
}

func TestProbeTimeoutDefaultsToJobTimeout(t *testing.T) {
	opts := Config{JobTimeout: 5 * time.Minute}
	opts.setDefaults()
	if opts.ProbeTimeout != 5*time.Minute {
		t.Fatalf("expected ProbeTimeout to follow JobTimeout, got %v", opts.ProbeTimeout)
	}

	defaults := Config{}
	defaults.setDefaults()
	if defaults.ProbeTimeout != 2*time.Minute || defaults.JobTimeout != 2*time.Minute {
		t.Fatalf("unexpected defaults probe=%v job=%v", defaults.ProbeTimeout, defaults.JobTimeout)
	}
}

func TestProbeTimeoutKeepsExplicitValueAndLeavesJobTimeout(t *testing.T) {
	opts := Config{JobTimeout: time.Minute, ProbeTimeout: 10 * time.Minute}
	opts.setDefaults()
	if opts.ProbeTimeout != 10*time.Minute || opts.JobTimeout != time.Minute {
		t.Fatalf("unexpected timeouts probe=%v job=%v", opts.ProbeTimeout, opts.JobTimeout)
	}
}

func TestOptionsRejectNegativeProbeTimeout(t *testing.T) {
	opts := Config{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, ProbeTimeout: -time.Second}
	if err := opts.validateError(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected invalid options, got %v", err)
	}
}

func TestLiveAndChannelDefaults(t *testing.T) {
	opts := Config{}
	opts.setDefaults()
	if opts.LiveMaxSessions != 16 || opts.LiveMaxTranscodes != 2 {
		t.Errorf("live defaults = %d sessions, %d transcodes", opts.LiveMaxSessions, opts.LiveMaxTranscodes)
	}
	if opts.EncoderSlots != opts.VideoPoolSize+opts.LiveMaxTranscodes {
		t.Errorf("EncoderSlots = %d, want VideoPoolSize + LiveMaxTranscodes", opts.EncoderSlots)
	}
	if opts.ChannelPoolSize != 0 {
		t.Errorf("channels must be off by default, got pool size %d", opts.ChannelPoolSize)
	}
}

func TestOptionsRejectUnusableLiveBudgets(t *testing.T) {
	cases := map[string]Config{
		"negative slots":         {EncoderSlots: -1},
		"negative sessions":      {LiveMaxSessions: -1},
		"negative transcodes":    {LiveMaxTranscodes: -1},
		"transcodes over slots":  {EncoderSlots: 2, LiveMaxTranscodes: 3},
		"negative channel pool":  {ChannelPoolSize: -1},
		"channel batch too long": {ChannelSegmentsPerJob: maxSegmentsPerJob + 1},
	}
	for name, opts := range cases {
		opts.Storage, opts.Coordinator, opts.PathGen = &stubStorage{}, &stubCoordinator{}, stubPathGen{}
		err := opts.validateError()
		if err == nil {
			opts.setDefaults()
			err = opts.validateResolvedError()
		}
		if !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("%s: got %v, want ErrInvalidOptions", name, err)
		}
	}
}
