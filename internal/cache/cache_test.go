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

	t.Run("keeps a parsed value with its entry until the entry is replaced or evicted", func(t *testing.T) {
		m := cache.NewMemory(8)
		v := []byte("1234")
		m.Put("a", v)
		_, parsed, ok := m.GetParsed("a")
		is.True(t, ok)
		is.True(t, parsed == nil)
		m.SetParsed("a", v, 1234)
		got, parsed, ok := m.GetParsed("a")
		is.True(t, ok)
		is.Equal(t, "1234", string(got))
		is.Equal(t, any(1234), parsed)
		is.Equal(t, int64(4), m.Bytes(), "the parsed value is not counted")

		m.Put("a", []byte("5678"))
		_, parsed, _ = m.GetParsed("a")
		is.True(t, parsed == nil, "a new value drops the parsed one")
		m.SetParsed("a", v, 1234)
		_, parsed, _ = m.GetParsed("a")
		is.True(t, parsed == nil, "a parsed value of an older value is not kept")

		m.Put("b", make([]byte, 4))
		m.Put("c", make([]byte, 4))
		_, _, ok = m.GetParsed("a")
		is.True(t, !ok, "the parsed value leaves with its entry")
		m.SetParsed("a", v, 1234)
		_, _, ok = m.GetParsed("a")
		is.True(t, !ok, "setting a parsed value does not add an entry")
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
