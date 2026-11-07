package maragudb

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/verify"
)

func TestOptions_withDefaults(t *testing.T) {
	t.Run("bounds the disk cache to 1 GiB by default, and keeps a bound that is set", func(t *testing.T) {
		verify.Covers(t, "DiskCacheBytes defaults to 1 GiB, and a bound that is set, also zero, stays", "INV-OPTIONS")
		is.Equal(t, int64(1<<30), *Options{}.withDefaults().DiskCacheBytes)
		is.Equal(t, int64(0), *Options{DiskCacheBytes: new(int64)}.withDefaults().DiskCacheBytes)
	})
}
