package misc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/disintegration/imaging"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/mediaexec"
	"golang.org/x/sync/singleflight"
)

const (
	defaultThumbWidth  = 160
	defaultThumbHeight = 90
	defaultInterval    = 5.0
	defaultCols        = 10
	defaultRows        = 10
)

type Generator struct {
	storage     domain.Storage
	spriteStore domain.SpriteStorage
	hwAccel     *domain.HWAccelConfig
	sprites     singleflight.Group
	previews    singleflight.Group
	thumbnails  singleflight.Group
	subtitles   singleflight.Group
	coordinator domain.Coordinator
	observer    domain.Observer

	thumbWidth  int
	thumbHeight int
	interval    float64
	cols        int
	rows        int
}

func NewGeneratorWithCoordinator(storage domain.Storage, coordinator domain.Coordinator, hwAccel *domain.HWAccelConfig, observers ...domain.Observer) *Generator {
	generator := NewGenerator(storage, hwAccel)
	generator.coordinator = coordinator
	if len(observers) > 0 {
		generator.observer = observers[0]
	}
	return generator
}

func NewGenerator(storage domain.Storage, hwAccel ...*domain.HWAccelConfig) *Generator {
	cfg := domain.HWAccelConfig{Accelerator: domain.AccelNone}
	if len(hwAccel) > 0 && hwAccel[0] != nil {
		cfg = *hwAccel[0]
	}

	return &Generator{
		storage:     storage,
		spriteStore: storage,
		hwAccel:     &cfg,
		thumbWidth:  defaultThumbWidth,
		thumbHeight: defaultThumbHeight,
		interval:    defaultInterval,
		cols:        defaultCols,
		rows:        defaultRows,
	}
}

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

func (g *Generator) GetSpriteVTT(ctx context.Context, sourceURL, spriteKey string, duration float64, spriteURL func(int) string) ([]byte, error) {
	exists, err := g.spriteBackend().SpriteVTTExists(ctx, spriteKey)
	if err != nil {
		return nil, fmt.Errorf("check sprite vtt: %w", err)
	}

	if !exists {
		if err := g.ensureSprites(ctx, sourceURL, spriteKey, duration, spriteURL); err != nil {
			return nil, err
		}
	}

	return g.spriteBackend().ReadSpriteVTT(ctx, spriteKey)
}

func (g *Generator) GetSprite(ctx context.Context, sourceURL, spriteKey string, duration float64, spriteURL func(int) string, index int) ([]byte, error) {
	exists, err := g.spriteBackend().SpriteExists(ctx, spriteKey, index)
	if err != nil {
		return nil, fmt.Errorf("check sprite: %w", err)
	}

	if !exists {
		if err := g.ensureSprites(ctx, sourceURL, spriteKey, duration, spriteURL); err != nil {
			return nil, err
		}
	}

	return g.spriteBackend().ReadSprite(ctx, spriteKey, index)
}

func (g *Generator) ensureSprites(ctx context.Context, sourceURL, spriteKey string, duration float64, spriteURL func(int) string) error {
	return g.generateOnce(ctx, &g.sprites, spriteKey, func(runCtx context.Context) (bool, error) {
		return g.spritesReady(runCtx, spriteKey, duration)
	}, func(runCtx context.Context) error {
		return g.generateSprites(runCtx, sourceURL, spriteKey, duration, spriteURL)
	})
}

func (g *Generator) spritesReady(ctx context.Context, spriteKey string, duration float64) (bool, error) {
	vttExists, err := g.spriteBackend().SpriteVTTExists(ctx, spriteKey)
	if err != nil {
		return false, fmt.Errorf("check sprite vtt: %w", err)
	}
	if !vttExists {
		return false, nil
	}

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

func (g *Generator) generateSprites(ctx context.Context, sourceURL, spriteKey string, duration float64, spriteURL func(int) string) error {
	numSprites := g.spriteCount(duration)

	tmpDir, err := os.MkdirTemp("", "sprites-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outputPattern := filepath.Join(tmpDir, "sprite-%d.jpg")

	args := g.spriteArgs(sourceURL, outputPattern, duration)

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

	vtt := g.generateVTT(duration, numSprites, spriteURL)
	if err := g.spriteBackend().WriteSpriteVTT(ctx, spriteKey, vtt); err != nil {
		return fmt.Errorf("write sprite vtt: %w", err)
	}

	return nil
}

func (g *Generator) spriteArgs(sourceURL string, outputPattern string, duration float64) []string {
	args := []string{"-nostats", "-hide_banner", "-loglevel", "warning"}
	if g.hwAccel != nil && g.hwAccel.Accelerator == domain.AccelCUDA {
		args = append(args, "-threads", "1")
	}
	if g.hwAccel != nil {
		args = append(args, g.hwAccel.DecodeFlags...)
	}
	args = append(args,
		"-skip_frame", "nokey",
		"-i", sourceURL,
		"-vf", g.spriteFilter(duration),
		"-q:v", "5",
		outputPattern,
	)
	return args
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

				buf.WriteString(fmt.Sprintf("%s --> %s\n", formatVTTTime(startTime), formatVTTTime(endTime)))
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

func (g *Generator) GetSubtitles(ctx context.Context, sourceURL, subtitleKey string, streamIndex int, trackID string) ([]byte, error) {
	exists, err := g.storage.SubtitleVTTExists(ctx, subtitleKey, trackID)
	if err != nil {
		return nil, fmt.Errorf("check subtitle vtt: %w", err)
	}

	if !exists {
		if err := g.generateOnce(ctx, &g.subtitles, subtitleKey, func(runCtx context.Context) (bool, error) {
			return g.storage.SubtitleVTTExists(runCtx, subtitleKey, trackID)
		}, func(runCtx context.Context) error {
			return g.extractSubtitles(runCtx, sourceURL, subtitleKey, streamIndex, trackID)
		}); err != nil {
			return nil, err
		}
	}

	return g.storage.ReadSubtitleVTT(ctx, subtitleKey, trackID)
}

func (g *Generator) extractSubtitles(ctx context.Context, sourceURL, subtitleKey string, streamIndex int, trackID string) error {
	args := []string{
		"-i", sourceURL,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-c:s", "webvtt",
		"-f", "webvtt",
		"pipe:1",
	}

	output, err := mediaexec.Output(ctx, "ffmpeg", args...)
	if err != nil {
		return fmt.Errorf("ffmpeg subtitle extraction: %w", err)
	}

	if err := g.storage.WriteSubtitleVTT(ctx, subtitleKey, trackID, output); err != nil {
		return fmt.Errorf("write subtitle vtt: %w", err)
	}

	return nil
}

func formatVTTTime(seconds float64) string {
	hours := int(seconds) / 3600
	minutes := (int(seconds) % 3600) / 60
	secs := int(seconds) % 60
	millis := int((seconds - float64(int(seconds))) * 1000)

	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, secs, millis)
}

func (g *Generator) GetPreview(ctx context.Context, sourceURL, spriteKey, previewKey string, duration float64, spriteURL func(int) string, config domain.PreviewConfig) ([]byte, error) {
	exists, err := g.storage.PreviewExists(ctx, previewKey)
	if err != nil {
		return nil, fmt.Errorf("check preview: %w", err)
	}

	if exists {
		return g.storage.ReadPreview(ctx, previewKey)
	}

	if err := g.generateOnce(ctx, &g.previews, previewKey, func(runCtx context.Context) (bool, error) {
		return g.storage.PreviewExists(runCtx, previewKey)
	}, func(runCtx context.Context) error {
		if err := g.ensureSprites(runCtx, sourceURL, spriteKey, duration, spriteURL); err != nil {
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

func (g *Generator) GetThumbnail(ctx context.Context, sourceURL, thumbnailKey string, duration float64, config domain.ThumbnailConfig) ([]byte, error) {
	exists, err := g.storage.ThumbnailExists(ctx, thumbnailKey)
	if err != nil {
		return nil, fmt.Errorf("check thumbnail: %w", err)
	}

	if exists {
		return g.storage.ReadThumbnail(ctx, thumbnailKey)
	}

	if err := g.generateOnce(ctx, &g.thumbnails, thumbnailKey, func(runCtx context.Context) (bool, error) {
		return g.storage.ThumbnailExists(runCtx, thumbnailKey)
	}, func(runCtx context.Context) error {
		thumbnail, err := mediaexec.Output(runCtx, "ffmpeg", g.thumbnailArgs(sourceURL, duration, config)...)
		if err != nil {
			return err
		}
		if err := g.storage.WriteThumbnail(runCtx, thumbnailKey, thumbnail); err != nil {
			return fmt.Errorf("write thumbnail: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return g.storage.ReadThumbnail(ctx, thumbnailKey)
}

func (g *Generator) thumbnailArgs(sourceURL string, duration float64, config domain.ThumbnailConfig) []string {
	timestamp := duration * config.Position
	if timestamp < 0 {
		timestamp = 0
	}
	if timestamp >= duration {
		timestamp = duration - g.interval
		if timestamp < 0 {
			timestamp = 0
		}
	}

	return []string{
		"-nostats", "-hide_banner", "-loglevel", "warning",
		"-ss", fmt.Sprintf("%.6f", timestamp),
		"-i", sourceURL,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", config.Width),
		"-q:v", "2",
		"-f", "image2",
		"pipe:1",
	}
}

func (g *Generator) generateOnce(ctx context.Context, group *singleflight.Group, key string, ready func(context.Context) (bool, error), generate func(context.Context) error) error {
	result := group.DoChan(key, func() (any, error) {
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return nil, g.coordinateGeneration(runCtx, key, ready, generate)
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outcome := <-result:
		return outcome.Err
	}
}

func (g *Generator) coordinateGeneration(ctx context.Context, key string, ready func(context.Context) (bool, error), generate func(context.Context) error) error {
	if g.coordinator == nil {
		isReady, err := ready(ctx)
		if err != nil || isReady {
			return err
		}
		return generate(ctx)
	}
	lockKey := "asset:" + key
	token, err := g.coordinator.AcquireLock(ctx, lockKey, 11*time.Minute)
	if err != nil {
		if !errors.Is(err, domain.ErrLockHeld) {
			return fmt.Errorf("acquire asset lock: %w", err)
		}
		g.observe(ctx, domain.Event{Type: domain.EventLockWait, MediaKey: key})
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				isReady, readyErr := ready(ctx)
				if readyErr != nil {
					return readyErr
				}
				if isReady {
					return nil
				}
			}
		}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if releaseErr := g.coordinator.ReleaseLock(cleanup, lockKey, token); releaseErr != nil {
			g.observe(cleanup, domain.Event{Type: domain.EventLockReleaseFailed, MediaKey: key, Error: releaseErr.Error()})
		}
	}()
	isReady, err := ready(ctx)
	if err != nil || isReady {
		return err
	}
	return generate(ctx)
}

func (g *Generator) observe(ctx context.Context, event domain.Event) {
	if g.observer == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	g.observer.Observe(ctx, event)
}
