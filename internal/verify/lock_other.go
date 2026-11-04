//go:build !unix

package verify

// lock does nothing where there is no flock, so concurrent test binaries may overwrite each
// other's report there.
func lock(string) (func(), error) {
	return func() {}, nil
}
