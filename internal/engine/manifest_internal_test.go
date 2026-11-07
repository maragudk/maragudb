package engine

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/verify"
)

func TestManifest(t *testing.T) {
	t.Parallel()

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
				size := hegel.Draw(ht, hegel.Integers[int64](minBlockBytes, 1<<40))
				length := hegel.Draw(ht, hegel.Integers[int64](minBlockBytes, size))
				keys := [][]byte{hegel.Draw(ht, hegel.Binary(0, 8)), hegel.Draw(ht, hegel.Binary(0, 8))}
				if bytes.Compare(keys[0], keys[1]) > 0 {
					keys[0], keys[1] = keys[1], keys[0]
				}
				count := hegel.Draw(ht, hegel.Integers(0, 1<<30))
				m.Segments = append(m.Segments, segmentMeta{
					Key:        hegel.Draw(ht, hegel.Text().MinSize(1).MaxSize(16)),
					Size:       size,
					Root:       blockRef{Offset: hegel.Draw(ht, hegel.Integers[int64](0, size-length)), Length: length},
					MinKey:     keys[0],
					MaxKey:     keys[1],
					Count:      count,
					Tombstones: hegel.Draw(ht, hegel.Integers(0, count)),
				})
			}
			n = hegel.Draw(ht, hegel.Integers(0, 5))
			for range n {
				m.Garbage = append(m.Garbage, hegel.Draw(ht, hegel.Text().MaxSize(16)))
			}
			n = hegel.Draw(ht, hegel.Integers(0, 5))
			for range n {
				m.Batches = append(m.Batches, batchRef{
					Key:       hegel.Draw(ht, hegel.Text().MinSize(1).MaxSize(16)),
					Since:     time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 1<<35)), 0).UTC(),
					StampedAt: time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 1<<35)), 0).UTC(),
					Objects:   hegel.Draw(ht, hegel.Integers(0, 1<<30)),
				})
			}
			n = hegel.Draw(ht, hegel.Integers(0, 5))
			for i := range n {
				if seq := m.Seq - uint64(n-1-i); seq <= m.Seq {
					m.Commits = append(m.Commits, commitRecord{Seq: seq, ID: hegel.Draw(ht, hegel.Text().MaxSize(16))})
				}
			}
			b, err := encodeManifest("db/manifest", m)
			if err != nil {
				ht.Fatal(err)
			}
			got, err := decodeManifest("db/manifest", b)
			if err != nil {
				ht.Fatal(err)
			}
			if got.Seq != m.Seq || got.CommitID != m.CommitID || !got.UpdatedAt.Equal(m.UpdatedAt) || len(got.Segments) != len(m.Segments) || len(got.Garbage) != len(m.Garbage) || len(got.Batches) != len(m.Batches) || len(got.Commits) != len(m.Commits) {
				ht.Fatalf("got %+v, want %+v", got, m)
			}
			for i := range m.Segments {
				a, b := got.Segments[i], m.Segments[i]
				if a.Key != b.Key || a.Size != b.Size || a.Root != b.Root || !bytes.Equal(a.MinKey, b.MinKey) || !bytes.Equal(a.MaxKey, b.MaxKey) || a.Count != b.Count || a.Tombstones != b.Tombstones {
					ht.Fatalf("segment %d: got %+v, want %+v", i, a, b)
				}
			}
			for i := range m.Garbage {
				if got.Garbage[i] != m.Garbage[i] {
					ht.Fatalf("garbage %d: got %q, want %q", i, got.Garbage[i], m.Garbage[i])
				}
			}
			for i := range m.Batches {
				a, b := got.Batches[i], m.Batches[i]
				if a.Key != b.Key || !a.Since.Equal(b.Since) || !a.StampedAt.Equal(b.StampedAt) || a.Objects != b.Objects {
					ht.Fatalf("batch %d: got %+v, want %+v", i, a, b)
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
			if _, err := decodeManifest("db/manifest", b); err == nil {
				ht.Fatalf("decoded %q", b)
			}
		}, hegel.WithTestCases(500))
		_, err := decodeManifest("db/manifest", seal(manifestMagic, "db/manifest", []byte(`{"version":4}`)))
		is.Error(t, ErrUnsupportedManifest, err)
		_, err = decodeManifest("db/manifest", []byte(`{"version":3}`))
		is.Error(t, ErrCorrupt, err)
	})

	t.Run("rejects manifests of versions 1 and 2, which are plain JSON, with an error that names the versions", func(t *testing.T) {
		_, err := decodeManifest("db/manifest", []byte(`{"version":1,"seq":7,"commitID":"c","segments":[],"garbage":[{"key":"db/seg/0000000000000001-a","unreferencedAt":"2026-01-01T00:00:00Z"}],"commits":[{"seq":7,"id":"c"}]}`))
		is.Error(t, ErrUnsupportedManifest, err)
		is.Equal(t, "engine: unsupported manifest version: the manifest has version 1, and this version of maragudb reads only version 3", err.Error())
		_, err = decodeManifest("db/manifest", []byte(`{"version":2,"seq":7,"commitID":"c","segments":[{"key":"db/seg/0000000000000001-a","size":40,"root":{"o":0,"l":40},"minKey":"YQ==","maxKey":"YQ==","count":1}],"commits":[{"seq":7,"id":"c"}],"updatedAt":"2026-01-01T00:00:00Z"}`))
		is.Error(t, ErrUnsupportedManifest, err)
		is.Equal(t, "engine: unsupported manifest version: the manifest has version 2, and this version of maragudb reads only version 3", err.Error())
	})

	t.Run("fails with ErrCorrupt when a bit of the manifest flips, when it is cut short, or when it is read from another key", func(t *testing.T) {
		verify.Covers(t, "every bit flip and every truncation of a manifest, and a manifest read from another key, fail with ErrCorrupt", "INV-DAMAGE-DETECTED")
		m := &manifest{Version: manifestVersion, Seq: 2, CommitID: "c2", Segments: []segmentMeta{{Key: "db/seg/0000000000000001-a", Size: 40, Root: blockRef{Offset: 0, Length: 40}, MinKey: []byte("a"), MaxKey: []byte("b"), Count: 2}}, Garbage: []string{"db/seg/0000000000000000-b"}, Batches: []batchRef{{Key: "db/gc/0000000000000001-c", Objects: 3}}, Commits: []commitRecord{{Seq: 1, ID: "c1"}, {Seq: 2, ID: "c2"}}}
		b, err := encodeManifest("db/manifest", m)
		is.NotError(t, err)
		_, err = decodeManifest("db/manifest", b)
		is.NotError(t, err)
		for i := range b {
			for bit := range 8 {
				damaged := bytes.Clone(b)
				damaged[i] ^= 1 << bit
				_, err := decodeManifest("db/manifest", damaged)
				is.Error(t, ErrCorrupt, err, fmt.Sprintf("bit %d of byte %d", bit, i))
			}
		}
		for n := range len(b) {
			_, err := decodeManifest("db/manifest", b[:n])
			is.Error(t, ErrCorrupt, err, fmt.Sprintf("cut to %d bytes", n))
		}
		_, err = decodeManifest("other/manifest", b)
		is.Error(t, ErrCorrupt, err)
	})

	t.Run("fails with ErrCorrupt on a sealed manifest that does not hold up", func(t *testing.T) {
		for _, j := range []string{
			`{"version":3,"seq":1,"segments":[{"key":"","size":40,"root":{"o":0,"l":40},"count":1}]}`,
			`{"version":3,"seq":1,"segments":[{"key":"s","size":40,"root":{"o":10,"l":40},"count":1}]}`,
			`{"version":3,"seq":1,"segments":[{"key":"s","size":40,"root":{"o":-1,"l":6},"count":1}]}`,
			`{"version":3,"seq":1,"segments":[{"key":"s","size":40,"root":{"o":0,"l":5},"count":1}]}`,
			`{"version":3,"seq":1,"segments":[{"key":"s","size":40,"root":{"o":0,"l":40},"count":1,"tombstones":2}]}`,
			`{"version":3,"seq":1,"segments":[{"key":"s","size":40,"root":{"o":0,"l":40},"minKey":"Yg==","maxKey":"YQ==","count":1}]}`,
			`{"version":3,"seq":1,"batches":[{"key":"b","objects":-1}]}`,
			`{"version":3,"seq":1,"commits":[{"seq":2,"id":"c"}]}`,
			`{"version":3,"seq":3,"commits":[{"seq":2,"id":"c"},{"seq":2,"id":"d"}]}`,
			`{"version":3,"seq":"x"}`,
		} {
			_, err := decodeManifest("db/manifest", seal(manifestMagic, "db/manifest", []byte(j)))
			is.Error(t, ErrCorrupt, err, j)
		}
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

func TestGarbageBatch(t *testing.T) {
	t.Parallel()

	t.Run("encode then decode round-trips", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			b := &garbageBatch{
				Version:   garbageBatchVersion,
				StampedAt: time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 1<<35)), 0).UTC(),
				Prev:      hegel.Draw(ht, hegel.Text().MaxSize(16)),
			}
			for range hegel.Draw(ht, hegel.Integers(0, 5)) {
				b.Keys = append(b.Keys, hegel.Draw(ht, hegel.Text().MaxSize(16)))
			}
			body, err := encodeGarbageBatch("db/gc/0000000000000001-a", b)
			if err != nil {
				ht.Fatal(err)
			}
			got, err := decodeGarbageBatch("db/gc/0000000000000001-a", body)
			if err != nil {
				ht.Fatal(err)
			}
			if !got.StampedAt.Equal(b.StampedAt) || got.Prev != b.Prev || fmt.Sprint(got.Keys) != fmt.Sprint(b.Keys) {
				ht.Fatalf("got %+v, want %+v", got, b)
			}
		}, hegel.WithTestCases(200))
	})

	t.Run("rejects arbitrary bytes and other versions", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			b := hegel.Draw(ht, hegel.Binary(0, 32))
			if _, err := decodeGarbageBatch("db/gc/a", b); err == nil {
				ht.Fatalf("decoded %q", b)
			}
		}, hegel.WithTestCases(200))
		_, err := decodeGarbageBatch("db/gc/a", seal(garbageBatchMagic, "db/gc/a", []byte(`{"version":3,"keys":["a"]}`)))
		is.Error(t, ErrUnsupportedManifest, err)
		_, err = decodeGarbageBatch("db/gc/a", []byte(`{"version":1,"keys":["a"]}`))
		is.Error(t, ErrCorrupt, err)
		_, err = decodeGarbageBatch("db/gc/a", seal(garbageBatchMagic, "db/gc/a", []byte(`{"version":2,"keys":["a"],"prev":"db/gc/a"}`)))
		is.Error(t, ErrCorrupt, err)
	})

	t.Run("fails with ErrCorrupt when a bit of the batch flips, when it is cut short, or when it is read from another key", func(t *testing.T) {
		verify.Covers(t, "every bit flip and every truncation of a garbage batch, and a garbage batch read from another key, fail with ErrCorrupt", "INV-DAMAGE-DETECTED")
		key := "db/gc/0000000000000001-a"
		b, err := encodeGarbageBatch(key, &garbageBatch{Version: garbageBatchVersion, StampedAt: time.Unix(100, 0).UTC(), Keys: []string{"db/seg/0000000000000000-x", "db/seg/0000000000000001-y"}, Prev: "db/gc/0000000000000000-p"})
		is.NotError(t, err)
		for i := range b {
			for bit := range 8 {
				damaged := bytes.Clone(b)
				damaged[i] ^= 1 << bit
				_, err := decodeGarbageBatch(key, damaged)
				is.Error(t, ErrCorrupt, err, fmt.Sprintf("bit %d of byte %d", bit, i))
			}
		}
		for n := range len(b) {
			_, err := decodeGarbageBatch(key, b[:n])
			is.Error(t, ErrCorrupt, err, fmt.Sprintf("cut to %d bytes", n))
		}
		_, err = decodeGarbageBatch("db/gc/0000000000000001-b", b)
		is.Error(t, ErrCorrupt, err)
	})
}

// FuzzManifest decodes arbitrary bytes as a manifest, which never panics and fails with
// [ErrCorrupt] or [ErrUnsupportedManifest] when it fails. Half of the inputs are sealed, so that
// the fuzzer reaches the decoding behind the checksum.
func FuzzManifest(f *testing.F) {
	b, err := encodeManifest("db/manifest", &manifest{Version: manifestVersion, Seq: 2, Segments: []segmentMeta{{Key: "db/seg/a", Size: 40, Root: blockRef{Offset: 0, Length: 40}, Count: 1}}, Batches: []batchRef{{Key: "db/gc/b", Objects: 1}}, Commits: []commitRecord{{Seq: 2, ID: "c"}}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b, false)
	f.Add(b[sealBytes:], true)
	f.Add([]byte(`{"version":2}`), false)
	f.Fuzz(func(t *testing.T, b []byte, sealed bool) {
		if sealed {
			b = seal(manifestMagic, "db/manifest", b)
		}
		m, err := decodeManifest("db/manifest", b)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupportedManifest) {
				t.Fatalf("error %v wraps neither ErrCorrupt nor ErrUnsupportedManifest", err)
			}
			return
		}
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		again, err := encodeManifest("db/manifest", m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeManifest("db/manifest", again); err != nil {
			t.Fatalf("a decoded manifest does not decode again: %v", err)
		}
	})
}

// FuzzGarbageBatch decodes arbitrary bytes as a garbage batch, which never panics and fails with
// [ErrCorrupt] or [ErrUnsupportedManifest] when it fails.
func FuzzGarbageBatch(f *testing.F) {
	b, err := encodeGarbageBatch("db/gc/a", &garbageBatch{Version: garbageBatchVersion, Keys: []string{"db/seg/x"}, Prev: "db/gc/p"})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b, false)
	f.Add(b[sealBytes:], true)
	f.Fuzz(func(t *testing.T, b []byte, sealed bool) {
		if sealed {
			b = seal(garbageBatchMagic, "db/gc/a", b)
		}
		g, err := decodeGarbageBatch("db/gc/a", b)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupportedManifest) {
				t.Fatalf("error %v wraps neither ErrCorrupt nor ErrUnsupportedManifest", err)
			}
			return
		}
		if g.Prev == "db/gc/a" {
			t.Fatal("a batch names itself")
		}
	})
}

func TestObjectKey(t *testing.T) {
	t.Parallel()

	t.Run("round-trips the base sequence of segment and garbage batch keys and sorts by it", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			a := hegel.Draw(ht, hegel.Integers[uint64](0, 1<<63))
			b := hegel.Draw(ht, hegel.Integers[uint64](0, 1<<63))
			key := segmentKey
			if hegel.Draw(ht, hegel.Booleans()) {
				key = garbageBatchKey
			}
			ka := key(a, "x")
			kb := key(b, "y")
			got, ok := objectBaseSeq(ka)
			if !ok || got != a {
				ht.Fatalf("parse %q: %d %v", ka, got, ok)
			}
			if (a < b) != (ka < kb) && a != b {
				ht.Fatalf("order of %q and %q disagrees with %d and %d", ka, kb, a, b)
			}
		}, hegel.WithTestCases(500))
	})

	t.Run("rejects other keys", func(t *testing.T) {
		for _, k := range []string{"", "manifest", "maintenance-lease", "seg/", "gc/", "seg/zz-1", "gc/zz-1", "other/0000000000000001-x", "seg/1-x", "seg/000000000000000F-x", "gc/000000000000000F-x", "seg/00000000000000001-x"} {
			_, ok := objectBaseSeq(k)
			is.True(t, !ok, k)
		}
	})
}
