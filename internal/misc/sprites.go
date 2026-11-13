package misc

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/mediaexec"
	"github.com/eleven-am/goshl/internal/source"
	"github.com/eleven-am/goshl/internal/webvtt"
)

func (g *Generator) SetSpriteStorage(store domain.SpriteStorage) {
	if store != nil {
		g.spriteStore = store
	}
}

func (g *Generator) spriteBackend() domain.SpriteStorage {
	if g.spriteStore != nil {
		return g.spriteStore
	}
	return g.storage
}

func (g *Generator) SpriteIdentity() string {
	return fmt.Sprintf("%dx%d:%g:%dx%d", g.thumbWidth, g.thumbHeight, g.interval, g.cols, g.rows)
}

func (g *Generator) GetSpriteVTT(ctx context.Context, sourceURL, spriteKey string, duration float64, spriteURL func(int) string) ([]byte, error) {
	ready, err := g.spritesReady(ctx, spriteKey, duration)
	if err != nil {
		return nil, err
	}

	if !ready {
		if err := g.ensureSprites(ctx, sourceURL, spriteKey, duration); err != nil {
			return nil, err
		}
	}

	return g.generateVTT(duration, g.spriteCount(duration), spriteURL), nil
}

func (g *Generator) GetSprite(ctx context.Context, sourceURL, spriteKey string, duration float64, index int) ([]byte, error) {
	exists, err := g.spriteBackend().SpriteExists(ctx, spriteKey, index)
	if err != nil {
		return nil, fmt.Errorf("check sprite: %w", err)
	}

	if !exists {
		if err := g.ensureSprites(ctx, sourceURL, spriteKey, duration); err != nil {
			return nil, err
		}
	}

	return g.spriteBackend().ReadSprite(ctx, spriteKey, index)
}

func (g *Generator) ensureSprites(ctx context.Context, sourceURL, spriteKey string, duration float64) error {
	return g.runner().Once(ctx, &g.sprites, spriteKey, func(runCtx context.Context) (bool, error) {
		return g.spritesReady(runCtx, spriteKey, duration)
	}, func(runCtx context.Context) error {
		return g.generateSprites(runCtx, sourceURL, spriteKey, duration)
	})
}

func (g *Generator) spritesReady(ctx context.Context, spriteKey string, duration float64) (bool, error) {
	for i := 0; i < g.spriteCount(duration); i++ {
		exists, err := g.spriteBackend().SpriteExists(ctx, spriteKey, i)
		if err != nil {
			return false, fmt.Errorf("check sprite %d: %w", i, err)
		}
		if !exists {
			return false, nil
		}
	}

	return true, nil
}

func (g *Generator) generateSprites(ctx context.Context, sourceURL, spriteKey string, duration float64) error {
	numSprites := g.spriteCount(duration)

	tmpDir, err := os.MkdirTemp("", "sprites-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outputPattern := filepath.Join(tmpDir, "sprite-%d.jpg")

	args, err := g.spriteArgs(sourceURL, outputPattern, duration)
	if err != nil {
		return err
	}

	if err := mediaexec.Run(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("ffmpeg sprite generation: %w", err)
	}

	for i := 0; i < numSprites; i++ {
		spritePath := filepath.Join(tmpDir, fmt.Sprintf("sprite-%d.jpg", i+1))
		data, err := os.ReadFile(spritePath)
		if err != nil {
			return fmt.Errorf("read sprite %d: %w", i, err)
		}

		if err := g.spriteBackend().WriteSprite(ctx, spriteKey, i, data); err != nil {
			return fmt.Errorf("write sprite %d: %w", i, err)
		}
	}

	return nil
}

func (g *Generator) spriteArgs(sourceURL string, outputPattern string, duration float64) ([]string, error) {
	input, err := source.InputArgs(sourceURL)
	if err != nil {
		return nil, err
	}
	args := []string{"-nostats", "-hide_banner", "-loglevel", "warning"}
	if g.hwAccel != nil && g.hwAccel.Accelerator == domain.AccelCUDA {
		args = append(args, "-threads", "1")
	}
	if g.hwAccel != nil {
		args = append(args, g.hwAccel.DecodeFlags...)
	}
	args = append(args, "-skip_frame", "nokey")
	args = append(args, input...)
	args = append(args,
		"-vf", g.spriteFilter(duration),
		"-q:v", "5",
		outputPattern,
	)
	return args, nil
}

func (g *Generator) spriteCount(duration float64) int {
	thumbsPerSprite := g.cols * g.rows
	totalThumbs := int(math.Ceil(duration / g.interval))
	return int(math.Ceil(float64(totalThumbs) / float64(thumbsPerSprite)))
}

func (g *Generator) spriteFilter(duration float64) string {
	pad := fmt.Sprintf("tpad=stop_mode=clone:stop_duration=%g", math.Ceil(duration)+g.interval)
	limit := fmt.Sprintf("trim=end_frame=%d", int(math.Ceil(duration/g.interval)))
	filter := fmt.Sprintf("scale=%d:%d,%s,fps=1/%g,%s,tile=%dx%d", g.thumbWidth, g.thumbHeight, pad, g.interval, limit, g.cols, g.rows)
	if g.hwAccel == nil {
		return filter
	}

	switch g.hwAccel.Accelerator {
	case domain.AccelCUDA, domain.AccelQSV, domain.AccelVAAPI:
		if !g.hwAccel.DeviceFrames {
			return filter
		}
		scale := fmt.Sprintf(g.hwAccel.ScaleFilter, g.thumbWidth, g.thumbHeight)
		return fmt.Sprintf("%s,hwdownload,format=nv12,%s,fps=1/%g,%s,tile=%dx%d", scale, pad, g.interval, limit, g.cols, g.rows)
	default:
		return filter
	}
}

func (g *Generator) generateVTT(duration float64, numSprites int, spriteURL func(int) string) []byte {
	var buf bytes.Buffer
	buf.WriteString("WEBVTT\n\n")

	currentTime := 0.0

	for spriteIndex := 0; spriteIndex < numSprites; spriteIndex++ {
		url := spriteURL(spriteIndex)

		for row := 0; row < g.rows; row++ {
			for col := 0; col < g.cols; col++ {
				if currentTime >= duration {
					break
				}

				startTime := currentTime
				endTime := math.Min(currentTime+g.interval, duration)

				x := col * g.thumbWidth
				y := row * g.thumbHeight

				buf.WriteString(fmt.Sprintf("%s --> %s\n", webvtt.Timestamp(startTime), webvtt.Timestamp(endTime)))
				buf.WriteString(fmt.Sprintf("%s#xywh=%d,%d,%d,%d\n\n", url, x, y, g.thumbWidth, g.thumbHeight))

				currentTime += g.interval
			}
			if currentTime >= duration {
				break
			}
		}
	}

	return buf.Bytes()
}
