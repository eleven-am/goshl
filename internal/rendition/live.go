package rendition

import (
	"fmt"
	"math"

	"github.com/eleven-am/goshl/internal/domain"
)

func Live(video domain.VideoStream, displayWidth, maxHeight int) (domain.VideoRendition, error) {
	if video.Height <= 0 || displayWidth <= 0 || maxHeight <= 0 {
		return domain.VideoRendition{}, fmt.Errorf("live source has no usable dimensions (%dx%d display %d, max height %d)", video.Width, video.Height, displayWidth, maxHeight)
	}
	height := evenDown(min(video.Height, maxHeight))
	width := evenDown(int(math.Round(float64(displayWidth) * float64(height) / float64(video.Height))))
	bitrate := classFor(min(width, height)).estimateBitrate
	rendition, err := transcodedRendition(width, height, bitrate, video.FrameRate)
	if err != nil {
		return domain.VideoRendition{}, err
	}
	rendition.Colour = domain.PlanColour(video)
	return rendition, nil
}
