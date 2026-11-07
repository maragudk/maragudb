// Package cache has byte caches for immutable objects: an LRU in memory and an LRU in a directory
// on disk, both bounded in bytes.
package cache

import (
	"container/list"
	"sync"
	"unsafe"
)

// Cache of byte values by key. Implementations must be safe for concurrent use.
// A cache may drop or fail to store any entry at any time, so it only ever speeds things up.
type Cache interface {
	Get(key string) ([]byte, bool)
	Put(key string, value []byte)
}

// Memory is an LRU cache bounded by total value bytes.
type Memory struct {
	maxBytes int64
	mu       sync.Mutex
	bytes    int64
	order    *list.List
	entries  map[string]*list.Element
}

type memoryEntry struct {
	key   string
	value []byte
	// parsed is a value that [Memory.SetParsed] derived from value, or nil.
	parsed any
}

// NewMemory cache holding at most maxBytes of values, except that the newest entry is always
// kept even when it alone exceeds the budget. A non-positive maxBytes means unbounded.
func NewMemory(maxBytes int64) *Memory {
	return &Memory{maxBytes: maxBytes, order: list.New(), entries: map[string]*list.Element{}}
}

func (m *Memory) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, false
	}
	m.order.MoveToFront(e)
	return e.Value.(*memoryEntry).value, true
}

func (m *Memory) Put(key string, value []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		entry := e.Value.(*memoryEntry)
		m.bytes += int64(len(value)) - int64(len(entry.value))
		entry.value, entry.parsed = value, nil
		m.order.MoveToFront(e)
	} else {
		e := m.order.PushFront(&memoryEntry{key: key, value: value})
		m.entries[key] = e
		m.bytes += int64(len(value))
	}
	for m.maxBytes > 0 && m.bytes > m.maxBytes && m.order.Len() > 1 {
		oldest := m.order.Back()
		entry := oldest.Value.(*memoryEntry)
		m.order.Remove(oldest)
		delete(m.entries, entry.key)
		m.bytes -= int64(len(entry.value))
	}
}

// GetParsed returns the value of key, like [Memory.Get], and the value that [Memory.SetParsed]
// derived from it, or nil. The parsed value is not counted in the bytes the cache holds, and it
// leaves the cache with its value.
func (m *Memory) GetParsed(key string) ([]byte, any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, nil, false
	}
	m.order.MoveToFront(e)
	entry := e.Value.(*memoryEntry)
	return entry.value, entry.parsed, true
}

// SetParsed keeps parsed, a value derived from value, next to the entry of key, when the entry
// still holds that same value. Every caller that reads parsed shares it, so it must not change.
func (m *Memory) SetParsed(key string, value []byte, parsed any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return
	}
	entry := e.Value.(*memoryEntry)
	if len(entry.value) == len(value) && unsafe.SliceData(entry.value) == unsafe.SliceData(value) {
		entry.parsed = parsed
	}
}

// Len is the number of entries.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.order.Len()
}

// Bytes held.
func (m *Memory) Bytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytes
}
