package catalog

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// TTL memoizes values for a while. Stale-on-error: when a refresh fails, the last good value is
// served rather than the error, so a CDN blip does not take the service down.
type TTL struct {
	ttl    time.Duration
	mu     sync.Mutex
	values map[string]ttlEntry
	group  singleflight.Group
}

type ttlEntry struct {
	at    time.Time
	value any
}

// staleRetry is how long a stale value is served after a failed refresh before the next try, so
// an origin that is down is not asked again on every request.
const staleRetry = 10 * time.Second

// NewTTL makes a cache whose values are fresh for ttl.
func NewTTL(ttl time.Duration) *TTL { return &TTL{ttl: ttl, values: map[string]ttlEntry{}} }

// Get returns key's value, calling produce when it is missing or stale.
func (c *TTL) Get(key string, produce func() (any, error)) (any, error) {
	c.mu.Lock()
	hit, ok := c.values[key]
	c.mu.Unlock()
	if ok && time.Since(hit.at) < c.ttl {
		return hit.value, nil
	}
	v, err, _ := c.group.Do(key, func() (any, error) {
		v, err := produce()
		if err != nil {
			if ok {
				c.mu.Lock()
				c.values[key] = ttlEntry{time.Now().Add(staleRetry - c.ttl), hit.value}
				c.mu.Unlock()
			}
			return nil, err
		}
		c.mu.Lock()
		c.values[key] = ttlEntry{time.Now(), v}
		c.mu.Unlock()
		return v, nil
	})
	if err != nil {
		if ok {
			return hit.value, nil
		}
		return nil, err
	}
	return v, nil
}

// Forget drops key, so the next Get produces it again.
func (c *TTL) Forget(key string) {
	c.mu.Lock()
	delete(c.values, key)
	c.mu.Unlock()
	c.group.Forget(key)
}

// Last returns key's value however old it is.
func (c *TTL) Last(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.values[key]
	return e.value, ok
}
