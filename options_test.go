package goshl

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eleven-am/goshl/internal/engine"
)

func TestNewControllerWithContextRejectsInvalidOptions(t *testing.T) {
	_, err := NewControllerWithContext(context.Background(), Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, SegmentsPerJob: -1})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected invalid options error, got %v", err)
	}
}

func installVersionedBinaries(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		script := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo \"" + name + " version " + version + "\"; fi\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func versionTestOptions() Options {
	return Options{
		Storage:     &stubStorage{},
		Coordinator: &stubCoordinator{},
		PathGen:     stubPathGen{},
	}
}

func TestStartupRejectsOldFFmpeg(t *testing.T) {
	installVersionedBinaries(t, "6.0")
	controller, err := NewControllerWithContext(context.Background(), versionTestOptions())
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("err = %v, want ErrInvalidOptions", err)
	}
	if controller != nil {
		t.Fatal("controller must be nil on error")
	}
}

func TestStartupAcceptsSupportedFFmpeg(t *testing.T) {
	installVersionedBinaries(t, "6.1")
	controller, err := NewControllerWithContext(context.Background(), versionTestOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = controller
}

func TestUnknownHWAccelModeIsInvalidOptions(t *testing.T) {
	_, err := NewControllerWithContext(context.Background(), Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, HWAccelMode: Accelerator("quantum")})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("unknown accelerator must wrap ErrInvalidOptions, got %v", err)
	}
	if !strings.Contains(err.Error(), "quantum") {
		t.Fatalf("error must name the accelerator, got %v", err)
	}
}

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

func TestOptionsAndRequestsRejectSameInvalidConfigs(t *testing.T) {
	baseOptions := func() Options {
		return Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}}
	}

	previewCases := map[string]PreviewConfig{
		"negative max frames":        {MaxFrames: -1},
		"negative frame duration":    {Frameduration: -1},
		"negative width":             {Width: -1},
		"max frames above limit":     {MaxFrames: 1001},
		"frame duration above limit": {Frameduration: 60_001},
		"width above limit":          {Width: 4097},
	}
	for name, config := range previewCases {
		t.Run("preview/"+name, func(t *testing.T) {
			opts := baseOptions()
			opts.DefaultPreviewConfig = config
			_, optionsErr := NewControllerWithContext(context.Background(), opts)
			if !errors.Is(optionsErr, ErrInvalidOptions) || !errors.Is(optionsErr, ErrInvalidPreviewConfig) {
				t.Fatalf("options error = %v, want ErrInvalidOptions and ErrInvalidPreviewConfig", optionsErr)
			}

			controller, err := NewControllerWithContext(context.Background(), baseOptions())
			if err != nil {
				t.Fatalf("new controller: %v", err)
			}
			cfg := config
			if _, requestErr := controller.Preview(context.Background(), "file:///missing.mp4", &cfg); !errors.Is(requestErr, ErrInvalidPreviewConfig) {
				t.Fatalf("request error = %v, want ErrInvalidPreviewConfig", requestErr)
			}
		})
	}

	thumbnailCases := map[string]ThumbnailConfig{
		"negative position":  {Position: -0.1},
		"position above one": {Position: 1.1},
		"nan position":       {Position: math.NaN()},
		"infinite position":  {Position: math.Inf(1)},
		"negative width":     {Width: -1},
		"width above limit":  {Width: 8193},
	}
	for name, config := range thumbnailCases {
		t.Run("thumbnail/"+name, func(t *testing.T) {
			opts := baseOptions()
			opts.DefaultThumbnailConfig = config
			_, optionsErr := NewControllerWithContext(context.Background(), opts)
			if !errors.Is(optionsErr, ErrInvalidOptions) || !errors.Is(optionsErr, ErrInvalidThumbnailConfig) {
				t.Fatalf("options error = %v, want ErrInvalidOptions and ErrInvalidThumbnailConfig", optionsErr)
			}

			controller, err := NewControllerWithContext(context.Background(), baseOptions())
			if err != nil {
				t.Fatalf("new controller: %v", err)
			}
			cfg := config
			if _, requestErr := controller.Thumbnail(context.Background(), "file:///missing.mp4", &cfg); !errors.Is(requestErr, ErrInvalidThumbnailConfig) {
				t.Fatalf("request error = %v, want ErrInvalidThumbnailConfig", requestErr)
			}
		})
	}
}

func TestOptionsMapOntoEngineConfig(t *testing.T) {
	optionsType := reflect.TypeOf(Options{})
	configType := reflect.TypeOf(engine.Config{})
	if optionsType.NumField() != configType.NumField() {
		t.Fatalf("Options has %d fields, engine.Config has %d", optionsType.NumField(), configType.NumField())
	}
	opts := Options{
		Storage:                &stubStorage{},
		Coordinator:            &stubCoordinator{},
		PathGen:                stubPathGen{},
		HWAccel:                true,
		HWAccelMode:            AccelVAAPI,
		SegmentTimeout:         3 * time.Second,
		TargetDuration:         4,
		SegmentsPerJob:         5,
		VideoPoolSize:          6,
		AudioPoolSize:          7,
		DefaultPreviewConfig:   PreviewConfig{MaxFrames: 8, Frameduration: 9, Width: 10},
		DefaultThumbnailConfig: ThumbnailConfig{Position: 0.5, Width: 11},
		JobTimeout:             12 * time.Second,
		ProbeTimeout:           13 * time.Second,
		GenerationTimeout:      14 * time.Second,
		SpriteStorage:          &stubStorage{},
		CacheVersion:           "version",
		SourceRevision:         func(context.Context, string) (string, error) { return "revision", nil },
		Observer:               &hardwareEventRecorder{},
		HWAccelDecodeMode:      DecodeDevice,
		AllowHardwareFallback:  true,
		OmitSubtitleRenditions: true,
	}
	given := reflect.ValueOf(opts)
	mapped := reflect.ValueOf(opts.config())
	for index := 0; index < optionsType.NumField(); index++ {
		field := optionsType.Field(index)
		if configType.Field(index).Name != field.Name {
			t.Fatalf("field %d is Options.%s but engine.Config.%s", index, field.Name, configType.Field(index).Name)
		}
		if given.Field(index).IsZero() {
			t.Fatalf("the test must set Options.%s", field.Name)
		}
		if field.Name == "SourceRevision" {
			revision, err := opts.config().SourceRevision(context.Background(), "source")
			if err != nil || revision != "revision" {
				t.Errorf("SourceRevision was not carried over: %q, %v", revision, err)
			}
			continue
		}
		if !reflect.DeepEqual(given.Field(index).Interface(), mapped.Field(index).Interface()) {
			t.Errorf("Options.%s = %v, engine.Config.%s = %v", field.Name, given.Field(index).Interface(), field.Name, mapped.Field(index).Interface())
		}
	}
	if (Options{}).config().SourceRevision != nil {
		t.Errorf("an unset SourceRevision must stay nil")
	}
}

func TestPublicSentinelsAreTheEngineSentinels(t *testing.T) {
	pairs := map[string][2]error{
		"ErrInvalidOptions":               {ErrInvalidOptions, engine.ErrInvalidOptions},
		"ErrInvalidStreamType":            {ErrInvalidStreamType, engine.ErrInvalidStreamType},
		"ErrRenditionNotFound":            {ErrRenditionNotFound, engine.ErrRenditionNotFound},
		"ErrSegmentOutOfRange":            {ErrSegmentOutOfRange, engine.ErrSegmentOutOfRange},
		"ErrSegmentTimeout":               {ErrSegmentTimeout, engine.ErrSegmentTimeout},
		"ErrMetadataTimeout":              {ErrMetadataTimeout, engine.ErrMetadataTimeout},
		"ErrCacheInvalidationUnsupported": {ErrCacheInvalidationUnsupported, engine.ErrCacheInvalidationUnsupported},
		"ErrInvalidPreviewConfig":         {ErrInvalidPreviewConfig, engine.ErrInvalidPreviewConfig},
		"ErrInvalidThumbnailConfig":       {ErrInvalidThumbnailConfig, engine.ErrInvalidThumbnailConfig},
		"ErrEnqueueRejected":              {ErrEnqueueRejected, engine.ErrEnqueueRejected},
		"ErrTonemapUnavailable":           {ErrTonemapUnavailable, engine.ErrTonemapUnavailable},
		"ErrUnsupportedSource":            {ErrUnsupportedSource, engine.ErrUnsupportedSource},
		"ErrControllerStopped":            {ErrControllerStopped, engine.ErrControllerStopped},
	}
	messages := map[string]string{
		"ErrInvalidOptions":               "invalid goshl options",
		"ErrInvalidStreamType":            "invalid stream type",
		"ErrRenditionNotFound":            "rendition not found",
		"ErrSegmentOutOfRange":            "segment index out of range",
		"ErrSegmentTimeout":               "segment wait timed out",
		"ErrMetadataTimeout":              "metadata wait timed out",
		"ErrCacheInvalidationUnsupported": "storage does not support cache invalidation",
		"ErrInvalidPreviewConfig":         "invalid preview configuration",
		"ErrInvalidThumbnailConfig":       "invalid thumbnail configuration",
	}
	for name, pair := range pairs {
		if pair[0] == nil || pair[0] != pair[1] {
			t.Errorf("%s is %v publicly and %v in the engine", name, pair[0], pair[1])
		}
		if want, pinned := messages[name]; pinned && pair[0].Error() != want {
			t.Errorf("%s reads %q, want %q", name, pair[0].Error(), want)
		}
	}
}
