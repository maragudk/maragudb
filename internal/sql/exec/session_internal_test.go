package exec

import (
	"fmt"
	"slices"
	"testing"

	"hegel.dev/go/hegel"
)

func TestSortedKeys(t *testing.T) {
	t.Run("returns the keys of any range in order, after any sequence of adds", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			var s sortedKeys
			var all []string
			for range hegel.Draw(ht, hegel.Integers(0, 300)) {
				k := fmt.Sprintf("%04d", hegel.Draw(ht, hegel.Integers(0, 999)))
				if slices.Contains(all, k) {
					continue
				}
				s.add(k)
				all = append(all, k)
			}
			slices.Sort(all)
			start := fmt.Sprintf("%04d", hegel.Draw(ht, hegel.Integers(0, 1000)))
			end := fmt.Sprintf("%04d", hegel.Draw(ht, hegel.Integers(0, 1000)))
			var want []string
			for _, k := range all {
				if k >= start && k < end {
					want = append(want, k)
				}
			}
			if got := s.between(start, end); !slices.Equal(got, want) {
				ht.Fatalf("between(%q, %q) = %q, want %q", start, end, got, want)
			}
			for i := 1; i < len(s.runs); i++ {
				if len(s.runs[i]) > len(s.runs[i-1])/2 {
					ht.Fatalf("run %d has %d keys after a run of %d", i, len(s.runs[i]), len(s.runs[i-1]))
				}
			}
		})
	})
}
