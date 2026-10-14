// Package lru is a small concurrency-safe LRU cache bounded by entry count.
package lru

import (
	"container/list"
	"sync"
)

type Cache[K comparable, V any] struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[K]*list.Element
}

type entry[K comparable, V any] struct {
	k K
	v V
}

func New[K comparable, V any](max int) *Cache[K, V] {
	return &Cache[K, V]{max: max, ll: list.New(), m: make(map[K]*list.Element)}
}

func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*entry[K, V]).v, true
	}
	var zero V
	return zero, false
}

func (c *Cache[K, V]) Add(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		e.Value.(*entry[K, V]).v = v
		c.ll.MoveToFront(e)
		return
	}
	c.m[k] = c.ll.PushFront(&entry[K, V]{k, v})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.m, old.Value.(*entry[K, V]).k)
	}
}

func (c *Cache[K, V]) Remove(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		c.ll.Remove(e)
		delete(c.m, k)
	}
}
