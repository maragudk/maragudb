package engine

import (
	"errors"
	"fmt"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"
)

func TestKeyFilter(t *testing.T) {
	t.Parallel()

	t.Run("holds every key it was built over, for any set of keys", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			keys := hegel.Draw(ht, hegel.Lists(hegel.Binary(0, 24)).MaxSize(300))
			var hashes []uint64
			for _, k := range keys {
				hashes = append(hashes, keyHash(k))
			}
			f := buildFilter(hashes)
			if err := f.validate(); err != nil {
				ht.Fatalf("the filter is not valid: %v", err)
			}
			for _, k := range keys {
				if !f.mayContain(k) {
					ht.Fatalf("the filter of %d keys says that %q is absent", len(keys), k)
				}
			}
		})
	})

	t.Run("says that about 1% of other keys may be present", func(t *testing.T) {
		const n = 10_000
		var hashes []uint64
		for i := range n {
			hashes = append(hashes, keyHash(fmt.Appendf(nil, "r/t\x00\x01%08d", i)))
		}
		f := buildFilter(hashes)
		is.Equal(t, 1+n*filterBitsPerKey/8, len(f))
		present := 0
		for i := range 10 * n {
			if f.mayContain(fmt.Appendf(nil, "r/t\x00\x01%08d", n+i)) {
				present++
			}
		}
		rate := float64(present) / (10 * n)
		t.Logf("false positive rate %.4f", rate)
		is.True(t, rate < 0.015, fmt.Sprint(rate))
	})

	t.Run("a nil filter holds every key", func(t *testing.T) {
		is.True(t, keyFilter(nil).mayContain([]byte("anything")))
	})

	t.Run("validation rejects a filter without bits, and one with no probes or more than 32", func(t *testing.T) {
		is.True(t, keyFilter{7}.validate() != nil)
		is.True(t, keyFilter{0, 1}.validate() != nil)
		is.True(t, keyFilter{33, 1}.validate() != nil)
		is.NotError(t, keyFilter{32, 1}.validate())
	})
}

// FuzzFilterBlock decodes arbitrary bytes as a filter block, which never panics, fails with
// [ErrCorrupt] when it fails, and gives a filter that answers lookups without panicking.
func FuzzFilterBlock(f *testing.F) {
	var entries []entry
	for i := range 40 {
		entries = append(entries, entry{key: fmt.Appendf(nil, "key-%03d", i), value: []byte("v")})
	}
	body, meta := build(f, 128, entries)
	if meta.FilterBlock == nil {
		f.Fatal("the seed segment has no filter block")
	}
	ref := *meta.FilterBlock
	f.Add(body[ref.Offset:ref.Offset+ref.Length], ref.Offset, true)
	f.Add(body[ref.Offset:ref.Offset+ref.Length-blockChecksumBytes], ref.Offset, false)
	f.Add([]byte{blockKindFilter, 1, 7, 0}, int64(0), false)
	f.Fuzz(func(t *testing.T, b []byte, offset int64, checksum bool) {
		ref := blockRef{Offset: offset, Length: int64(len(b))}
		var filter keyFilter
		var err error
		if checksum {
			filter, err = decodeFilterBlock(testSegmentKey, ref, b)
		} else {
			filter, err = parseFilterBlock(testSegmentKey, ref, b)
		}
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("error %v does not wrap ErrCorrupt", err)
			}
			return
		}
		filter.mayContain([]byte("key-007"))
		filter.mayContain(nil)
	})
}
