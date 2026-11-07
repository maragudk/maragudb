package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// A segment is one immutable object holding a sorted run of entries as a B+tree of blocks.
//
// Object layout: data blocks in key order, then index blocks bottom-up, then the root block.
// The root's location is recorded in the manifest, so a lookup needs no footer read.
//
// Block layout: kind byte, uvarint count, then entries.
//   - data entry: uvarint key length, key, flags byte (bit 0: tombstone), and unless a tombstone,
//     uvarint value length, value
//   - index entry: uvarint first key length, first key, uvarint offset, uvarint length
//
// An index entry covers the keys from its first key up to the next entry's first key.
const (
	blockKindData  = 0
	blockKindIndex = 1
)

// blockRef locates a block in its object.
type blockRef struct {
	Offset int64 `json:"o"`
	Length int64 `json:"l"`
}

type entry struct {
	key       []byte
	value     []byte
	tombstone bool
}

type indexEntry struct {
	firstKey []byte
	ref      blockRef
}

// segmentBuilder writes entries in strictly increasing key order.
type segmentBuilder struct {
	blockSize  int
	buf        bytes.Buffer
	block      bytes.Buffer
	blockN     int
	blockFirst []byte
	leaves     []indexEntry
	// blocks written so far, in object order, for priming caches.
	blocks []blockRef
	count  int
	// tombstones among the entries.
	tombstones int
	lastKey    []byte
	minKey     []byte
	maxKey     []byte
}

func newSegmentBuilder(blockSize int) *segmentBuilder {
	return &segmentBuilder{blockSize: blockSize}
}

func (b *segmentBuilder) add(key, value []byte, tombstone bool) error {
	if b.count > 0 && bytes.Compare(key, b.lastKey) <= 0 {
		return fmt.Errorf("engine: segment keys must be strictly increasing, got %q after %q", key, b.lastKey)
	}
	if b.blockN > 0 && b.block.Len() > 0 && b.block.Len()+len(key)+len(value)+16 > b.blockSize {
		b.flushBlock()
	}
	if b.blockN == 0 {
		b.blockFirst = append([]byte(nil), key...)
	}
	b.block.Write(binary.AppendUvarint(nil, uint64(len(key))))
	b.block.Write(key)
	if tombstone {
		b.block.WriteByte(1)
		b.tombstones++
	} else {
		b.block.WriteByte(0)
		b.block.Write(binary.AppendUvarint(nil, uint64(len(value))))
		b.block.Write(value)
	}
	b.blockN++
	b.count++
	b.lastKey = append(b.lastKey[:0], key...)
	if b.count == 1 {
		b.minKey = append([]byte(nil), key...)
	}
	b.maxKey = append(b.maxKey[:0], key...)
	return nil
}

func (b *segmentBuilder) flushBlock() {
	ref := b.writeBlock(blockKindData, b.blockN, b.block.Bytes())
	b.leaves = append(b.leaves, indexEntry{firstKey: b.blockFirst, ref: ref})
	b.block.Reset()
	b.blockN = 0
}

func (b *segmentBuilder) writeBlock(kind byte, n int, body []byte) blockRef {
	offset := int64(b.buf.Len())
	b.buf.WriteByte(kind)
	b.buf.Write(binary.AppendUvarint(nil, uint64(n)))
	b.buf.Write(body)
	ref := blockRef{Offset: offset, Length: int64(b.buf.Len()) - offset}
	b.blocks = append(b.blocks, ref)
	return ref
}

// finish the segment and return its bytes and metadata. The metadata has no key yet.
func (b *segmentBuilder) finish() ([]byte, segmentMeta, error) {
	if b.count == 0 {
		return nil, segmentMeta{}, errors.New("engine: empty segment")
	}
	if b.blockN > 0 {
		b.flushBlock()
	}
	level := b.leaves
	for len(level) > 1 {
		var next []indexEntry
		var block bytes.Buffer
		var n int
		var first []byte
		flush := func() {
			ref := b.writeBlock(blockKindIndex, n, block.Bytes())
			next = append(next, indexEntry{firstKey: first, ref: ref})
			block.Reset()
			n = 0
		}
		for _, e := range level {
			// An index block holds at least two entries so that every level shrinks.
			if n >= 2 && block.Len()+len(e.firstKey)+24 > b.blockSize {
				flush()
			}
			if n == 0 {
				first = e.firstKey
			}
			block.Write(binary.AppendUvarint(nil, uint64(len(e.firstKey))))
			block.Write(e.firstKey)
			block.Write(binary.AppendUvarint(nil, uint64(e.ref.Offset)))
			block.Write(binary.AppendUvarint(nil, uint64(e.ref.Length)))
			n++
		}
		flush()
		level = next
	}
	meta := segmentMeta{
		Size:       int64(b.buf.Len()),
		Root:       level[0].ref,
		MinKey:     b.minKey,
		MaxKey:     b.maxKey,
		Count:      b.count,
		Tombstones: b.tombstones,
	}
	return b.buf.Bytes(), meta, nil
}

// decodedBlock is either a data block or an index block.
type decodedBlock struct {
	kind    byte
	entries []entry
	index   []indexEntry
}

func decodeBlock(b []byte) (*decodedBlock, error) {
	if len(b) < 2 {
		return nil, errors.New("engine: block too short")
	}
	kind := b[0]
	r := bytes.NewReader(b[1:])
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("engine: reading block count: %w", err)
	}
	readBytes := func() ([]byte, error) {
		l, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		if l > uint64(r.Len()) {
			return nil, errors.New("length exceeds block")
		}
		out := make([]byte, l)
		if _, err := r.Read(out); err != nil && l > 0 {
			return nil, err
		}
		return out, nil
	}
	// Every entry takes at least two bytes, so a count beyond that is corrupt. Checking it here
	// also keeps a corrupt count from driving a huge allocation.
	if n > uint64(r.Len())/2+1 {
		return nil, fmt.Errorf("engine: block count %d exceeds its %d bytes", n, r.Len())
	}
	d := &decodedBlock{kind: kind}
	switch kind {
	case blockKindData:
		d.entries = make([]entry, 0, n)
		for range n {
			key, err := readBytes()
			if err != nil {
				return nil, fmt.Errorf("engine: reading key: %w", err)
			}
			flags, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("engine: reading flags: %w", err)
			}
			e := entry{key: key, tombstone: flags&1 == 1}
			if !e.tombstone {
				e.value, err = readBytes()
				if err != nil {
					return nil, fmt.Errorf("engine: reading value: %w", err)
				}
			}
			d.entries = append(d.entries, e)
		}
	case blockKindIndex:
		d.index = make([]indexEntry, 0, n)
		for range n {
			key, err := readBytes()
			if err != nil {
				return nil, fmt.Errorf("engine: reading index key: %w", err)
			}
			offset, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, fmt.Errorf("engine: reading index offset: %w", err)
			}
			length, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, fmt.Errorf("engine: reading index length: %w", err)
			}
			d.index = append(d.index, indexEntry{firstKey: key, ref: blockRef{Offset: int64(offset), Length: int64(length)}})
		}
	default:
		return nil, fmt.Errorf("engine: unknown block kind %d", kind)
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("engine: %d trailing bytes in block", r.Len())
	}
	return d, nil
}

// childFor returns the index of the child covering key: the last entry whose first key is <= key,
// or 0 when key precedes all entries.
func (d *decodedBlock) childFor(key []byte) int {
	i := sort.Search(len(d.index), func(i int) bool { return bytes.Compare(d.index[i].firstKey, key) > 0 })
	if i == 0 {
		return 0
	}
	return i - 1
}

// find the entry with the given key.
func (d *decodedBlock) find(key []byte) (entry, bool) {
	i := sort.Search(len(d.entries), func(i int) bool { return bytes.Compare(d.entries[i].key, key) >= 0 })
	if i < len(d.entries) && bytes.Equal(d.entries[i].key, key) {
		return d.entries[i], true
	}
	return entry{}, false
}

// blockFetcher reads a block of a segment.
type blockFetcher func(ref blockRef) (*decodedBlock, error)

// maxTreeDepth bounds walks through a segment, so that a corrupt index cannot loop forever.
const maxTreeDepth = 64

// segmentLookup finds key in a segment by walking from the root.
func segmentLookup(root blockRef, key []byte, fetch blockFetcher) (entry, bool, error) {
	ref := root
	for depth := 0; ; depth++ {
		if depth >= maxTreeDepth {
			return entry{}, false, errors.New("engine: segment index deeper than allowed")
		}
		d, err := fetch(ref)
		if err != nil {
			return entry{}, false, err
		}
		if d.kind == blockKindData {
			e, ok := d.find(key)
			return e, ok, nil
		}
		if len(d.index) == 0 {
			return entry{}, false, errors.New("engine: empty index block")
		}
		ref = d.index[d.childFor(key)].ref
	}
}

// segmentIterator walks a segment's entries in key order from a start key.
type segmentIterator struct {
	fetch blockFetcher
	stack []iterFrame
	cur   entry
	valid bool
	err   error
}

type iterFrame struct {
	block *decodedBlock
	pos   int
}

// newSegmentIterator positioned at the first entry with key >= start. A nil start begins at the first entry.
func newSegmentIterator(root blockRef, start []byte, fetch blockFetcher) *segmentIterator {
	it := &segmentIterator{fetch: fetch}
	it.seek(root, start)
	return it
}

func (it *segmentIterator) seek(root blockRef, start []byte) {
	ref := root
	for {
		if len(it.stack) >= maxTreeDepth {
			it.err = errors.New("engine: segment index deeper than allowed")
			return
		}
		d, err := it.fetch(ref)
		if err != nil {
			it.err = err
			return
		}
		if d.kind == blockKindData {
			pos := 0
			if start != nil {
				pos = sort.Search(len(d.entries), func(i int) bool { return bytes.Compare(d.entries[i].key, start) >= 0 })
			}
			it.stack = append(it.stack, iterFrame{block: d, pos: pos})
			it.settle()
			return
		}
		if len(d.index) == 0 {
			it.err = errors.New("engine: empty index block")
			return
		}
		pos := 0
		if start != nil {
			pos = d.childFor(start)
		}
		it.stack = append(it.stack, iterFrame{block: d, pos: pos})
		ref = d.index[pos].ref
	}
}

// settle makes the top frame point at a valid entry, descending into following blocks as needed.
func (it *segmentIterator) settle() {
	for len(it.stack) > 0 {
		top := &it.stack[len(it.stack)-1]
		if top.block.kind == blockKindData {
			if top.pos < len(top.block.entries) {
				it.cur = top.block.entries[top.pos]
				it.valid = true
				return
			}
			it.stack = it.stack[:len(it.stack)-1]
			if len(it.stack) > 0 {
				it.stack[len(it.stack)-1].pos++
			}
			continue
		}
		if top.pos < len(top.block.index) {
			if len(it.stack) >= maxTreeDepth {
				it.err = errors.New("engine: segment index deeper than allowed")
				it.valid = false
				return
			}
			d, err := it.fetch(top.block.index[top.pos].ref)
			if err != nil {
				it.err = err
				it.valid = false
				return
			}
			it.stack = append(it.stack, iterFrame{block: d, pos: 0})
			continue
		}
		it.stack = it.stack[:len(it.stack)-1]
		if len(it.stack) > 0 {
			it.stack[len(it.stack)-1].pos++
		}
	}
	it.valid = false
}

// Valid reports whether the iterator points at an entry.
func (it *segmentIterator) Valid() bool {
	return it.valid && it.err == nil
}

// Entry at the current position.
func (it *segmentIterator) Entry() entry {
	return it.cur
}

// Next advances to the following entry.
func (it *segmentIterator) Next() {
	if !it.Valid() {
		return
	}
	it.stack[len(it.stack)-1].pos++
	it.settle()
}

// Err from fetching blocks.
func (it *segmentIterator) Err() error {
	return it.err
}
