package engine

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"
)

func TestManifest(t *testing.T) {
	t.Run("encode then decode round-trips", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			m := &manifest{
				Version:   manifestVersion,
				Seq:       hegel.Draw(ht, hegel.Integers[uint64](0, 1<<62)),
				CommitID:  hegel.Draw(ht, hegel.Text().MaxSize(16)),
				UpdatedAt: time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 1<<35)), 0).UTC(),
			}
			n := hegel.Draw(ht, hegel.Integers(0, 5))
			for range n {
				m.Segments = append(m.Segments, segmentMeta{
					Key:    hegel.Draw(ht, hegel.Text().MaxSize(16)),
					Size:   hegel.Draw(ht, hegel.Integers[int64](0, 1<<40)),
					Root:   blockRef{Offset: hegel.Draw(ht, hegel.Integers[int64](0, 1<<40)), Length: hegel.Draw(ht, hegel.Integers[int64](0, 1<<20))},
					MinKey: hegel.Draw(ht, hegel.Binary(0, 8)),
					MaxKey: hegel.Draw(ht, hegel.Binary(0, 8)),
					Count:  hegel.Draw(ht, hegel.Integers(0, 1<<30)),
				})
			}
			n = hegel.Draw(ht, hegel.Integers(0, 5))
			for range n {
				m.Garbage = append(m.Garbage, garbageEntry{
					Key:            hegel.Draw(ht, hegel.Text().MaxSize(16)),
					UnreferencedAt: time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 1<<35)), 0).UTC(),
				})
			}
			n = hegel.Draw(ht, hegel.Integers(0, 5))
			for range n {
				m.Commits = append(m.Commits, commitRecord{Seq: hegel.Draw(ht, hegel.Integers[uint64](0, 1<<62)), ID: hegel.Draw(ht, hegel.Text().MaxSize(16))})
			}
			b, err := encodeManifest(m)
			if err != nil {
				ht.Fatal(err)
			}
			got, err := decodeManifest(b)
			if err != nil {
				ht.Fatal(err)
			}
			if got.Seq != m.Seq || got.CommitID != m.CommitID || !got.UpdatedAt.Equal(m.UpdatedAt) || len(got.Segments) != len(m.Segments) || len(got.Garbage) != len(m.Garbage) || len(got.Commits) != len(m.Commits) {
				ht.Fatalf("got %+v, want %+v", got, m)
			}
			for i := range m.Segments {
				a, b := got.Segments[i], m.Segments[i]
				if a.Key != b.Key || a.Size != b.Size || a.Root != b.Root || !bytes.Equal(a.MinKey, b.MinKey) || !bytes.Equal(a.MaxKey, b.MaxKey) || a.Count != b.Count {
					ht.Fatalf("segment %d: got %+v, want %+v", i, a, b)
				}
			}
			for i := range m.Garbage {
				if got.Garbage[i].Key != m.Garbage[i].Key || !got.Garbage[i].UnreferencedAt.Equal(m.Garbage[i].UnreferencedAt) {
					ht.Fatalf("garbage %d: got %+v, want %+v", i, got.Garbage[i], m.Garbage[i])
				}
			}
			for i := range m.Commits {
				if got.Commits[i] != m.Commits[i] {
					ht.Fatalf("commit %d: got %+v, want %+v", i, got.Commits[i], m.Commits[i])
				}
			}
		}, hegel.WithTestCases(500))
	})

	t.Run("rejects arbitrary bytes and other versions without panicking", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			b := hegel.Draw(ht, hegel.Binary(0, 32))
			if _, err := decodeManifest(b); err == nil {
				ht.Fatalf("decoded %q", b)
			}
		}, hegel.WithTestCases(500))
		_, err := decodeManifest([]byte(`{"version":2}`))
		is.True(t, err != nil)
	})

	t.Run("next bumps the sequence, records the commit, and keeps the window bounded", func(t *testing.T) {
		m := &manifest{Version: manifestVersion}
		for i := range 128 + 10 {
			m = m.next(fmt.Sprint("c", i), time.Unix(int64(i), 0))
			is.Equal(t, uint64(i+1), m.Seq)
			c, ok := m.commitAt(m.Seq)
			is.True(t, ok)
			is.Equal(t, fmt.Sprint("c", i), c.ID)
		}
		is.Equal(t, 128, len(m.Commits))
		_, ok := m.commitAt(1)
		is.True(t, !ok, "oldest commit fell out of the window")
	})
}

func TestSegmentKey(t *testing.T) {
	t.Run("round-trips the base sequence and sorts by it", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			a := hegel.Draw(ht, hegel.Integers[uint64](0, 1<<63))
			b := hegel.Draw(ht, hegel.Integers[uint64](0, 1<<63))
			ka := segmentKey(a, "x")
			kb := segmentKey(b, "y")
			got, ok := segmentBaseSeq(ka)
			if !ok || got != a {
				ht.Fatalf("parse %q: %d %v", ka, got, ok)
			}
			if (a < b) != (ka < kb) && a != b {
				ht.Fatalf("order of %q and %q disagrees with %d and %d", ka, kb, a, b)
			}
		}, hegel.WithTestCases(500))
	})

	t.Run("rejects other keys", func(t *testing.T) {
		for _, k := range []string{"", "manifest", "seg/", "seg/zz-1", "other/0000000000000001-x", "seg/1-x", "seg/000000000000000F-x", "seg/00000000000000001-x"} {
			_, ok := segmentBaseSeq(k)
			is.True(t, !ok, k)
		}
	})
}
