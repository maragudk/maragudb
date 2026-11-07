package engine

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"time"
)

// manifestVersion of the encoding. Version 3 seals the manifest and garbage batches with a
// checksum, and gives every block of a segment one. Version 2 kept stamped garbage in garbage
// batch objects, which the manifest only references, and version 1 listed every piece of garbage
// in the manifest. Versions 1 and 2 are plain JSON.
const manifestVersion = 3

// The manifest and the garbage batches are sealed: their objects hold a magic number of four
// bytes, then the CRC-32C of the object's key and of the JSON as four big-endian bytes, and then
// the JSON. The key in the checksum makes the bytes of another object fail it too.
const (
	manifestMagic     = "mdbm"
	garbageBatchMagic = "mdbg"
	sealBytes         = 8
)

// seal the JSON body of the object at key.
func seal(magic, key string, body []byte) []byte {
	out := make([]byte, sealBytes, sealBytes+len(body))
	copy(out, magic)
	binary.BigEndian.PutUint32(out[4:], sealChecksum(key, body))
	return append(out, body...)
}

func sealChecksum(key string, body []byte) uint32 {
	crc := crc32.Update(0, castagnoli, []byte(key))
	return crc32.Update(crc, castagnoli, body)
}

// unseal the object at key, whose bytes are b, and return its JSON body.
func unseal(magic, what, key string, b []byte) ([]byte, error) {
	if len(b) < sealBytes || string(b[:4]) != magic {
		return nil, fmt.Errorf("%w: %s %q does not begin as one", ErrCorrupt, what, key)
	}
	body := b[sealBytes:]
	if sealChecksum(key, body) != binary.BigEndian.Uint32(b[4:]) {
		return nil, fmt.Errorf("%w: %s %q fails its checksum", ErrCorrupt, what, key)
	}
	return body, nil
}

// manifest is the root of the database: the current commit sequence and the live segments.
// It is replaced atomically with a conditional write on every commit.
type manifest struct {
	Version  int           `json:"version"`
	Seq      uint64        `json:"seq"`
	CommitID string        `json:"commitID"`
	Segments []segmentMeta `json:"segments"`
	// Garbage holds the keys of the objects that left the segment list and that no GC pass has
	// stamped yet. A GC pass moves them into a garbage batch.
	Garbage []string `json:"garbage,omitempty"`
	// Batches are the chains of garbage batches, oldest first.
	Batches []batchRef     `json:"batches,omitempty"`
	Commits []commitRecord `json:"commits"`
	// UpdatedAt is the writer's clock at the time of the write, for diagnostics only.
	UpdatedAt time.Time `json:"updatedAt"`
}

// segmentMeta describes a live segment, oldest first in the manifest.
type segmentMeta struct {
	Key    string   `json:"key"`
	Size   int64    `json:"size"`
	Root   blockRef `json:"root"`
	MinKey []byte   `json:"minKey"`
	MaxKey []byte   `json:"maxKey"`
	Count  int      `json:"count"`
	// Tombstones among the Count entries. Manifests written before it was recorded lack it, and
	// read as none, which only delays a merge of the whole database.
	Tombstones int `json:"tombstones,omitempty"`
	// Filter is the segment's key filter when it is small enough for the manifest, and
	// FilterBlock locates it otherwise, right after the root block, when the segment has one.
	// Segments written before filters existed have neither, and a lookup reads them as before.
	Filter      keyFilter `json:"filter,omitempty"`
	FilterBlock *blockRef `json:"filterBlock,omitempty"`
}

// dataBytes of the segment: its size without its filter block.
func (s segmentMeta) dataBytes() int64 {
	if s.FilterBlock == nil {
		return s.Size
	}
	return s.Size - s.FilterBlock.Length
}

// batchRef references a chain of garbage batch objects: the newest one, which names the one
// before it, and so on. The objects of the chain and the garbage they list are all deleted
// together, once StampedAt is older than the grace period.
type batchRef struct {
	// Key of the newest batch object of the chain.
	Key string `json:"key"`
	// Since is the stamp of the oldest batch of the chain. A GC pass adds its batch to the newest
	// chain only while Since is less than an eighth of the grace period old, so that a chain
	// delays the deletion of its oldest garbage by at most that much, and the manifest
	// references a bounded number of chains.
	Since time.Time `json:"since"`
	// StampedAt is the latest stamp of any batch of the chain.
	StampedAt time.Time `json:"stampedAt"`
	// Objects counts the garbage of the chain and its batch objects, for [Stats].
	Objects int `json:"objects"`
}

// garbageBatchVersion of the encoding of a garbage batch object. Version 2 is sealed, and
// version 1 was plain JSON.
const garbageBatchVersion = 2

// garbageBatch is the body of a garbage batch object: the keys of the garbage that one GC pass
// stamped. Garbage batch objects are immutable.
type garbageBatch struct {
	Version   int       `json:"version"`
	StampedAt time.Time `json:"stampedAt"`
	Keys      []string  `json:"keys"`
	// Prev is the key of the batch object before this one in its chain, if any.
	Prev string `json:"prev,omitempty"`
}

// encodeGarbageBatch for the object at key.
func encodeGarbageBatch(key string, b *garbageBatch) ([]byte, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("engine: encoding garbage batch: %w", err)
	}
	return seal(garbageBatchMagic, key, body), nil
}

// decodeGarbageBatch of the object at key, whose bytes are body. Bytes that fail the checksum or
// do not decode to a valid batch give an error that wraps [ErrCorrupt].
func decodeGarbageBatch(key string, body []byte) (*garbageBatch, error) {
	j, err := unseal(garbageBatchMagic, "garbage batch", key, body)
	if err != nil {
		return nil, err
	}
	var b garbageBatch
	if err := json.Unmarshal(j, &b); err != nil {
		return nil, fmt.Errorf("%w: decoding garbage batch %q: %w", ErrCorrupt, key, err)
	}
	if b.Version != garbageBatchVersion {
		return nil, fmt.Errorf("%w: garbage batch %q has version %d, and this version of maragudb reads only version %d", ErrUnsupportedManifest, key, b.Version, garbageBatchVersion)
	}
	if b.Prev == key {
		return nil, fmt.Errorf("%w: garbage batch %q names itself as the batch before it", ErrCorrupt, key)
	}
	return &b, nil
}

// commitRecord of one manifest write.
type commitRecord struct {
	Seq uint64 `json:"seq"`
	ID  string `json:"id"`
}

// encodeManifest for the object at key.
func encodeManifest(key string, m *manifest) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("engine: encoding manifest: %w", err)
	}
	return seal(manifestMagic, key, b), nil
}

// decodeManifest of the object at key, whose bytes are b. A manifest of an earlier version gives
// an error that wraps [ErrUnsupportedManifest], and bytes that fail the checksum or do not
// decode to a valid manifest give one that wraps [ErrCorrupt].
func decodeManifest(key string, b []byte) (*manifest, error) {
	// The version is read on its own first, because the fields of other versions may not decode.
	var v struct {
		Version int `json:"version"`
	}
	if bytes.HasPrefix(b, []byte("{")) {
		// Versions 1 and 2 are plain JSON.
		if err := json.Unmarshal(b, &v); err == nil && v.Version > 0 && v.Version < manifestVersion {
			return nil, fmt.Errorf("%w: the manifest has version %d, and this version of maragudb reads only version %d", ErrUnsupportedManifest, v.Version, manifestVersion)
		}
	}
	j, err := unseal(manifestMagic, "manifest", key, b)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(j, &v); err != nil {
		return nil, fmt.Errorf("%w: decoding manifest: %w", ErrCorrupt, err)
	}
	if v.Version != manifestVersion {
		return nil, fmt.Errorf("%w: the manifest has version %d, and this version of maragudb reads only version %d", ErrUnsupportedManifest, v.Version, manifestVersion)
	}
	var m manifest
	if err := json.Unmarshal(j, &m); err != nil {
		return nil, fmt.Errorf("%w: decoding manifest: %w", ErrCorrupt, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%w: manifest %q: %w", ErrCorrupt, key, err)
	}
	return &m, nil
}

// validate what the engine relies on in a decoded manifest: that every segment's root block lies
// within its object, and its filter block right after the root block, that a filter in the
// manifest has the structure of one, that counts and sizes are not negative, and that the commit records are in
// order and end at most at the manifest's sequence number.
func (m *manifest) validate() error {
	for i, seg := range m.Segments {
		var filterErr error
		if seg.Filter != nil {
			filterErr = seg.Filter.validate()
		}
		switch {
		case seg.Key == "":
			return fmt.Errorf("segment %d has no key", i)
		case seg.Size < 0 || seg.Count < 0 || seg.Tombstones < 0 || seg.Tombstones > seg.Count:
			return fmt.Errorf("segment %q has size %d, %d entries, and %d tombstones", seg.Key, seg.Size, seg.Count, seg.Tombstones)
		case seg.Root.Offset < 0 || seg.Root.Length < minBlockBytes || seg.Root.Length > seg.Size-seg.Root.Offset:
			return fmt.Errorf("the root block %d+%d of segment %q is not within its %d bytes", seg.Root.Offset, seg.Root.Length, seg.Key, seg.Size)
		case bytes.Compare(seg.MinKey, seg.MaxKey) > 0:
			return fmt.Errorf("segment %q has a smallest key after its largest", seg.Key)
		case seg.Filter != nil && seg.FilterBlock != nil:
			return fmt.Errorf("segment %q has a filter in the manifest and a filter block", seg.Key)
		case filterErr != nil:
			return fmt.Errorf("segment %q: %w", seg.Key, filterErr)
		case seg.FilterBlock != nil && (seg.FilterBlock.Offset != seg.Root.Offset+seg.Root.Length || seg.FilterBlock.Length < minBlockBytes || seg.FilterBlock.Length > seg.Size-seg.FilterBlock.Offset):
			return fmt.Errorf("the filter block %d+%d of segment %q is not right after its root block %d+%d within its %d bytes", seg.FilterBlock.Offset, seg.FilterBlock.Length, seg.Key, seg.Root.Offset, seg.Root.Length, seg.Size)
		}
	}
	for i, ref := range m.Batches {
		if ref.Key == "" || ref.Objects < 0 {
			return fmt.Errorf("chain %d of garbage batches has key %q and %d objects", i, ref.Key, ref.Objects)
		}
	}
	for i, c := range m.Commits {
		if c.Seq > m.Seq || i > 0 && c.Seq <= m.Commits[i-1].Seq {
			return errors.New("the commit records are not in order up to the manifest's sequence number")
		}
	}
	return nil
}

// clone deeply enough that mutating the copy's slices does not affect the original.
func (m *manifest) clone() *manifest {
	c := *m
	c.Segments = append([]segmentMeta(nil), m.Segments...)
	c.Garbage = append([]string(nil), m.Garbage...)
	c.Batches = append([]batchRef(nil), m.Batches...)
	c.Commits = append([]commitRecord(nil), m.Commits...)
	return &c
}

// next manifest for a write with the given commit ID at the given time.
func (m *manifest) next(commitID string, now time.Time) *manifest {
	n := m.clone()
	n.Seq = m.Seq + 1
	n.CommitID = commitID
	// The last 128 commit records are enough to resolve a write whose answer was lost unless
	// that many other commits happen during its retries.
	n.Commits = append(n.Commits, commitRecord{Seq: n.Seq, ID: commitID})
	if len(n.Commits) > 128 {
		n.Commits = n.Commits[len(n.Commits)-128:]
	}
	n.UpdatedAt = now
	return n
}

// commitAt returns the record for seq, and whether the window covers seq at all.
func (m *manifest) commitAt(seq uint64) (commitRecord, bool) {
	for _, c := range m.Commits {
		if c.Seq == seq {
			return c, true
		}
	}
	return commitRecord{}, false
}

// segmentKeyPrefix and garbageBatchKeyPrefix under the database prefix.
const (
	segmentKeyPrefix      = "seg/"
	garbageBatchKeyPrefix = "gc/"
)

// segmentKey for a segment written against base sequence number seq with the given ID.
// The sequence number comes first so that a listing is ordered by it.
func segmentKey(seq uint64, id string) string {
	return fmt.Sprintf("%s%016x-%s", segmentKeyPrefix, seq, id)
}

// garbageBatchKey for a garbage batch written against base sequence number seq with the given ID.
func garbageBatchKey(seq uint64, id string) string {
	return fmt.Sprintf("%s%016x-%s", garbageBatchKeyPrefix, seq, id)
}

// objectBaseSeq parses the base sequence number from a segment or garbage batch key, after the
// database prefix.
func objectBaseSeq(key string) (uint64, bool) {
	rest, ok := strings.CutPrefix(key, segmentKeyPrefix)
	if !ok {
		rest, ok = strings.CutPrefix(key, garbageBatchKeyPrefix)
	}
	if !ok {
		return 0, false
	}
	hexSeq, _, ok := strings.Cut(rest, "-")
	if !ok || len(hexSeq) != 16 || strings.ToLower(hexSeq) != hexSeq {
		return 0, false
	}
	seq, err := strconv.ParseUint(hexSeq, 16, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}
