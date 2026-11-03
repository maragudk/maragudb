package engine_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"hegel.dev/go/hegel"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3fake"
)

// model of the committed key space.
type model map[string][]byte

func (m model) scan(start, end []byte) []string {
	var keys []string
	for k := range m {
		if (start == nil || bytes.Compare([]byte(k), start) >= 0) && (end == nil || bytes.Compare([]byte(k), end) < 0) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func (m model) clone() model {
	c := model{}
	for k, v := range m {
		c[k] = v
	}
	return c
}

func TestEngine_MatchesModel(t *testing.T) {
	t.Run("transactions over several processes with compaction and GC behave like a map", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			clock := s3fake.NewVirtualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			fake := s3fake.New(s3fake.WithClock(clock))
			blockSize := hegel.Draw(ht, hegel.Integers(16, 512))
			grace := time.Hour
			var dbs []*engine.DB
			for i := range hegel.Draw(ht, hegel.Integers(1, 3)) {
				memBytes := int64(hegel.Draw(ht, hegel.Integers(0, 1<<16)))
				db, err := engine.Open(t.Context(), engine.Options{
					Client: fake, Prefix: "db", Now: clock.Now, BlockSize: &blockSize, GCGracePeriod: &grace,
					MemoryCacheBytes: &memBytes, Rand: rand.New(rand.NewPCG(uint64(i), 1)),
				})
				if err != nil {
					ht.Fatal(err)
				}
				dbs = append(dbs, db)
			}
			committed := model{}
			keys := hegel.Draw(ht, hegel.Lists(hegel.Binary(0, 4)).MinSize(1).MaxSize(8))
			nTx := hegel.Draw(ht, hegel.Integers(1, 25))
			for range nTx {
				db := dbs[hegel.Draw(ht, hegel.Integers(0, len(dbs)-1))]
				switch hegel.Draw(ht, hegel.Integers(0, 9)) {
				case 0:
					if err := db.Compact(t.Context()); err != nil {
						ht.Fatalf("compact: %v", err)
					}
					continue
				case 1:
					clock.Sleep(t.Context(), time.Duration(hegel.Draw(ht, hegel.Integers(0, 2)))*grace)
					if _, err := db.GC(t.Context()); err != nil {
						ht.Fatalf("gc: %v", err)
					}
					continue
				}
				tx, err := db.Begin(t.Context())
				if err != nil {
					ht.Fatal(err)
				}
				pending := committed.clone()
				nOps := hegel.Draw(ht, hegel.Integers(1, 6))
				for range nOps {
					key := hegel.Draw(ht, hegel.SampledFrom(keys))
					switch hegel.Draw(ht, hegel.Integers(0, 3)) {
					case 0:
						value := hegel.Draw(ht, hegel.Binary(0, 6))
						if err := tx.Put(key, value); err != nil {
							ht.Fatal(err)
						}
						pending[string(key)] = value
					case 1:
						if err := tx.Delete(key); err != nil {
							ht.Fatal(err)
						}
						delete(pending, string(key))
					case 2:
						got, ok, err := tx.Get(t.Context(), key)
						if err != nil {
							ht.Fatal(err)
						}
						want, wantOK := pending[string(key)]
						if ok != wantOK || !bytes.Equal(got, want) {
							ht.Fatalf("get %q: got %q %v, want %q %v", key, got, ok, want, wantOK)
						}
					case 3:
						var start, end []byte
						if hegel.Draw(ht, hegel.Booleans()) {
							start = hegel.Draw(ht, hegel.Binary(0, 4))
						}
						if hegel.Draw(ht, hegel.Booleans()) {
							end = hegel.Draw(ht, hegel.Binary(0, 4))
						}
						it := tx.Scan(t.Context(), start, end)
						var got []string
						for it.Next() {
							got = append(got, string(it.Key()))
							if !bytes.Equal(it.Value(), pending[string(it.Key())]) {
								ht.Fatalf("scan value of %q: got %q, want %q", it.Key(), it.Value(), pending[string(it.Key())])
							}
						}
						if it.Err() != nil {
							ht.Fatal(it.Err())
						}
						want := pending.scan(start, end)
						if fmt.Sprint(got) != fmt.Sprint(want) {
							ht.Fatalf("scan [%q, %q): got %q, want %q", start, end, got, want)
						}
					}
				}
				if hegel.Draw(ht, hegel.WeightedBooleans(0.8)) {
					if err := tx.Commit(t.Context()); err != nil {
						ht.Fatalf("commit: %v", err)
					}
					committed = pending
				} else {
					if err := tx.Rollback(); err != nil {
						ht.Fatal(err)
					}
				}
			}
			// A fresh process with no cache sees exactly the committed state.
			fresh, err := engine.Open(t.Context(), engine.Options{Client: fake, Prefix: "db", Now: clock.Now, MemoryCacheBytes: new(int64)})
			if err != nil {
				ht.Fatal(err)
			}
			tx, err := fresh.BeginReadOnly(t.Context())
			if err != nil {
				ht.Fatal(err)
			}
			it := tx.Scan(t.Context(), nil, nil)
			var got []string
			for it.Next() {
				got = append(got, string(it.Key()))
				if !bytes.Equal(it.Value(), committed[string(it.Key())]) {
					ht.Fatalf("final value of %q: got %q, want %q", it.Key(), it.Value(), committed[string(it.Key())])
				}
			}
			if it.Err() != nil {
				ht.Fatal(it.Err())
			}
			if fmt.Sprint(got) != fmt.Sprint(committed.scan(nil, nil)) {
				ht.Fatalf("final keys: got %q, want %q", got, committed.scan(nil, nil))
			}
			// Objects are bounded: at most the manifest, the live segments, and the garbage.
			stats := fresh.Stats()
			if n := len(fake.Keys()); n > 1+stats.Segments+stats.Garbage {
				ht.Fatalf("%d objects for %d segments and %d garbage", n, stats.Segments, stats.Garbage)
			}
		}, hegel.WithTestCases(300))
	})

	t.Run("conflicting commits from two processes leave exactly one winner", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			clock := s3fake.NewVirtualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			fake := s3fake.New(s3fake.WithClock(clock))
			var dbs []*engine.DB
			for i := range 2 {
				db, err := engine.Open(t.Context(), engine.Options{Client: fake, Prefix: "db", Now: clock.Now, Rand: rand.New(rand.NewPCG(uint64(i), 1))})
				if err != nil {
					ht.Fatal(err)
				}
				dbs = append(dbs, db)
			}
			committed := model{}
			for range hegel.Draw(ht, hegel.Integers(1, 10)) {
				tx0, err := dbs[0].Begin(t.Context())
				if err != nil {
					ht.Fatal(err)
				}
				tx1, err := dbs[1].Begin(t.Context())
				if err != nil {
					ht.Fatal(err)
				}
				v0 := hegel.Draw(ht, hegel.Binary(1, 4))
				v1 := hegel.Draw(ht, hegel.Binary(1, 4))
				if err := tx0.Put([]byte("k"), v0); err != nil {
					ht.Fatal(err)
				}
				if err := tx1.Put([]byte("k"), v1); err != nil {
					ht.Fatal(err)
				}
				first, second := tx0, tx1
				winner := v0
				if hegel.Draw(ht, hegel.Booleans()) {
					first, second = tx1, tx0
					winner = v1
				}
				if err := first.Commit(t.Context()); err != nil {
					ht.Fatal(err)
				}
				if err := second.Commit(t.Context()); !errors.Is(err, engine.ErrConflict) {
					ht.Fatalf("second commit: got %v, want ErrConflict", err)
				}
				committed["k"] = winner
				for _, db := range dbs {
					tx, err := db.BeginReadOnly(t.Context())
					if err != nil {
						ht.Fatal(err)
					}
					got, ok, err := tx.Get(t.Context(), []byte("k"))
					if err != nil || !ok || !bytes.Equal(got, winner) {
						ht.Fatalf("got %q %v %v, want %q", got, ok, err, winner)
					}
				}
			}
		}, hegel.WithTestCases(200))
	})
}
