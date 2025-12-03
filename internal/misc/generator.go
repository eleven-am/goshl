package misc

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/eleven-am/goshl/internal/domain"
)

const (
	defaultThumbWidth  = 160
	defaultThumbHeight = 90
	defaultInterval    = 5.0
	defaultCols        = 10
	defaultRows        = 10
)

type Generator struct {
	storage domain.Storage

	thumbWidth  int
	thumbHeight int
	interval    float64
	cols        int
	rows        int
}

func NewGenerator(storage domain.Storage) *Generator {
	return &Generator{
		storage:     storage,
		thumbWidth:  defaultThumbWidth,
		thumbHeight: defaultThumbHeight,
		interval:    defaultInterval,
		cols:        defaultCols,
		rows:        defaultRows,
	}
}

func (g *Generator) GetSpriteVTT(ctx context.Context, sourceURL string, duration float64, urlPattern string) ([]byte, error) {
	exists, err := g.storage.SpriteVTTExists(ctx, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("check sprite vtt: %w", err)
	}

	if !exists {
		if err := g.generateSprites(ctx, sourceURL, duration, urlPattern); err != nil {
			return nil, err
		}
	}

	return g.storage.ReadSpriteVTT(ctx, sourceURL)
}

func (g *Generator) GetSprite(ctx context.Context, sourceURL string, duration float64, urlPattern string, index int) ([]byte, error) {
	exists, err := g.storage.SpriteExists(ctx, sourceURL, index)
	if err != nil {
		return nil, fmt.Errorf("check sprite: %w", err)
	}

	if !exists {
		if err := g.generateSprites(ctx, sourceURL, duration, urlPattern); err != nil {
			return nil, err
		}
	}

	return g.storage.ReadSprite(ctx, sourceURL, index)
}

func (g *Generator) generateSprites(ctx context.Context, sourceURL string, duration float64, urlPattern string) error {
	thumbsPerSprite := g.cols * g.rows
	totalThumbs := int(math.Ceil(duration / g.interval))
	numSprites := int(math.Ceil(float64(totalThumbs) / float64(thumbsPerSprite)))

	tmpDir, err := os.MkdirTemp("", "sprites-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outputPattern := filepath.Join(tmpDir, "sprite-%d.jpg")

	args := []string{
		"-i", sourceURL,
		"-vf", fmt.Sprintf("fps=1/%g,scale=%d:%d,tile=%dx%d", g.interval, g.thumbWidth, g.thumbHeight, g.cols, g.rows),
		"-q:v", "5",
		outputPattern,
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg sprite generation: %w", err)
	}

	for i := 0; i < numSprites; i++ {
		spritePath := filepath.Join(tmpDir, fmt.Sprintf("sprite-%d.jpg", i+1))
		data, err := os.ReadFile(spritePath)
		if err != nil {
			return fmt.Errorf("read sprite %d: %w", i, err)
		}

		if err := g.storage.WriteSprite(ctx, sourceURL, i, data); err != nil {
			return fmt.Errorf("write sprite %d: %w", i, err)
		}
	}

	vtt := g.generateVTT(duration, numSprites, urlPattern)
	if err := g.storage.WriteSpriteVTT(ctx, sourceURL, vtt); err != nil {
		return fmt.Errorf("write sprite vtt: %w", err)
	}

	return nil
}

func (g *Generator) generateVTT(duration float64, numSprites int, urlPattern string) []byte {
	var buf bytes.Buffer
	buf.WriteString("WEBVTT\n\n")

	currentTime := 0.0

	for spriteIndex := 0; spriteIndex < numSprites; spriteIndex++ {
		spriteURL := fmt.Sprintf(urlPattern, spriteIndex)

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
				buf.WriteString(fmt.Sprintf("%s#xywh=%d,%d,%d,%d\n\n", spriteURL, x, y, g.thumbWidth, g.thumbHeight))

				currentTime += g.interval
			}
			if currentTime >= duration {
				break
			}
		}
	}

	return buf.Bytes()
}

func (g *Generator) GetSubtitles(ctx context.Context, sourceURL string, streamIndex int, lang string) ([]byte, error) {
	exists, err := g.storage.SubtitleVTTExists(ctx, sourceURL, lang)
	if err != nil {
		return nil, fmt.Errorf("check subtitle vtt: %w", err)
	}

	if !exists {
		if err := g.extractSubtitles(ctx, sourceURL, streamIndex, lang); err != nil {
			return nil, err
		}
	}

	return g.storage.ReadSubtitleVTT(ctx, sourceURL, lang)
}

func (g *Generator) extractSubtitles(ctx context.Context, sourceURL string, streamIndex int, lang string) error {
	args := []string{
		"-i", sourceURL,
		"-map", fmt.Sprintf("0:s:%d", streamIndex),
		"-c:s", "webvtt",
		"-f", "webvtt",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("ffmpeg subtitle extraction: %w", err)
	}

	if err := g.storage.WriteSubtitleVTT(ctx, sourceURL, lang, output); err != nil {
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
