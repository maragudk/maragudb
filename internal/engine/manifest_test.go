package engine_test

import (
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
)

func TestManifestSegments(t *testing.T) {
	t.Run("are the keys of the live segments of the manifest in the store, oldest first", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1")
		put(t, db, "b", "2")
		body, ok := w.fake.Body("db/manifest")
		is.True(t, ok)
		keys, err := engine.ManifestSegments("db/manifest", body)
		is.NotError(t, err)
		is.EqualSlice(t, keysWith(w, "db/seg/"), keys)
		is.Equal(t, 2, len(keys))
	})

	t.Run("fails with ErrCorrupt for damaged bytes or another key", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1")
		body, _ := w.fake.Body("db/manifest")
		_, err := engine.ManifestSegments("other/manifest", body)
		is.Error(t, engine.ErrCorrupt, err)
		body[len(body)-1] ^= 1
		_, err = engine.ManifestSegments("db/manifest", body)
		is.Error(t, engine.ErrCorrupt, err)
	})
}

func TestManifestContents(t *testing.T) {
	t.Run("is every key of the database at the manifest with its newest value, without the deleted ones, before and after a compaction", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{BlockSize: new(64)})
		put(t, db, "a", "1", "b", "2", "c", "3")
		put(t, db, "a", "4")
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Delete([]byte("b")))
		is.NotError(t, tx.Commit(t.Context()))
		before, _ := w.fake.Body("db/manifest")
		got, err := engine.ManifestContents("db/manifest", before, w.fake.Body)
		is.NotError(t, err)
		is.Equal(t, "map[a:4 c:3]", fmt.Sprint(got))

		is.NotError(t, db.Compact(t.Context()))
		after, _ := w.fake.Body("db/manifest")
		is.True(t, string(after) != string(before))
		got, err = engine.ManifestContents("db/manifest", after, w.fake.Body)
		is.NotError(t, err)
		is.Equal(t, "map[a:4 c:3]", fmt.Sprint(got))
	})

	t.Run("fails with ErrSnapshotExpired when a segment is missing", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1")
		body, _ := w.fake.Body("db/manifest")
		_, err := engine.ManifestContents("db/manifest", body, func(string) ([]byte, bool) { return nil, false })
		is.Error(t, engine.ErrSnapshotExpired, err)
	})
}
