package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/source"
)

const cacheSchemaVersion = "goshl-cache-v6"

type SourceRevisionResolver func(context.Context, string) (string, error)

type CacheInvalidator interface {
	DeleteMedia(context.Context, string) error
}

func (e *Engine) MediaKey(ctx context.Context, sourceURL string) (string, error) {
	if err := e.ensureRunning(); err != nil {
		return "", err
	}
	return e.resolveMediaKey(ctx, sourceURL)
}

func (e *Engine) Invalidate(ctx context.Context, sourceURL string) error {
	if err := e.ensureRunning(); err != nil {
		return err
	}
	invalidator, ok := e.opts.Storage.(CacheInvalidator)
	if !ok {
		return ErrCacheInvalidationUnsupported
	}
	var spriteInvalidator CacheInvalidator
	if e.opts.SpriteStorage != nil {
		spriteInvalidator, ok = e.opts.SpriteStorage.(CacheInvalidator)
		if !ok {
			return ErrCacheInvalidationUnsupported
		}
	}
	mediaKey, err := e.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return err
	}
	e.metadata.clearFailure(mediaKey)
	e.channel.clearFailures(mediaKey)
	if err := invalidator.DeleteMedia(ctx, mediaKey); err != nil {
		return err
	}
	if spriteInvalidator != nil {
		return spriteInvalidator.DeleteMedia(ctx, mediaKey)
	}
	return nil
}

func (e *Engine) resolveMediaKey(ctx context.Context, sourceURL string) (string, error) {
	revision, err := e.sourceRevision(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("resolve source revision: %w", err)
	}
	return hashIdentity(cacheSchemaVersion, sourceURL, revision, e.pipelineVersion), nil
}

func (e *Engine) sourceRevision(ctx context.Context, sourceURL string) (string, error) {
	if e.opts.SourceRevision != nil {
		return e.opts.SourceRevision(ctx, sourceURL)
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

func routeProfile(pipeline string) string {
	return routeIdentity(pipeline, domain.JobContractVersion)
}

func routeIdentity(pipeline string, jobContractVersion int) string {
	return hashIdentity("goshl-route", pipeline, strconv.Itoa(jobContractVersion))
}

func pipelineVersion(opts Config, config *domain.HWAccelConfig) string {
	return pipelineIdentity(opts, config, domain.OutputTimelineOffset, domain.OutputContractVersion)
}

func pipelineIdentity(opts Config, config *domain.HWAccelConfig, timelineOffset time.Duration, jobContractVersion int) string {
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
