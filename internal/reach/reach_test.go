package reach_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func TestHit(t *testing.T) {
	t.Run("counts in the counter of the context", func(t *testing.T) {
		var c reach.Counter
		ctx := reach.WithCounter(t.Context(), &c)
		reach.Hit(ctx, reach.GCRebase)
		reach.Hit(ctx, reach.GCRebase)
		reach.Hit(ctx, reach.LeaseOverlap)
		is.Equal(t, 2, c.Counts()[reach.GCRebase])
		is.Equal(t, 1, c.Counts()[reach.LeaseOverlap])
		is.Equal(t, 2, len(c.Counts()))
	})

	t.Run("does nothing on a context without a counter", func(t *testing.T) {
		reach.Hit(t.Context(), reach.GCRebase)
	})

	t.Run("returns a copy of the counts", func(t *testing.T) {
		var c reach.Counter
		reach.Hit(reach.WithCounter(t.Context(), &c), reach.GCRebase)
		counts := c.Counts()
		counts[reach.GCRebase] = 10
		is.Equal(t, 1, c.Counts()[reach.GCRebase])
	})
}

func TestDeclarations(t *testing.T) {
	t.Run("declares every state once, with a description and an assertion message", func(t *testing.T) {
		seen := map[reach.State]bool{}
		for _, d := range reach.Declarations() {
			is.True(t, !seen[d.State], string(d.State))
			seen[d.State] = true
			is.True(t, d.Description != "", string(d.State))
			is.True(t, d.Assertion != "", string(d.State))
		}
		is.Equal(t, 29, len(seen))
	})
}
