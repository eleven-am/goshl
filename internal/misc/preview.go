package misc

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"math"

	"github.com/disintegration/imaging"
	"github.com/eleven-am/goshl/internal/domain"
)

func (g *Generator) GetPreview(ctx context.Context, sourceURL, spriteKey, previewKey string, duration float64, config domain.PreviewConfig) ([]byte, error) {
	exists, err := g.storage.PreviewExists(ctx, previewKey)
	if err != nil {
		return nil, fmt.Errorf("check preview: %w", err)
	}

	if exists {
		return g.storage.ReadPreview(ctx, previewKey)
	}

	if err := g.runner().Once(ctx, &g.previews, previewKey, func(runCtx context.Context) (bool, error) {
		return g.storage.PreviewExists(runCtx, previewKey)
	}, func(runCtx context.Context) error {
		if err := g.ensureSprites(runCtx, sourceURL, spriteKey, duration); err != nil {
			return err
		}
		preview, err := g.generatePreview(runCtx, spriteKey, duration, config)
		if err != nil {
			return err
		}
		if err := g.storage.WritePreview(runCtx, previewKey, preview); err != nil {
			return fmt.Errorf("write preview: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return g.storage.ReadPreview(ctx, previewKey)
}

func (g *Generator) generatePreview(ctx context.Context, spriteKey string, duration float64, config domain.PreviewConfig) ([]byte, error) {
	thumbsPerSprite := g.cols * g.rows
	totalThumbs := int(math.Ceil(duration / g.interval))
	if totalThumbs <= 0 || config.MaxFrames <= 0 || config.Width <= 0 || config.Frameduration <= 0 {
		return nil, fmt.Errorf("no frames extracted from sprites")
	}
	frameCount := min(totalThumbs, config.MaxFrames)
	indices := make([]int, frameCount)
	for i := range indices {
		indices[i] = int(float64(i) * float64(totalThumbs) / float64(frameCount))
	}

	aspectRatio := float64(g.thumbHeight) / float64(g.thumbWidth)
	targetHeight := int(float64(config.Width) * aspectRatio)
	resizedFrames := make([]*image.Paletted, 0, frameCount)
	loadedSprite := -1
	var spriteImg image.Image
	for _, index := range indices {
		spriteIndex := index / thumbsPerSprite
		if spriteIndex != loadedSprite {
			spriteData, err := g.spriteBackend().ReadSprite(ctx, spriteKey, spriteIndex)
			if err != nil {
				return nil, fmt.Errorf("read sprite %d: %w", spriteIndex, err)
			}
			spriteImg, err = jpeg.Decode(bytes.NewReader(spriteData))
			if err != nil {
				return nil, fmt.Errorf("decode sprite %d: %w", spriteIndex, err)
			}
			loadedSprite = spriteIndex
		}
		frameIndex := index % thumbsPerSprite
		row := frameIndex / g.cols
		col := frameIndex % g.cols
		rect := image.Rect(col*g.thumbWidth, row*g.thumbHeight, (col+1)*g.thumbWidth, (row+1)*g.thumbHeight)
		frame := imaging.Crop(spriteImg, rect)
		resized := imaging.Resize(frame, config.Width, targetHeight, imaging.Lanczos)

		bounds := resized.Bounds()
		palettedImg := image.NewPaletted(bounds, palette.Plan9)
		draw.FloydSteinberg.Draw(palettedImg, bounds, resized, image.Point{})
		resizedFrames = append(resizedFrames, palettedImg)
	}

	delays := make([]int, len(resizedFrames))
	for i := range delays {
		delays[i] = max(1, config.Frameduration/10)
	}

	animatedGIF := &gif.GIF{
		Image: resizedFrames,
		Delay: delays,
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, animatedGIF); err != nil {
		return nil, fmt.Errorf("encode gif: %w", err)
	}

	return buf.Bytes(), nil
}
