package coordtest

import (
	"context"
	"sync"
	"time"

	"github.com/eleven-am/goshl/internal/domain"
)

type MemCoordinator struct {
	domain.Coordinator
	mu    sync.Mutex
	locks map[string]string
	next  int
}

func NewMemCoordinator() *MemCoordinator {
	return &MemCoordinator{locks: map[string]string{}}
}

func (c *MemCoordinator) AcquireLock(ctx context.Context, key string, ttl time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.locks[key]; held {
		return "", domain.ErrLockHeld
	}
	c.next++
	token := key + "#" + time.Duration(c.next).String()
	c.locks[key] = token
	return token, nil
}

func (c *MemCoordinator) ReleaseLock(ctx context.Context, key string, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[key] == token {
		delete(c.locks, key)
	}
	return nil
}
