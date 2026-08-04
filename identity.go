package goshl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

const cacheSchemaVersion = "goshl-cache-v3"

type SourceRevisionResolver func(context.Context, string) (string, error)

type CacheInvalidator interface {
	DeleteMedia(context.Context, string) error
}

func (c *Controller) MediaKey(ctx context.Context, sourceURL string) (string, error) {
	return c.resolveMediaKey(ctx, sourceURL)
}

func (c *Controller) Invalidate(ctx context.Context, sourceURL string) error {
	invalidator, ok := c.opts.Storage.(CacheInvalidator)
	if !ok {
		return ErrCacheInvalidationUnsupported
	}
	mediaKey, err := c.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return err
	}
	return invalidator.DeleteMedia(ctx, mediaKey)
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
	if strings.HasPrefix(sourceURL, "file://") {
		path := strings.TrimPrefix(sourceURL, "file://")
		if strings.HasPrefix(path, "localhost/") {
			path = strings.TrimPrefix(path, "localhost")
		} else if !strings.HasPrefix(path, "/") {
			return "", false
		}
		decoded, err := url.PathUnescape(path)
		return decoded, err == nil
	}
	if strings.HasPrefix(sourceURL, "file:/") {
		decoded, err := url.PathUnescape(strings.TrimPrefix(sourceURL, "file:"))
		return decoded, err == nil
	}
	colon := strings.IndexByte(sourceURL, ':')
	if colon > 0 {
		return "", false
	}
	return sourceURL, true
}

func pipelineVersion(opts Options, config *domain.HWAccelConfig) string {
	value := struct {
		Schema         string
		Version        string
		TargetDuration float64
		Accelerator    domain.Accelerator
		DecodeFlags    []string
		EncodeFlags    []string
		ScaleFilter    string
		DecodeMode     domain.DecodeMode
	}{
		Schema:         cacheSchemaVersion,
		Version:        opts.CacheVersion,
		TargetDuration: opts.TargetDuration,
		Accelerator:    config.Accelerator,
		DecodeFlags:    config.DecodeFlags,
		EncodeFlags:    config.EncodeFlags,
		ScaleFilter:    config.ScaleFilter,
		DecodeMode:     config.DecodeMode,
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
