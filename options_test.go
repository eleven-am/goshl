package goshl

import (
	"context"
	"errors"
	"math"
	"testing"
)

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
