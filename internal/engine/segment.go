package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
)

// A segment is one immutable object holding a sorted run of entries as a B+tree of blocks.
//
// Object layout: data blocks in key order, then index blocks bottom-up, then the root block, and
// then, in some segments, the block of the segment's key filter, which filter.go describes. The
// root's location is recorded in the manifest, and so is the filter's, so a lookup needs no
// footer read.
//
// Block layout: kind byte, uvarint count, then entries, then a checksum.
//   - data entry: uvarint key length, key, flags byte (bit 0: tombstone), and unless a tombstone,
//     uvarint value length, value
//   - index entry: uvarint first key length, first key, uvarint offset, uvarint length
//   - checksum: four big-endian bytes, the CRC-32C of the segment's object key, of the block's
//     offset in the object as eight big-endian bytes, and of the block's bytes before the
//     checksum. A block read from the wrong object or the wrong offset fails it too, so a read
//     of one block checks it without reading the rest of the object.
//
// Every block has at least one entry, and the keys of a block are strictly increasing. An index
// entry covers the keys from its first key up to the next entry's first key, and its block lies
// after the block of the entry before it and before the index block itself.
//
// The manifest's version is the version of this format too: a manifest of version 3 references
// only segments whose blocks have checksums.
const (
	blockKindData  = 0
	blockKindIndex = 1
	// blockChecksumBytes is the size of the checksum at the end of every block.
	blockChecksumBytes = 4
	// minBlockBytes is the size of the smallest block: a kind, a count, and the checksum.
	minBlockBytes = 2 + blockChecksumBytes
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// blockChecksum of the block at offset in the object at objectKey, whose bytes before the
// checksum are body.
func blockChecksum(objectKey string, offset int64, body []byte) uint32 {
	var off [8]byte
	binary.BigEndian.PutUint64(off[:], uint64(offset))
	crc := crc32.Update(0, castagnoli, []byte(objectKey))
	crc = crc32.Update(crc, castagnoli, off[:])
	return crc32.Update(crc, castagnoli, body)
}

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
	// hashes of the keys, for the key filter.
	hashes []uint64
	// omitFilterKeys leaves every other key out of the filter, for [Mutations.FilterOmitsKeys].
	omitFilterKeys bool
}

func newSegmentBuilder(blockSize int) *segmentBuilder {
	return &segmentBuilder{blockSize: blockSize}
}

// inlineFilterBytes is the size up to which the key filter is kept in the manifest: a sixteenth
// of a block, 1 KiB at the default block size, about 800 keys.
func (b *segmentBuilder) inlineFilterBytes() int {
	return b.blockSize / 16
}

// maxFilterBlockBytes is the size up to which the key filter is a block of the segment: four
// blocks, 64 KiB at the default block size, about 52,000 keys.
func (b *segmentBuilder) maxFilterBlockBytes() int {
	return 4 * b.blockSize
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
	if !b.omitFilterKeys || b.count%2 == 0 {
		b.hashes = append(b.hashes, keyHash(key))
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

// writeBlock with room for its checksum, which [segmentBuilder.finish] fills in once the object
// key is known.
func (b *segmentBuilder) writeBlock(kind byte, n int, body []byte) blockRef {
	offset := int64(b.buf.Len())
	b.buf.WriteByte(kind)
	b.buf.Write(binary.AppendUvarint(nil, uint64(n)))
	b.buf.Write(body)
	b.buf.Write(make([]byte, blockChecksumBytes))
	ref := blockRef{Offset: offset, Length: int64(b.buf.Len()) - offset}
	b.blocks = append(b.blocks, ref)
	return ref
}

// finish the segment for the object at objectKey and return its bytes and metadata, with Key
// set to objectKey.
func (b *segmentBuilder) finish(objectKey string) ([]byte, segmentMeta, error) {
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
	root := level[0].ref
	var inline keyFilter
	var filterRef *blockRef
	switch filter := buildFilter(b.hashes); {
	case len(filter) <= b.inlineFilterBytes():
		inline = filter
	case len(b.leaves) > 1 && len(filter)+minBlockBytes <= b.maxFilterBlockBytes():
		ref := b.writeBlock(blockKindFilter, 1, filter)
		filterRef = &ref
	}
	out := b.buf.Bytes()
	for _, ref := range b.blocks {
		block := out[ref.Offset : ref.Offset+ref.Length]
		end := len(block) - blockChecksumBytes
		binary.BigEndian.PutUint32(block[end:], blockChecksum(objectKey, ref.Offset, block[:end]))
	}
	meta := segmentMeta{
		Key:         objectKey,
		Size:        int64(b.buf.Len()),
		Root:        root,
		MinKey:      b.minKey,
		MaxKey:      b.maxKey,
		Count:       b.count,
		Tombstones:  b.tombstones,
		Filter:      inline,
		FilterBlock: filterRef,
	}
	return out, meta, nil
}

// decodedBlock is either a data block or an index block.
type decodedBlock struct {
	kind    byte
	entries []entry
	index   []indexEntry
}

// decodeBlock checks the block at ref of the object at objectKey, whose bytes are b, and decodes
// it. Bytes that fail the check or do not decode give an error that wraps [ErrCorrupt].
func decodeBlock(objectKey string, ref blockRef, b []byte) (*decodedBlock, error) {
	body, err := checkBlock(objectKey, ref, b)
	if err != nil {
		return nil, err
	}
	return parseBlock(objectKey, ref, body)
}

// checkBlock's length and checksum, and return its bytes before the checksum.
func checkBlock(objectKey string, ref blockRef, b []byte) ([]byte, error) {
	body, err := blockBody(objectKey, ref, b)
	if err != nil {
		return nil, err
	}
	if want := binary.BigEndian.Uint32(b[len(body):]); blockChecksum(objectKey, ref.Offset, body) != want {
		return nil, fmt.Errorf("%w: block %d+%d of %q fails its checksum", ErrCorrupt, ref.Offset, ref.Length, objectKey)
	}
	return body, nil
}

// blockBody is the bytes of the block before its checksum, when b has the block's length.
func blockBody(objectKey string, ref blockRef, b []byte) ([]byte, error) {
	if ref.Offset < 0 || int64(len(b)) != ref.Length || len(b) < minBlockBytes {
		return nil, fmt.Errorf("%w: block %d+%d of %q has %d bytes", ErrCorrupt, ref.Offset, ref.Length, objectKey, len(b))
	}
	return b[:len(b)-blockChecksumBytes], nil
}

// parseBlock decodes the bytes before the checksum of the block at ref of the object at
// objectKey.
func parseBlock(objectKey string, ref blockRef, b []byte) (*decodedBlock, error) {
	d, err := parseBlockBody(ref, b)
	if err != nil {
		return nil, fmt.Errorf("%w: block %d+%d of %q: %w", ErrCorrupt, ref.Offset, ref.Length, objectKey, err)
	}
	return d, nil
}

func parseBlockBody(ref blockRef, b []byte) (*decodedBlock, error) {
	if len(b) < 2 {
		return nil, errors.New("block too short")
	}
	if ref.Offset < 0 {
		return nil, errors.New("block at a negative offset")
	}
	kind := b[0]
	r := bytes.NewReader(b[1:])
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("reading block count: %w", err)
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
	if n == 0 {
		return nil, errors.New("block without entries")
	}
	if n > uint64(r.Len())/2+1 {
		return nil, fmt.Errorf("block count %d exceeds its %d bytes", n, r.Len())
	}
	var last []byte
	ordered := func(i int, key []byte) error {
		if i > 0 && bytes.Compare(key, last) <= 0 {
			return fmt.Errorf("key %d of the block is not after the one before it", i)
		}
		last = key
		return nil
	}
	d := &decodedBlock{kind: kind}
	switch kind {
	case blockKindData:
		d.entries = make([]entry, 0, n)
		for i := range int(n) {
			key, err := readBytes()
			if err != nil {
				return nil, fmt.Errorf("reading key: %w", err)
			}
			if err := ordered(i, key); err != nil {
				return nil, err
			}
			flags, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("reading flags: %w", err)
			}
			if flags > 1 {
				return nil, fmt.Errorf("unknown flags %d", flags)
			}
			e := entry{key: key, tombstone: flags == 1}
			if !e.tombstone {
				e.value, err = readBytes()
				if err != nil {
					return nil, fmt.Errorf("reading value: %w", err)
				}
			}
			d.entries = append(d.entries, e)
		}
	case blockKindIndex:
		d.index = make([]indexEntry, 0, n)
		// Every child lies after the one before it and before this block, so that no walk can
		// visit a block twice through one index block, or loop.
		var next uint64
		for i := range int(n) {
			key, err := readBytes()
			if err != nil {
				return nil, fmt.Errorf("reading index key: %w", err)
			}
			if err := ordered(i, key); err != nil {
				return nil, err
			}
			offset, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, fmt.Errorf("reading index offset: %w", err)
			}
			length, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, fmt.Errorf("reading index length: %w", err)
			}
			if offset < next || length < minBlockBytes || length > uint64(ref.Offset) || offset > uint64(ref.Offset)-length {
				return nil, fmt.Errorf("child %d+%d of index entry %d is not after the one before it and before the block", offset, length, i)
			}
			next = offset + length
			d.index = append(d.index, indexEntry{firstKey: key, ref: blockRef{Offset: int64(offset), Length: int64(length)}})
		}
	default:
		return nil, fmt.Errorf("unknown block kind %d", kind)
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes in block", r.Len())
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
			return entry{}, false, fmt.Errorf("%w: segment index deeper than allowed", ErrCorrupt)
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
			return entry{}, false, fmt.Errorf("%w: empty index block", ErrCorrupt)
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
	// leafEnd is the end of the last data block that the walk fetched. Data blocks lie in key
	// order in the object, so each one the walk fetches must lie after the one before it, which
	// bounds a walk of a corrupt index to one visit of every data block.
	leafEnd int64
	// yielded reports whether the walk yielded an entry, and last is the key of the last one.
	// Every entry must have a key after the one before it.
	yielded bool
	last    []byte
}

// fetchBlock for the walk, checking that a data block lies after the one before it.
func (it *segmentIterator) fetchBlock(ref blockRef) (*decodedBlock, error) {
	d, err := it.fetch(ref)
	if err != nil {
		return nil, err
	}
	if d.kind == blockKindData {
		if ref.Offset < it.leafEnd {
			return nil, fmt.Errorf("%w: data block %d+%d of a segment lies before the one the walk read before it", ErrCorrupt, ref.Offset, ref.Length)
		}
		it.leafEnd = ref.Offset + ref.Length
	}
	return d, nil
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
			it.err = fmt.Errorf("%w: segment index deeper than allowed", ErrCorrupt)
			return
		}
		d, err := it.fetchBlock(ref)
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
			it.err = fmt.Errorf("%w: empty index block", ErrCorrupt)
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
				if it.yielded && bytes.Compare(it.cur.key, it.last) <= 0 {
					it.err = fmt.Errorf("%w: a segment has the key %q after %q", ErrCorrupt, it.cur.key, it.last)
					it.valid = false
					return
				}
				it.yielded, it.last = true, it.cur.key
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
				it.err = fmt.Errorf("%w: segment index deeper than allowed", ErrCorrupt)
				it.valid = false
				return
			}
			d, err := it.fetchBlock(top.block.index[top.pos].ref)
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
