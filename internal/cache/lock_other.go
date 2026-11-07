//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package cache

// lockFile does nothing where there is no file lock, so nothing keeps two processes from
// opening one disk cache directory, whose bound then holds for each process alone.
func lockFile(string) (func(), error) {
	return func() {}, nil
}
