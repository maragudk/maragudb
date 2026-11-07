package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A key filter is a Bloom filter over every key of a segment, of its values and of its
// tombstones, so that a point lookup can skip a segment that does not hold its key. A filter
// never says that a key of the segment is absent; it says that another key is present at a rate
// of about 1% at [filterBitsPerKey] bits per key and [filterProbes] probes.
//
// Encoding: the number of probes as one byte, then the bits, bit i in byte i/8 at 1<<(i%8). Its
// size is about 1.25 bytes per key.
//
// Where it lives depends on its size, because a filter only saves requests when a lookup has it
// before the segment's blocks:
//   - A filter of at most [segmentBuilder.inlineFilterBytes] is in the manifest, sealed by the
//     manifest's checksum, so it costs no request at all.
//   - A larger one of at most [segmentBuilder.maxFilterBlockBytes], of a segment whose tree has
//     more than one level, is a block of its own right after the root block, with a block
//     checksum like every other block. A cold lookup reads the root and the filter in one range
//     GET, so a segment that does not hold the key costs one GET instead of one per level.
//   - Any other segment has no filter: one of a single block gains nothing from a filter in its
//     object, and a larger filter would make the first read of the segment large.
const (
	filterBitsPerKey = 10
	filterProbes     = 7
	// blockKindFilter is the kind of a filter block. Its body is the encoded filter.
	blockKindFilter = 2
)

// keyFilter is an encoded filter. A nil filter holds every key.
type keyFilter []byte

// buildFilter over the keys whose hashes are hashes.
func buildFilter(hashes []uint64) keyFilter {
	bits := max(64, filterBitsPerKey*len(hashes))
	f := make(keyFilter, 1+(bits+7)/8)
	f[0] = filterProbes
	m := uint64(len(f)-1) * 8
	for _, h := range hashes {
		h1, h2 := filterHashes(h)
		for i := range uint64(filterProbes) {
			bit := (h1 + i*h2) % m
			f[1+bit/8] |= 1 << (bit % 8)
		}
	}
	return f
}

// mayContain reports whether the filter may hold key. It is false only when key is not one of
// the keys the filter was built over.
func (f keyFilter) mayContain(key []byte) bool {
	return f.mayContainHash(keyHash(key))
}

// mayContainHash reports whether the filter may hold the key whose [keyHash] is h.
func (f keyFilter) mayContainHash(h uint64) bool {
	if f == nil {
		return true
	}
	m := uint64(len(f)-1) * 8
	h1, h2 := filterHashes(h)
	for i := range uint64(f[0]) {
		bit := (h1 + i*h2) % m
		if f[1+bit/8]&(1<<(bit%8)) == 0 {
			return false
		}
	}
	return true
}

// validate the structure of an encoded filter: a probe count of 1 to 32 and at least one byte
// of bits. Damaged bits are caught by the checksum around the filter, not here.
func (f keyFilter) validate() error {
	if len(f) < 2 {
		return fmt.Errorf("a filter of %d bytes", len(f))
	}
	if f[0] == 0 || f[0] > 32 {
		return fmt.Errorf("a filter with %d probes", f[0])
	}
	return nil
}

// keyHash is the 64-bit FNV-1a hash of key, mixed by the finalizer of MurmurHash3, so that keys
// that differ only in their last bytes spread over the whole filter.
func keyHash(key []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range key {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return mix64(h)
}

func mix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// filterHashes are the two hashes of the double hashing of a key's hash h, the second odd.
func filterHashes(h uint64) (uint64, uint64) {
	return h, mix64(h^0x9e3779b97f4a7c15) | 1
}

// decodeFilterBlock checks the filter block at ref of the object at objectKey, whose bytes are b,
// and decodes it. A filter block is laid out like the other blocks: its kind, a count of 1, the
// filter, and the checksum. Bytes that fail the check or do not decode give an error that wraps
// [ErrCorrupt].
func decodeFilterBlock(objectKey string, ref blockRef, b []byte) (keyFilter, error) {
	body, err := checkBlock(objectKey, ref, b)
	if err != nil {
		return nil, err
	}
	return parseFilterBlock(objectKey, ref, body)
}

// parseFilterBlock decodes the bytes before the checksum of the filter block at ref of the
// object at objectKey.
func parseFilterBlock(objectKey string, ref blockRef, body []byte) (keyFilter, error) {
	f, err := parseFilterBody(body)
	if err != nil {
		return nil, fmt.Errorf("%w: filter block %d+%d of %q: %w", ErrCorrupt, ref.Offset, ref.Length, objectKey, err)
	}
	return f, nil
}

func parseFilterBody(body []byte) (keyFilter, error) {
	if len(body) < 2 || body[0] != blockKindFilter {
		return nil, errors.New("not a filter block")
	}
	n, size := binary.Uvarint(body[1:])
	if size <= 0 || n != 1 {
		return nil, errors.New("a filter block with a count other than 1")
	}
	f := keyFilter(body[1+size:])
	if err := f.validate(); err != nil {
		return nil, err
	}
	return f, nil
}
