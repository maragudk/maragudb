// Package cache has byte caches for immutable objects: an in-memory LRU and a directory on disk.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
		entry.value = value
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

// Disk is a cache of files in a directory, named by a hash of the key. It never evicts.
type Disk struct {
	dir string
}

// NewDisk cache in dir, which is created if missing.
func NewDisk(dir string) (*Disk, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: creating directory %q: %w", dir, err)
	}
	return &Disk{dir: dir}, nil
}

func (d *Disk) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(d.dir, hex.EncodeToString(sum[:]))
}

func (d *Disk) Get(key string) ([]byte, bool) {
	b, err := os.ReadFile(d.path(key))
	if err != nil {
		return nil, false
	}
	return b, true
}

func (d *Disk) Put(key string, value []byte) {
	path := d.path(key)
	tmp, err := os.CreateTemp(d.dir, "tmp-*")
	if err != nil {
		return
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.Write(value); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}
