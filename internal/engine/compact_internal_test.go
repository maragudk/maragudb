package engine

import (
	"fmt"
	"slices"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/verify"
)

// sized segments, keyed by their index, with the given sizes, oldest first.
func sized(sizes ...int64) []segmentMeta {
	segs := make([]segmentMeta, len(sizes))
	for i, size := range sizes {
		segs[i] = segmentMeta{Key: fmt.Sprint(i), Size: size}
	}
	return segs
}

func TestTiers_plan(t *testing.T) {
	t.Parallel()

	tiers := Tiers{FanIn: 4, BaseBytes: 100}

	t.Run("puts segments below the base in tier 0, and each tier fan-in times larger than the one below", func(t *testing.T) {
		for size, want := range map[int64]int{0: 0, 99: 0, 100: 1, 399: 1, 400: 2, 1599: 2, 1600: 3, 1 << 62: 28} {
			is.Equal(t, want, tiers.withDefaults().tier(size), fmt.Sprint(size))
		}
	})

	t.Run("plans nothing for fewer than two segments", func(t *testing.T) {
		is.Equal(t, 0, len(tiers.plan(nil)))
		is.Equal(t, 0, len(tiers.plan(sized(10))))
	})

	t.Run("merges the whole database of at least fan-in segments when those after the oldest are together the full ratio times as large as it", func(t *testing.T) {
		is.Equal(t, "[{0 4}]", fmt.Sprint(tiers.plan(sized(1000, 300, 300, 400))))
		is.Equal(t, "[]", fmt.Sprint(tiers.plan(sized(1000, 300, 300, 399))))
		is.Equal(t, "[]", fmt.Sprint(tiers.plan(sized(1000, 2000))), "fewer than fan-in segments")
		ratio := Tiers{FanIn: 4, BaseBytes: 100, FullRatio: 2}
		is.Equal(t, "[]", fmt.Sprint(ratio.plan(sized(1000, 600, 600, 399))), "three segments of tier 2 and one of tier 1")
		is.Equal(t, "[{0 4}]", fmt.Sprint(ratio.plan(sized(1000, 700, 700, 700))))
	})

	t.Run("merges the whole database of at least fan-in segments when those after the oldest hold tombstones for the full ratio times as many of its entries as remain", func(t *testing.T) {
		segs := sized(100_000, 50, 50, 50)
		segs[0].Count = 1000
		segs[1].Tombstones, segs[2].Tombstones, segs[3].Tombstones = 150, 150, 199
		is.Equal(t, "[]", fmt.Sprint(tiers.plan(segs)))
		segs[3].Tombstones = 200
		is.Equal(t, "[{0 4}]", fmt.Sprint(tiers.plan(segs)), "half the entries")
		ratio := Tiers{FanIn: 4, BaseBytes: 100, FullRatio: 2}
		is.Equal(t, "[]", fmt.Sprint(ratio.plan(segs)))
		segs[3].Tombstones = 367
		is.Equal(t, "[{0 4}]", fmt.Sprint(ratio.plan(segs)), "two thirds of the entries")
	})

	t.Run("merges at least fan-in adjacent segments of one tier, as many as fit in the next tier, from the oldest of the tier, and leaves the others", func(t *testing.T) {
		// Tiers 5, 3, 3, 3, 3, 3, 0, 0, 0, where five segments of tier 3 do not fit in tier 4.
		is.Equal(t, "[{1 5}]", fmt.Sprint(tiers.plan(sized(100_000, 6000, 6000, 6000, 6000, 6000, 50, 50, 50))))
		// Two runs, of which the one of tier 0 has nine segments that all fit in tier 1.
		is.Equal(t, "[{1 5} {6 15}]", fmt.Sprint(tiers.plan(sized(100_000, 6000, 6000, 6000, 6000, 1000, 40, 40, 40, 40, 40, 40, 40, 40, 40))))
		// Windows of as many as fit in tier 1, below 400 bytes, and at least four.
		is.Equal(t, "[{1 5} {5 9}]", fmt.Sprint(tiers.plan(sized(100_000, 90, 90, 90, 90, 90, 90, 90, 90, 90))))
		is.Equal(t, "[{1 6} {6 11}]", fmt.Sprint(tiers.plan(sized(100_000, 70, 70, 70, 70, 70, 70, 70, 70, 70, 70, 70))))
	})

	t.Run("counts a segment in the tier of a newer one when that is higher", func(t *testing.T) {
		// The segment of 50 bytes is in tier 3, with the newer ones.
		is.Equal(t, "[{1 5}]", fmt.Sprint(tiers.plan(sized(100_000, 50, 6000, 6000, 6000))))
		// Newer, smaller segments do not lower the tier of older ones.
		is.Equal(t, "[]", fmt.Sprint(tiers.plan(sized(100_000, 5000, 5000, 5000, 50, 50, 50))))
	})

	t.Run("plans windows that do not overlap, each of fan-in adjacent segments or the whole database, and when merged until nothing is due, leaves fewer than fan-in segments per tier", func(t *testing.T) {
		verify.Covers(t, "random segment sizes, merged as planned, with outputs as large as their inputs or smaller, end with fewer than fan-in segments in each tier up to that of the largest segment", "AC-5.3")
		hegel.Test(t, func(ht *hegel.T) {
			tiers := Tiers{
				FanIn:     hegel.Draw(ht, hegel.Integers(2, 6)),
				BaseBytes: int64(hegel.Draw(ht, hegel.Integers(1, 1000))),
				FullRatio: hegel.Draw(ht, hegel.Integers(1, 8)),
			}
			sizes := hegel.Draw(ht, hegel.Lists(hegel.Integers(1, 1_000_000)).MinSize(0).MaxSize(60))
			var segs []segmentMeta
			for _, size := range sizes {
				segs = append(segs, segmentMeta{Size: int64(size)})
			}
			for rounds := 0; ; rounds++ {
				if rounds > 200 {
					ht.Fatalf("still merging after %d rounds: %v", rounds, segs)
				}
				ws := tiers.plan(segs)
				if len(ws) == 0 {
					break
				}
				var merges []merge
				for i, w := range ws {
					if w.start < 0 || w.end > len(segs) || w.end-w.start < 2 || (i > 0 && w.start < ws[i-1].end) {
						ht.Fatalf("window %v of %v for %d segments", w, ws, len(segs))
					}
					if w.end-w.start < tiers.withDefaults().FanIn && (w.start != 0 || w.end != len(segs)) {
						ht.Fatalf("window %v has fewer than fan-in segments, and not all %d", w, len(segs))
					}
					var size int64
					var members []int
					for j := w.start; j < w.end; j++ {
						size += segs[j].Size
						members = append(members, j)
					}
					// Overwrites and deletes can make the output smaller than its inputs.
					size = max(1, size*int64(hegel.Draw(ht, hegel.Integers(1, 4)))/4)
					merges = append(merges, merge{members: members, out: &segmentMeta{Size: size}})
				}
				segs = replaceMerged(segs, merges)
			}
			// Every tier, counted as the highest of a segment and the newer ones, holds fewer than
			// fan-in segments, and there are no more tiers than that of the largest segment.
			fanIn := tiers.withDefaults().FanIn
			var largest int64
			perTier := map[int]int{}
			tier := 0
			for _, seg := range slices.Backward(segs) {
				largest = max(largest, seg.Size)
				tier = max(tier, tiers.withDefaults().tier(seg.Size))
				perTier[tier]++
				if perTier[tier] >= fanIn {
					ht.Fatalf("%d segments in tier %d, fan-in %d: %v", perTier[tier], tier, fanIn, segs)
				}
			}
			if limit := (fanIn - 1) * (tiers.withDefaults().tier(largest) + 1); len(segs) > limit {
				ht.Fatalf("%d segments, more than %d, with largest %d: %v", len(segs), limit, largest, segs)
			}
		}, hegel.WithTestCases(300))
	})

	t.Run("merges each committed byte about once per tier, plus a bounded number of merges in tier 0 and of the whole database", func(t *testing.T) {
		verify.Covers(t, "random streams of commits, merged as planned after every commit without overwrites, read and write at most twice the committed bytes for every tier up to that of the largest segment, for the merges in tier 0, and for the merges of the whole database", "AC-6.5")
		hegel.Test(t, func(ht *hegel.T) {
			tiers := Tiers{
				FanIn:     hegel.Draw(ht, hegel.Integers(2, 6)),
				BaseBytes: int64(hegel.Draw(ht, hegel.Integers(1, 4096))),
				FullRatio: hegel.Draw(ht, hegel.Integers(1, 8)),
			}.withDefaults()
			const smallest = 50
			var segs []segmentMeta
			var committed, moved int64
			for range hegel.Draw(ht, hegel.Integers(1, 2000)) {
				size := int64(hegel.Draw(ht, hegel.Integers(smallest, 5000)))
				committed += size
				segs = append(segs, segmentMeta{Size: size})
				for {
					ws := tiers.plan(segs)
					if len(ws) == 0 {
						break
					}
					var merges []merge
					for _, w := range ws {
						var size int64
						var members []int
						for j := w.start; j < w.end; j++ {
							size += segs[j].Size
							members = append(members, j)
						}
						moved += 2 * size
						merges = append(merges, merge{members: members, out: &segmentMeta{Size: size}})
					}
					segs = replaceMerged(segs, merges)
				}
			}
			// A byte climbs a tier with every merge above tier 0. In tier 0, the merged segment grows
			// by at least fan-in minus one commits per merge until it leaves the tier. A merge of the
			// whole database reads and writes the oldest segment and the newer ones, which are at
			// least the full ratio times as large and were all committed since the last such merge.
			perByte := float64(tiers.tier(committed)+1) + float64(tiers.BaseBytes)/float64((tiers.FanIn-1)*smallest) + 1 + 1/float64(tiers.FullRatio)
			if float64(moved) > 2*perByte*float64(committed) {
				ht.Fatalf("%d bytes moved for %d committed, more than %.1f per byte", moved, committed, 2*perByte)
			}
		}, hegel.WithTestCases(200))
	})
}

func TestDB_members(t *testing.T) {
	t.Parallel()

	t.Run("are the windows' segments, or with the mutant, the windows without their second-oldest segment and with the one after them", func(t *testing.T) {
		db := &DB{}
		is.Equal(t, "[[0 1 2] [3 4]]", fmt.Sprint(db.members([]window{{0, 3}, {3, 5}}, 6)))
		db.mutations.CompactNonAdjacent = true
		is.Equal(t, "[[0 2 3] [3 5]]", fmt.Sprint(db.members([]window{{0, 3}, {3, 5}}, 6)))
		is.Equal(t, "[]", fmt.Sprint(db.members([]window{{4, 6}}, 6)))
	})
}

func TestReplaceMerged(t *testing.T) {
	t.Parallel()

	t.Run("puts each output in the place of its oldest member and drops the other members and empty outputs", func(t *testing.T) {
		segs := sized(0, 1, 2, 3, 4, 5, 6)
		got := replaceMerged(segs, []merge{
			{members: []int{1, 2, 3}, out: &segmentMeta{Key: "a"}},
			{members: []int{4, 5}},
		})
		var keys []string
		for _, seg := range got {
			keys = append(keys, seg.Key)
		}
		is.Equal(t, "[0 a 6]", fmt.Sprint(keys))
	})
}
