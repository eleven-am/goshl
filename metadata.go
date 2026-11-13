package goshl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/coordination"
	"github.com/eleven-am/goshl/internal/domain"
	"golang.org/x/sync/singleflight"
)

func (c *Controller) Metadata(ctx context.Context, sourceURL string) (*Metadata, error) {
	return c.getMetadata(ctx, sourceURL)
}

func (c *Controller) ProbeMetadata(ctx context.Context, sourceURL string) (*Metadata, error) {
	if err := c.ensureRunning(); err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.opts.ProbeTimeout)
	defer cancel()
	return c.prober.ProbeUncached(probeCtx, sourceURL)
}

func (c *Controller) getMetadata(ctx context.Context, sourceURL string) (*domain.Metadata, error) {
	metadata, _, err := c.getMetadataResolved(ctx, sourceURL)
	return metadata, err
}

func (c *Controller) getMetadataResolved(ctx context.Context, sourceURL string) (*domain.Metadata, string, error) {
	if err := c.ensureRunning(); err != nil {
		return nil, "", err
	}
	mediaKey, err := c.resolveMediaKey(ctx, sourceURL)
	if err != nil {
		return nil, "", err
	}
	metadata, err := c.getMetadataWithKey(ctx, sourceURL, mediaKey)
	return metadata, mediaKey, err
}

func (c *Controller) getMetadataWithKey(ctx context.Context, sourceURL, mediaKey string) (*domain.Metadata, error) {
	cached, found, err := c.loadCachedMetadata(ctx, mediaKey)
	var corrupt *corruptMetadataError
	if err != nil && !errors.As(err, &corrupt) {
		return nil, err
	}
	if found {
		c.observe(ctx, domain.Event{Type: domain.EventMetadataCacheHit, MediaKey: mediaKey})
		return cached, nil
	}
	miss := domain.Event{Type: domain.EventMetadataCacheMiss, MediaKey: mediaKey}
	if corrupt != nil {
		miss.Error = corrupt.Error()
	}
	c.observe(ctx, miss)
	if failure := c.metadata.recentFailure(mediaKey); failure != nil {
		return nil, failure
	}

	var produced *domain.Metadata
	ready := func(readyCtx context.Context) (bool, error) {
		metadata, ok, readErr := c.loadCachedMetadata(readyCtx, mediaKey)
		var corruptRead *corruptMetadataError
		if errors.As(readErr, &corruptRead) {
			return false, nil
		}
		if readErr != nil || !ok {
			return false, readErr
		}
		produced = metadata
		return true, nil
	}
	generate := func(probeCtx context.Context) error {
		epoch := c.metadata.currentEpoch()
		started := time.Now()
		metadata, probeErr := c.prober.ProbeUncached(probeCtx, sourceURL)
		event := domain.Event{Type: domain.EventMetadataProbed, MediaKey: mediaKey, Duration: time.Since(started)}
		if probeErr != nil {
			if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				probeErr = fmt.Errorf("%w: %w: %w", ErrMetadataTimeout, context.DeadlineExceeded, probeErr)
			}
			event.Error = probeErr.Error()
			c.observe(probeCtx, event)
			c.metadata.recordFailure(mediaKey, probeErr, epoch)
			return probeErr
		}
		data, marshalErr := json.Marshal(metadata)
		if marshalErr != nil {
			return marshalErr
		}
		if storeErr := c.opts.Storage.SetMetadata(probeCtx, mediaKey, data); storeErr != nil {
			event.Error = storeErr.Error()
			c.observe(probeCtx, event)
			return storeErr
		}
		c.observe(probeCtx, event)
		produced = metadata
		return nil
	}

	runner := coordination.Runner{
		Lifecycle:   c.lifecycle,
		Kind:        "metadata",
		Coordinator: c.opts.Coordinator,
		Timeout:     func() time.Duration { return c.opts.ProbeTimeout },
		Observe:     c.observe,
	}
	err = runner.Once(ctx, &c.metadata.group, mediaKey, ready, generate)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrMetadataTimeout) {
			return nil, fmt.Errorf("%w: %w", ErrMetadataTimeout, err)
		}
		return nil, err
	}
	if produced != nil {
		return produced, nil
	}
	metadata, ok, err := c.loadCachedMetadata(ctx, mediaKey)
	if err != nil {
		var corruptRead *corruptMetadataError
		if errors.As(err, &corruptRead) {
			return nil, ErrMetadataTimeout
		}
		return nil, err
	}
	if !ok {
		return nil, ErrMetadataTimeout
	}
	return metadata, nil
}

func (c *Controller) loadCachedMetadata(ctx context.Context, mediaKey string) (*domain.Metadata, bool, error) {
	exists, err := c.opts.Storage.MetadataExists(ctx, mediaKey)
	if err != nil || !exists {
		return nil, false, err
	}
	data, err := c.opts.Storage.GetMetadata(ctx, mediaKey)
	if err != nil {
		return nil, false, err
	}
	var metadata domain.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, false, &corruptMetadataError{cause: err}
	}
	return &metadata, true, nil
}

type corruptMetadataError struct {
	cause error
}

func (e *corruptMetadataError) Error() string {
	return fmt.Sprintf("corrupt cached metadata: %v", e.cause)
}

func (e *corruptMetadataError) Unwrap() error {
	return e.cause
}

const (
	metadataNegativeTTL = 10 * time.Second
	metadataNegativeMax = 1024
)

type metadataFailure struct {
	err     error
	expires time.Time
}

type metadataState struct {
	group    singleflight.Group
	mu       sync.Mutex
	failures map[string]metadataFailure
	epoch    uint64
	now      func() time.Time
}

func (s *metadataState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *metadataState) currentEpoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

func (s *metadataState) recentFailure(mediaKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	failure, ok := s.failures[mediaKey]
	if !ok {
		return nil
	}
	if s.clock().After(failure.expires) {
		delete(s.failures, mediaKey)
		return nil
	}
	return failure.err
}

func (s *metadataState) recordFailure(mediaKey string, err error, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.epoch {
		return
	}
	if s.failures == nil {
		s.failures = make(map[string]metadataFailure)
	}
	now := s.clock()
	for key, failure := range s.failures {
		if now.After(failure.expires) {
			delete(s.failures, key)
		}
	}
	if _, exists := s.failures[mediaKey]; !exists && len(s.failures) >= metadataNegativeMax {
		var oldestKey string
		var oldest time.Time
		for key, failure := range s.failures {
			if oldestKey == "" || failure.expires.Before(oldest) {
				oldestKey, oldest = key, failure.expires
			}
		}
		delete(s.failures, oldestKey)
	}
	s.failures[mediaKey] = metadataFailure{err: err, expires: now.Add(metadataNegativeTTL)}
}

func (s *metadataState) clearFailure(mediaKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	delete(s.failures, mediaKey)
}
