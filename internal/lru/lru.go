// Package lru is a byte-bounded least-recently-used cache, safe for concurrent use.
package lru

import (
	"container/list"
	"sync"
)

type entry[K comparable, V any] struct {
	key   K
	value V
	bytes int64
}

// Cache evicts the least recently used entries once their sizes add up past its budget.
type Cache[K comparable, V any] struct {
	mu     sync.Mutex
	budget int64
	bytes  int64
	order  *list.List // front = most recently used
	items  map[K]*list.Element
}

// New makes a cache holding at most budget bytes.
func New[K comparable, V any](budget int64) *Cache[K, V] {
	return &Cache[K, V]{budget: budget, order: list.New(), items: map[K]*list.Element{}}
}

// Get returns the value for key and marks it used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*entry[K, V]).value, true
	}
	var zero V
	return zero, false
}

// Put stores value under key with its size, then evicts down to the budget.
func (c *Cache[K, V]) Put(key K, value V, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		c.bytes += bytes - e.bytes
		e.value, e.bytes = value, bytes
		c.order.MoveToFront(el)
	} else {
		c.items[key] = c.order.PushFront(&entry[K, V]{key, value, bytes})
		c.bytes += bytes
	}
	c.evict()
}

// Grow adds n bytes to key's size (an entry that fills in over time), then evicts.
func (c *Cache[K, V]) Grow(key K, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*entry[K, V]).bytes += n
		c.bytes += n
		c.evict()
	}
}

// Bytes is the total size held.
func (c *Cache[K, V]) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *Cache[K, V]) evict() {
	for c.bytes > c.budget && c.order.Len() > 0 {
		el := c.order.Back()
		e := el.Value.(*entry[K, V])
		c.order.Remove(el)
		delete(c.items, e.key)
		c.bytes -= e.bytes
	}
}
