package goshl

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUnknownHWAccelModeIsInvalidOptions(t *testing.T) {
	_, err := NewControllerWithContext(context.Background(), Options{Storage: &stubStorage{}, Coordinator: &stubCoordinator{}, PathGen: stubPathGen{}, HWAccelMode: Accelerator("quantum")})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("unknown accelerator must wrap ErrInvalidOptions, got %v", err)
	}
	if !strings.Contains(err.Error(), "quantum") {
		t.Fatalf("error must name the accelerator, got %v", err)
	}
}
