package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/verify"
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

// testSegmentKey is the object key of the segments that the tests build.
const testSegmentKey = "db/seg/0000000000000001-test"

// memoryFetcher serves blocks from a segment held in memory at [testSegmentKey], checking them,
// and counts fetches.
func memoryFetcher(body []byte, fetches *int) blockFetcher {
	return func(ref blockRef) (*decodedBlock, error) {
		if fetches != nil {
			*fetches++
		}
		b, err := rangeOf(body, ref)
		if err != nil {
			return nil, err
		}
		return decodeBlock(testSegmentKey, ref, b)
	}
}

// rangeOf body that ref locates, as a range read of the store returns it.
func rangeOf(body []byte, ref blockRef) ([]byte, error) {
	if ref.Offset < 0 || ref.Length <= 0 || ref.Offset >= int64(len(body)) {
		return nil, fmt.Errorf("ref %+v out of range for %d bytes", ref, len(body))
	}
	return body[ref.Offset:min(ref.Offset+ref.Length, int64(len(body)))], nil
}

func build(t testing.TB, blockSize int, entries []entry) ([]byte, segmentMeta) {
	t.Helper()
	return buildAt(t, testSegmentKey, blockSize, entries)
}

func buildAt(t testing.TB, key string, blockSize int, entries []entry) ([]byte, segmentMeta) {
	t.Helper()
	b := newSegmentBuilder(blockSize)
	for _, e := range entries {
		is.NotError(t, b.add(e.key, e.value, e.tombstone))
	}
	body, meta, err := b.finish(key)
	is.NotError(t, err)
	is.Equal(t, key, meta.Key)
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
	t.Parallel()

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
		verify.Covers(t, "a point lookup in a random segment fetches at most one block per tree level", "AC-5.3")
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

	t.Run("a segment keeps its key filter in the manifest when it is small, as a block after the root when its tree has levels, and not at all otherwise", func(t *testing.T) {
		entriesOf := func(n, valueBytes int) []entry {
			var entries []entry
			for i := range n {
				e := entry{key: fmt.Appendf(nil, "k%05d", i), tombstone: i%9 == 4}
				if !e.tombstone {
					e.value = bytes.Repeat([]byte("v"), valueBytes)
				}
				entries = append(entries, e)
			}
			return entries
		}
		// holdsAll checks that the filter of the segment holds every key, through the decoder.
		holdsAll := func(t *testing.T, body []byte, meta segmentMeta, entries []entry) {
			t.Helper()
			f := meta.Filter
			if meta.FilterBlock != nil {
				var err error
				f, err = decodeFilterBlock(testSegmentKey, *meta.FilterBlock, body[meta.FilterBlock.Offset:meta.FilterBlock.Offset+meta.FilterBlock.Length])
				is.NotError(t, err)
			}
			for _, e := range entries {
				is.True(t, f.mayContain(e.key), string(e.key))
			}
		}

		// 4 keys, a filter of 9 bytes, within a sixteenth of a block of 256 bytes.
		entries := entriesOf(4, 4)
		body, meta := build(t, 256, entries)
		is.Equal(t, 9, len(meta.Filter))
		is.True(t, meta.FilterBlock == nil)
		is.Equal(t, int64(len(body)), meta.Root.Offset+meta.Root.Length, "the object ends with the root block")
		holdsAll(t, body, meta, entries)

		// 100 keys of a tree of several levels, a filter of 126 bytes, after the root block.
		entries = entriesOf(100, 4)
		body, meta = build(t, 256, entries)
		is.True(t, meta.Filter == nil)
		is.True(t, meta.FilterBlock != nil)
		is.Equal(t, meta.Root.Offset+meta.Root.Length, meta.FilterBlock.Offset)
		is.Equal(t, int64(len(body)), meta.FilterBlock.Offset+meta.FilterBlock.Length, "the object ends with the filter block")
		is.Equal(t, int64(126+minBlockBytes), meta.FilterBlock.Length)
		is.Equal(t, meta.Size, int64(len(body)))
		is.Equal(t, meta.Size-meta.FilterBlock.Length, meta.dataBytes())
		holdsAll(t, body, meta, entries)
		lookup := memoryFetcher(body, nil)
		for _, e := range entries {
			got, ok, err := segmentLookup(meta.Root, e.key, lookup)
			is.NotError(t, err)
			is.True(t, ok && equalEntries([]entry{got}, []entry{e}))
		}
		is.True(t, equalEntries(entries, collect(newSegmentIterator(meta.Root, nil, lookup))), "a scan never reads the filter block")

		// 900 keys, whose filter of 1,126 bytes is more than four blocks of 256 bytes.
		_, meta = build(t, 256, entriesOf(900, 4))
		is.True(t, meta.Filter == nil && meta.FilterBlock == nil)

		// 2,000 keys in one block of 32 KiB, whose filter of 2,501 bytes is more than a sixteenth
		// of the block, and a block of its own would save no request.
		_, meta = build(t, 32<<10, entriesOf(2000, 0))
		is.Equal(t, int64(0), meta.Root.Offset, "the segment is one block")
		is.True(t, meta.Filter == nil && meta.FilterBlock == nil)
	})

	t.Run("rejects keys out of order and duplicates", func(t *testing.T) {
		b := newSegmentBuilder(64)
		is.NotError(t, b.add([]byte("b"), []byte("1"), false))
		is.True(t, b.add([]byte("a"), []byte("1"), false) != nil)
		is.True(t, b.add([]byte("b"), []byte("1"), false) != nil)
	})

	t.Run("rejects an empty segment", func(t *testing.T) {
		_, _, err := newSegmentBuilder(64).finish(testSegmentKey)
		is.True(t, err != nil)
	})

	t.Run("a corrupt index that points at itself fails instead of looping", func(t *testing.T) {
		var b bytes.Buffer
		b.WriteByte(blockKindIndex)
		b.Write(binary.AppendUvarint(nil, 1))
		b.Write(binary.AppendUvarint(nil, 0))
		b.Write(binary.AppendUvarint(nil, 0))
		b.Write(binary.AppendUvarint(nil, 0))
		body := sealBlock(testSegmentKey, 0, b.Bytes())
		root := blockRef{Offset: 0, Length: int64(len(body))}
		_, _, err := segmentLookup(root, []byte("k"), memoryFetcher(body, nil))
		is.Error(t, ErrCorrupt, err)
		it := newSegmentIterator(root, nil, memoryFetcher(body, nil))
		is.True(t, !it.Valid())
		is.Error(t, ErrCorrupt, it.Err())
	})

	t.Run("a walk through index blocks that share their children reads every data block once and fails instead of repeating", func(t *testing.T) {
		// Two data blocks, then two index blocks that both point at both of them, and a root that
		// points at both index blocks, so that a walk would read each data block twice.
		var obj []byte
		block := func(body []byte) blockRef {
			ref := blockRef{Offset: int64(len(obj))}
			obj = append(obj, sealBlock(testSegmentKey, ref.Offset, body)...)
			ref.Length = int64(len(obj)) - ref.Offset
			return ref
		}
		data := func(key string) []byte {
			b := []byte{blockKindData, 1}
			b = binary.AppendUvarint(b, uint64(len(key)))
			b = append(b, key...)
			return append(b, 0, 1, 'v')
		}
		index := func(keys []string, refs []blockRef) []byte {
			b := []byte{blockKindIndex, byte(len(keys))}
			for i, k := range keys {
				b = binary.AppendUvarint(b, uint64(len(k)))
				b = append(b, k...)
				b = binary.AppendUvarint(b, uint64(refs[i].Offset))
				b = binary.AppendUvarint(b, uint64(refs[i].Length))
			}
			return b
		}
		a, c := block(data("a")), block(data("c"))
		x := block(index([]string{"a", "c"}, []blockRef{a, c}))
		y := block(index([]string{"e", "g"}, []blockRef{a, c}))
		root := block(index([]string{"a", "e"}, []blockRef{x, y}))
		it := newSegmentIterator(root, nil, memoryFetcher(obj, nil))
		got := collect(it)
		is.Equal(t, 2, len(got))
		is.Error(t, ErrCorrupt, it.Err())
	})

	t.Run("decoding arbitrary bytes never panics or allocates beyond the input", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			b := hegel.Draw(ht, hegel.Binary(0, 64))
			d, err := parseBlockBody(blockRef{Offset: 1 << 20, Length: int64(len(b))}, b)
			if err != nil {
				return
			}
			if len(d.entries)+len(d.index) > len(b) {
				ht.Fatalf("decoded %d entries from %d bytes", len(d.entries)+len(d.index), len(b))
			}
		}, hegel.WithTestCases(2000))
	})

	t.Run("every block fails with ErrCorrupt when a bit of it flips, when it is cut short, or when its bytes come from another object or another offset", func(t *testing.T) {
		verify.Covers(t, "every bit flip and every truncation of every block of a segment, and a block read from another segment or another offset, fail with ErrCorrupt", "INV-DAMAGE-DETECTED")
		var entries []entry
		for i := range 40 {
			entries = append(entries, entry{key: fmt.Appendf(nil, "key-%03d", i), value: fmt.Appendf(nil, "value-%d", i), tombstone: i%7 == 3})
		}
		body, meta := build(t, 128, entries)
		other, _ := buildAt(t, "db/seg/0000000000000002-other", 128, entries)
		is.True(t, bytes.Equal(body[:10], other[:10]), "the two segments hold the same entries")
		b := newSegmentBuilder(128)
		for _, e := range entries {
			is.NotError(t, b.add(e.key, e.value, e.tombstone))
		}
		_, _, err := b.finish(testSegmentKey)
		is.NotError(t, err)
		refs := b.blocks
		is.True(t, len(refs) > 4, "the segment has several levels of blocks")
		is.True(t, meta.FilterBlock != nil, "the segment has a filter block")
		// decode the block at ref, the filter block with its own decoder.
		decode := func(ref blockRef, b []byte) error {
			if ref == *meta.FilterBlock {
				_, err := decodeFilterBlock(testSegmentKey, ref, b)
				return err
			}
			_, err := decodeBlock(testSegmentKey, ref, b)
			return err
		}
		for _, ref := range refs {
			block := body[ref.Offset : ref.Offset+ref.Length]
			is.NotError(t, decode(ref, block))
			for i := range block {
				for bit := range 8 {
					damaged := bytes.Clone(block)
					damaged[i] ^= 1 << bit
					is.Error(t, ErrCorrupt, decode(ref, damaged), fmt.Sprintf("block %+v with bit %d of byte %d flipped", ref, bit, i))
				}
			}
			for n := range len(block) {
				is.Error(t, ErrCorrupt, decode(ref, block[:n]), fmt.Sprintf("block %+v cut to %d bytes", ref, n))
			}
			is.Error(t, ErrCorrupt, decode(ref, other[ref.Offset:ref.Offset+ref.Length]), "the same block of another segment")
			for _, at := range refs {
				if at != ref && at.Length == ref.Length {
					is.Error(t, ErrCorrupt, decode(at, block), "a block of the segment at another offset")
				}
			}
		}
		_, err = decodeBlock(testSegmentKey, *meta.FilterBlock, body[meta.FilterBlock.Offset:meta.FilterBlock.Offset+meta.FilterBlock.Length])
		is.Error(t, ErrCorrupt, err, "the filter block read as a block of the tree")
		_, err = decodeFilterBlock(testSegmentKey, meta.Root, body[meta.Root.Offset:meta.Root.Offset+meta.Root.Length])
		is.Error(t, ErrCorrupt, err, "the root block read as a filter block")
	})
}

// sealBlock appends the checksum of the block at offset of the object at key to body.
func sealBlock(key string, offset int64, body []byte) []byte {
	return binary.BigEndian.AppendUint32(bytes.Clone(body), blockChecksum(key, offset, body))
}

// FuzzSegment walks a segment of arbitrary bytes with a point lookup and a scan, which never
// panic, loop, or read a data block twice, and fail with [ErrCorrupt] when they fail. Half of the
// inputs get valid checksums, so that the fuzzer reaches the decoding behind them.
func FuzzSegment(f *testing.F) {
	var entries []entry
	for i := range 30 {
		entries = append(entries, entry{key: fmt.Appendf(nil, "k%02d", i), value: fmt.Appendf(nil, "v%d", i), tombstone: i%5 == 2})
	}
	for _, blockSize := range []int{1, 40, 200} {
		body, meta := build(f, blockSize, entries)
		f.Add(body, meta.Root.Offset, meta.Root.Length, []byte("k07"), false)
		f.Add(body, meta.Root.Offset, meta.Root.Length, []byte(""), true)
	}
	f.Add([]byte{1, 1, 0, 0, 6, 0, 0, 0, 0}, int64(0), int64(9), []byte("k"), true)
	f.Fuzz(func(t *testing.T, body []byte, rootOffset, rootLength int64, key []byte, reseal bool) {
		// reseal gives every block that the walk reads a valid checksum.
		fetch := func(ref blockRef) (*decodedBlock, error) {
			b, err := rangeOf(body, ref)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
			}
			if reseal && len(b) >= minBlockBytes {
				b = sealBlock(testSegmentKey, ref.Offset, b[:len(b)-blockChecksumBytes])
			}
			return decodeBlock(testSegmentKey, ref, b)
		}
		root := blockRef{Offset: rootOffset, Length: rootLength}
		if _, _, err := segmentLookup(root, key, fetch); err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("lookup: %v", err)
		}
		fetches := 0
		it := newSegmentIterator(root, key, func(ref blockRef) (*decodedBlock, error) {
			fetches++
			return fetch(ref)
		})
		var last []byte
		for n := 0; it.Valid(); n++ {
			if n > 0 && bytes.Compare(it.Entry().key, last) <= 0 {
				t.Fatalf("the scan yields %q after %q", it.Entry().key, last)
			}
			last = it.Entry().key
			it.Next()
		}
		if err := it.Err(); err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("scan: %v", err)
		}
		// Every data block is read at most once, and each read of an index block leads to a read
		// of a data block or to the end of the walk, along a path of at most maxTreeDepth blocks.
		if bound := (len(body)/minBlockBytes + 1) * maxTreeDepth; fetches > bound {
			t.Fatalf("the scan read %d blocks of %d bytes", fetches, len(body))
		}
	})
}

// FuzzBlock decodes arbitrary bytes as a block, which never panics, fails with [ErrCorrupt] when
// it fails, and yields entries in key order.
func FuzzBlock(f *testing.F) {
	body, _ := build(f, 64, []entry{{key: []byte("a"), value: []byte("1")}, {key: []byte("b"), tombstone: true}, {key: []byte("c"), value: []byte("3")}})
	f.Add(body, int64(0), true)
	f.Add(body, int64(len(body)), false)
	f.Add([]byte{blockKindIndex, 2, 1, 'a', 0, 6, 1, 'b', 6, 6}, int64(100), false)
	f.Fuzz(func(t *testing.T, b []byte, offset int64, checksum bool) {
		ref := blockRef{Offset: offset, Length: int64(len(b))}
		var d *decodedBlock
		var err error
		if checksum {
			d, err = decodeBlock(testSegmentKey, ref, b)
		} else {
			d, err = parseBlock(testSegmentKey, ref, b)
		}
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("error %v does not wrap ErrCorrupt", err)
			}
			return
		}
		for i := 1; i < len(d.entries); i++ {
			if bytes.Compare(d.entries[i-1].key, d.entries[i].key) >= 0 {
				t.Fatalf("entries out of order")
			}
		}
		for i, x := range d.index {
			if x.ref.Offset < 0 || x.ref.Offset+x.ref.Length > offset || i > 0 && x.ref.Offset < d.index[i-1].ref.Offset+d.index[i-1].ref.Length {
				t.Fatalf("child %+v of index entry %d of the block at %d", x.ref, i, offset)
			}
		}
	})
}

func keysOf(entries []entry) [][]byte {
	var keys [][]byte
	for _, e := range entries {
		keys = append(keys, e.key)
	}
	return keys
}
