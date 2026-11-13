package probe

import (
	"testing"

	"github.com/eleven-am/goshl/internal/domain"
)

func TestValidateMetadataRejectsImplausibleDuration(t *testing.T) {
	metadata := func(duration float64) *domain.Metadata {
		return &domain.Metadata{Duration: duration, Video: domain.VideoStream{Codec: "h264", Width: 64, Height: 36}}
	}
	if err := validateMetadata(metadata(100 * 3600)); err != nil {
		t.Fatalf("a 100 hour source must stay playable: %v", err)
	}
	for _, duration := range []float64{100*3600 + 1, 80_000_000, 2e9, 1e300} {
		if err := validateMetadata(metadata(duration)); err == nil {
			t.Errorf("duration %v was accepted", duration)
		}
	}
}
