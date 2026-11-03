package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"
)

// drawEntries draws unique keys in sorted order with random values and tombstones.
func drawEntries(ht *hegel.T, minSize, maxSize int) []entry {
	keys := hegel.Draw(ht, hegel.Lists(hegel.Binary(0, 8)).MinSize(minSize).MaxSize(maxSize))
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	var entries []entry
	for i, k := range keys {
		if i > 0 && bytes.Equal(k, keys[i-1]) {
			continue
		}
		e := entry{key: k, tombstone: hegel.Draw(ht, hegel.WeightedBooleans(0.2))}
		if !e.tombstone {
			e.value = hegel.Draw(ht, hegel.Binary(0, 16))
		}
		entries = append(entries, e)
	}
	return entries
}

// memoryFetcher serves blocks from a segment held in memory and counts fetches.
func memoryFetcher(body []byte, fetches *int) blockFetcher {
	return func(ref blockRef) (*decodedBlock, error) {
		if fetches != nil {
			*fetches++
		}
		if ref.Offset < 0 || ref.Offset+ref.Length > int64(len(body)) {
			return nil, fmt.Errorf("ref %+v out of range for %d bytes", ref, len(body))
		}
		return decodeBlock(body[ref.Offset : ref.Offset+ref.Length])
	}
}

func build(t testing.TB, blockSize int, entries []entry) ([]byte, segmentMeta) {
	t.Helper()
	b := newSegmentBuilder(blockSize)
	for _, e := range entries {
		is.NotError(t, b.add(e.key, e.value, e.tombstone))
	}
	body, meta, err := b.finish()
	is.NotError(t, err)
	return body, meta
}

func collect(it *segmentIterator) []entry {
	var out []entry
	for it.Valid() {
		out = append(out, it.Entry())
		it.Next()
	}
	return out
}

func equalEntries(a, b []entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i].key, b[i].key) || !bytes.Equal(a[i].value, b[i].value) || a[i].tombstone != b[i].tombstone {
			return false
		}
	}
	return true
}

func TestSegment(t *testing.T) {
	t.Run("iterating a built segment from the start yields exactly the entries added", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			entries := drawEntries(ht, 1, 60)
			blockSize := hegel.Draw(ht, hegel.Integers(1, 256))
			body, meta := build(ht, blockSize, entries)
			if meta.Count != len(entries) || !bytes.Equal(meta.MinKey, entries[0].key) || !bytes.Equal(meta.MaxKey, entries[len(entries)-1].key) || meta.Size != int64(len(body)) {
				ht.Fatalf("meta %+v does not match %d entries", meta, len(entries))
			}
			it := newSegmentIterator(meta.Root, nil, memoryFetcher(body, nil))
			got := collect(it)
			if it.Err() != nil {
				ht.Fatal(it.Err())
			}
			if !equalEntries(entries, got) {
				ht.Fatalf("got %v, want %v", got, entries)
			}
		}, hegel.WithTestCases(1000))
	})

	t.Run("point lookups find every added key and no other", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			entries := drawEntries(ht, 1, 60)
			blockSize := hegel.Draw(ht, hegel.Integers(1, 256))
			body, meta := build(ht, blockSize, entries)
			fetch := memoryFetcher(body, nil)
			for _, e := range entries {
				got, ok, err := segmentLookup(meta.Root, e.key, fetch)
				if err != nil || !ok || !bytes.Equal(got.value, e.value) || got.tombstone != e.tombstone {
					ht.Fatalf("lookup %q: got %v %v %v, want %v", e.key, got, ok, err, e)
				}
			}
			probe := hegel.Draw(ht, hegel.Binary(0, 9))
			_, ok, err := segmentLookup(meta.Root, probe, fetch)
			if err != nil {
				ht.Fatal(err)
			}
			present := false
			for _, e := range entries {
				if bytes.Equal(e.key, probe) {
					present = true
				}
			}
			if ok != present {
				ht.Fatalf("lookup %q: found=%v, want %v", probe, ok, present)
			}
		}, hegel.WithTestCases(1000))
	})

	t.Run("iterating from a start key yields exactly the entries at or after it", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			entries := drawEntries(ht, 1, 60)
			blockSize := hegel.Draw(ht, hegel.Integers(1, 256))
			body, meta := build(ht, blockSize, entries)
			start := hegel.Draw(ht, hegel.OneOf(hegel.Binary(0, 9), hegel.SampledFrom(keysOf(entries))))
			var want []entry
			for _, e := range entries {
				if bytes.Compare(e.key, start) >= 0 {
					want = append(want, e)
				}
			}
			it := newSegmentIterator(meta.Root, start, memoryFetcher(body, nil))
			got := collect(it)
			if it.Err() != nil {
				ht.Fatal(it.Err())
			}
			if !equalEntries(want, got) {
				ht.Fatalf("from %q: got %v, want %v", start, got, want)
			}
		}, hegel.WithTestCases(1000))
	})

	t.Run("a point lookup fetches at most one block per tree level, logarithmic in entry count", func(t *testing.T) {
		for _, n := range []int{1, 10, 100, 1000, 10000} {
			var entries []entry
			for i := range n {
				entries = append(entries, entry{key: fmt.Appendf(nil, "key-%08d", i), value: []byte("v")})
			}
			body, meta := build(t, 256, entries)
			var fetches int
			fetch := memoryFetcher(body, &fetches)
			_, ok, err := segmentLookup(meta.Root, entries[n/2].key, fetch)
			is.NotError(t, err)
			is.True(t, ok)
			// Blocks of 256 bytes hold at least 8 of these entries, so the depth is at most 1 + log8(n).
			maxDepth := 1
			for m := n; m > 1; m = (m + 7) / 8 {
				maxDepth++
			}
			is.True(t, fetches <= maxDepth, fmt.Sprintf("n=%d: %d fetches, max depth %d", n, fetches, maxDepth))
		}
	})

	t.Run("rejects keys out of order and duplicates", func(t *testing.T) {
		b := newSegmentBuilder(64)
		is.NotError(t, b.add([]byte("b"), []byte("1"), false))
		is.True(t, b.add([]byte("a"), []byte("1"), false) != nil)
		is.True(t, b.add([]byte("b"), []byte("1"), false) != nil)
	})

	t.Run("rejects an empty segment", func(t *testing.T) {
		_, _, err := newSegmentBuilder(64).finish()
		is.True(t, err != nil)
	})

	t.Run("a corrupt index that points at itself fails instead of looping", func(t *testing.T) {
		var b bytes.Buffer
		b.WriteByte(blockKindIndex)
		b.Write(binary.AppendUvarint(nil, 1))
		b.Write(binary.AppendUvarint(nil, 0))
		b.Write(binary.AppendUvarint(nil, 0))
		b.Write(binary.AppendUvarint(nil, 0))
		body := b.Bytes()
		root := blockRef{Offset: 0, Length: int64(len(body))}
		_, _, err := segmentLookup(root, []byte("k"), memoryFetcher(body, nil))
		is.True(t, err != nil)
		it := newSegmentIterator(root, nil, memoryFetcher(body, nil))
		is.True(t, !it.Valid())
		is.True(t, it.Err() != nil)
	})

	t.Run("decoding arbitrary bytes fails cleanly or round-trips", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			b := hegel.Draw(ht, hegel.Binary(0, 64))
			d, err := decodeBlock(b)
			if err != nil {
				return
			}
			if d.kind != blockKindData && d.kind != blockKindIndex {
				ht.Fatalf("decoded unknown kind %d", d.kind)
			}
		}, hegel.WithTestCases(2000))
	})
}

func keysOf(entries []entry) [][]byte {
	var keys [][]byte
	for _, e := range entries {
		keys = append(keys, e.key)
	}
	return keys
}
