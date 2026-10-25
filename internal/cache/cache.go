// Package cache keeps values for a fixed time.
package cache

import (
	"sync"
	"time"
)

// TTL holds one value per key for a fixed time. Each key has its own
// lock, so a slow load for one key does not block other keys.
type TTL[T any] struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*entry[T]
}

type entry[T any] struct {
	mu    sync.Mutex
	value T
	at    time.Time
}

// NewTTL returns an empty cache that keeps values for ttl.
func NewTTL[T any](ttl time.Duration) *TTL[T] {
	return &TTL[T]{ttl: ttl, now: time.Now, entries: map[string]*entry[T]{}}
}

func (c *TTL[T]) entry(key string) *entry[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		e = &entry[T]{}
		c.entries[key] = e
	}
	return e
}

// Get returns the cached value and the time it was loaded. It calls load
// when the value is missing or older than the TTL.
func (c *TTL[T]) Get(key string, load func() T) (T, time.Time) {
	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.at.IsZero() || c.now().Sub(e.at) >= c.ttl {
		e.value = load()
		e.at = c.now()
	}
	return e.value, e.at
}

// Invalidate makes the next Get for key load a new value.
func (c *TTL[T]) Invalidate(key string) {
	e := c.entry(key)
	e.mu.Lock()
	e.at = time.Time{}
	e.mu.Unlock()
}

// Reset removes all values.
func (c *TTL[T]) Reset() {
	c.mu.Lock()
	c.entries = map[string]*entry[T]{}
	c.mu.Unlock()
}
