package cache_test

import (
	"fmt"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func TestMemory(t *testing.T) {
	t.Run("returns what was put", func(t *testing.T) {
		m := cache.NewMemory(100)
		m.Put("a", []byte("1"))
		v, ok := m.Get("a")
		is.True(t, ok)
		is.Equal(t, "1", string(v))
		_, ok = m.Get("b")
		is.True(t, !ok)
	})

	t.Run("evicts the least recently used entries when over the byte budget", func(t *testing.T) {
		m := cache.NewMemory(10)
		m.Put("a", make([]byte, 4))
		m.Put("b", make([]byte, 4))
		_, ok := m.Get("a")
		is.True(t, ok)
		m.Put("c", make([]byte, 4))
		_, ok = m.Get("b")
		is.True(t, !ok, "b was least recently used")
		_, ok = m.Get("a")
		is.True(t, ok)
		_, ok = m.Get("c")
		is.True(t, ok)
		is.True(t, m.Bytes() <= 10)
	})

	t.Run("keeps a single entry larger than the budget", func(t *testing.T) {
		m := cache.NewMemory(2)
		m.Put("a", make([]byte, 4))
		_, ok := m.Get("a")
		is.True(t, ok)
	})

	t.Run("never exceeds the budget with more than one entry and never loses the newest entry", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			budget := hegel.Draw(ht, hegel.Integers(1, 64))
			m := cache.NewMemory(int64(budget))
			n := hegel.Draw(ht, hegel.Integers(1, 100))
			for range n {
				key := fmt.Sprint(hegel.Draw(ht, hegel.Integers(0, 20)))
				size := hegel.Draw(ht, hegel.Integers(0, 32))
				m.Put(key, make([]byte, size))
				if _, ok := m.Get(key); !ok {
					ht.Fatalf("newest entry %q missing", key)
				}
				if m.Len() > 1 && m.Bytes() > int64(budget) {
					ht.Fatalf("bytes %d over budget %d with %d entries", m.Bytes(), budget, m.Len())
				}
			}
		}, hegel.WithTestCases(500))
	})
}

func TestDisk(t *testing.T) {
	t.Run("returns what was put and survives a new instance on the same directory", func(t *testing.T) {
		dir := t.TempDir()
		d, err := cache.NewDisk(dir)
		is.NotError(t, err)
		d.Put("a/b", []byte("1"))
		v, ok := d.Get("a/b")
		is.True(t, ok)
		is.Equal(t, "1", string(v))
		_, ok = d.Get("missing")
		is.True(t, !ok)

		d2, err := cache.NewDisk(dir)
		is.NotError(t, err)
		v, ok = d2.Get("a/b")
		is.True(t, ok)
		is.Equal(t, "1", string(v))
	})
}
