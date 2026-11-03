package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// manifestVersion of the encoding.
const manifestVersion = 1

// commitWindow is how many recent commit records a manifest keeps for resolving ambiguous writes.
const commitWindow = 128

// manifest is the root of the database: the current commit sequence and the live segments.
// It is replaced atomically with a conditional write on every commit.
type manifest struct {
	Version  int            `json:"version"`
	Seq      uint64         `json:"seq"`
	CommitID string         `json:"commitID"`
	Segments []segmentMeta  `json:"segments"`
	Garbage  []garbageEntry `json:"garbage,omitempty"`
	Commits  []commitRecord `json:"commits"`
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
	// Seq of the manifest write that added the segment.
	Seq uint64 `json:"seq"`
}

// garbageEntry is an object no longer referenced by the current manifest, kept until the grace
// period has passed so that readers with older snapshots can still read it.
type garbageEntry struct {
	Key            string    `json:"key"`
	UnreferencedAt time.Time `json:"unreferencedAt"`
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
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("engine: decoding manifest: %w", err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("engine: unsupported manifest version %d", m.Version)
	}
	return &m, nil
}

// clone deeply enough that mutating the copy's slices does not affect the original.
func (m *manifest) clone() *manifest {
	c := *m
	c.Segments = append([]segmentMeta(nil), m.Segments...)
	c.Garbage = append([]garbageEntry(nil), m.Garbage...)
	c.Commits = append([]commitRecord(nil), m.Commits...)
	return &c
}

// next manifest for a write with the given commit ID at the given time.
func (m *manifest) next(commitID string, now time.Time) *manifest {
	n := m.clone()
	n.Seq = m.Seq + 1
	n.CommitID = commitID
	n.Commits = append(n.Commits, commitRecord{Seq: n.Seq, ID: commitID})
	if len(n.Commits) > commitWindow {
		n.Commits = n.Commits[len(n.Commits)-commitWindow:]
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

// segmentKeyPrefix under the database prefix.
const segmentKeyPrefix = "seg/"

// segmentKey for a segment written against base sequence number seq with the given ID.
// The sequence number comes first so that a listing is ordered by it.
func segmentKey(seq uint64, id string) string {
	return fmt.Sprintf("%s%016x-%s", segmentKeyPrefix, seq, id)
}

// segmentBaseSeq parses the base sequence number from a segment key, after the database prefix.
func segmentBaseSeq(key string) (uint64, bool) {
	rest, ok := strings.CutPrefix(key, segmentKeyPrefix)
	if !ok {
		return 0, false
	}
	hexSeq, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseUint(hexSeq, 16, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}
