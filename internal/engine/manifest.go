package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// manifestVersion of the encoding. Version 2 keeps stamped garbage in garbage batch objects,
// which the manifest only references. Version 1 listed every piece of garbage in the manifest.
const manifestVersion = 2

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

// garbageBatchVersion of the encoding of a garbage batch object.
const garbageBatchVersion = 1

// garbageBatch is the body of a garbage batch object: the keys of the garbage that one GC pass
// stamped. Garbage batch objects are immutable.
type garbageBatch struct {
	Version   int       `json:"version"`
	StampedAt time.Time `json:"stampedAt"`
	Keys      []string  `json:"keys"`
	// Prev is the key of the batch object before this one in its chain, if any.
	Prev string `json:"prev,omitempty"`
}

func encodeGarbageBatch(b *garbageBatch) ([]byte, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("engine: encoding garbage batch: %w", err)
	}
	return body, nil
}

func decodeGarbageBatch(body []byte) (*garbageBatch, error) {
	var b garbageBatch
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("engine: decoding garbage batch: %w", err)
	}
	if b.Version != garbageBatchVersion {
		return nil, fmt.Errorf("engine: unsupported garbage batch version %d", b.Version)
	}
	return &b, nil
}

// commitRecord of one manifest write.
type commitRecord struct {
	Seq uint64 `json:"seq"`
	ID  string `json:"id"`
}

func encodeManifest(m *manifest) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("engine: encoding manifest: %w", err)
	}
	return b, nil
}

func decodeManifest(b []byte) (*manifest, error) {
	// The version is read on its own first, because the fields of other versions may not decode.
	var v struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("engine: decoding manifest: %w", err)
	}
	if v.Version != manifestVersion {
		return nil, fmt.Errorf("%w: the manifest has version %d, and this version of maragudb reads only version %d", ErrUnsupportedManifest, v.Version, manifestVersion)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("engine: decoding manifest: %w", err)
	}
	return &m, nil
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
