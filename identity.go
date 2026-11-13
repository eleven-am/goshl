package goshl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/source"
)

const cacheSchemaVersion = "goshl-cache-v6"

type SourceRevisionResolver func(context.Context, string) (string, error)

type CacheInvalidator interface {
	DeleteMedia(context.Context, string) error
}

func (c *Controller) MediaKey(ctx context.Context, sourceURL string) (string, error) {
	if err := c.ensureRunning(); err != nil {
		return "", err
	}
	return c.resolveMediaKey(ctx, sourceURL)
}

func (c *Controller) Invalidate(ctx context.Context, sourceURL string) error {
	if err := c.ensureRunning(); err != nil {
		return err
	}
	invalidator, ok := c.opts.Storage.(CacheInvalidator)
	if !ok {
		return ErrCacheInvalidationUnsupported
	}
	var spriteInvalidator CacheInvalidator
	if c.opts.SpriteStorage != nil {
		spriteInvalidator, ok = c.opts.SpriteStorage.(CacheInvalidator)
		if !ok {
			return ErrCacheInvalidationUnsupported
		}
	}
	mediaKey, err := c.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return err
	}
	c.metadata.clearFailure(mediaKey)
	if err := invalidator.DeleteMedia(ctx, mediaKey); err != nil {
		return err
	}
	if spriteInvalidator != nil {
		return spriteInvalidator.DeleteMedia(ctx, mediaKey)
	}
	return nil
}

func (c *Controller) resolveMediaKey(ctx context.Context, sourceURL string) (string, error) {
	revision, err := c.sourceRevision(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("resolve source revision: %w", err)
	}
	return hashIdentity(cacheSchemaVersion, sourceURL, revision, c.pipelineVersion), nil
}

func (c *Controller) sourceRevision(ctx context.Context, sourceURL string) (string, error) {
	if c.opts.SourceRevision != nil {
		return c.opts.SourceRevision(ctx, sourceURL)
	}
	if path, local := localSourcePath(sourceURL); local {
		if info, statErr := os.Stat(path); statErr == nil {
			return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano()), nil
		}
	}
	return sourceURL, nil
}

func localSourcePath(sourceURL string) (string, bool) {
	input, err := source.Resolve(sourceURL)
	if err != nil || input.Remote {
		return "", false
	}
	return input.LocalPath, true
}

func pipelineVersion(opts Options, config *domain.HWAccelConfig) string {
	return pipelineIdentity(opts, config, domain.OutputTimelineOffset, domain.JobContractVersion)
}

func pipelineIdentity(opts Options, config *domain.HWAccelConfig, timelineOffset time.Duration, jobContractVersion int) string {
	value := struct {
		Schema             string
		Version            string
		TargetDuration     float64
		Accelerator        domain.Accelerator
		DecodeFlags        []string
		EncodeFlags        []string
		ScaleFilter        string
		DecodeMode         domain.DecodeMode
		TimelineOffset     time.Duration
		JobContractVersion int
	}{
		Schema:             cacheSchemaVersion,
		Version:            opts.CacheVersion,
		TargetDuration:     opts.TargetDuration,
		Accelerator:        config.Accelerator,
		DecodeFlags:        config.DecodeFlags,
		EncodeFlags:        config.EncodeFlags,
		ScaleFilter:        config.ScaleFilter,
		DecodeMode:         config.DecodeMode,
		TimelineOffset:     timelineOffset,
		JobContractVersion: jobContractVersion,
	}
	encoded, _ := json.Marshal(value)
	return hashIdentity(string(encoded))
}

func assetIdentity(mediaKey, kind string, config any) string {
	encoded, _ := json.Marshal(config)
	return mediaKey + ":asset:" + hashIdentity(kind, string(encoded))
}

func hashIdentity(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte{0})
		hash.Write([]byte(part))
	}
	return "goshl:" + hex.EncodeToString(hash.Sum(nil))
}

func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
