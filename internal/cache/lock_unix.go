//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package cache

import (
	"errors"
	"os"
	"syscall"
)

// lockFile at path exclusively without waiting, creating it if missing, and return the unlock.
// It fails with [ErrInUse] when another open file holds the lock. The lock goes away with the
// process.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrInUse
		}
		return nil, err
	}
	// Closing the file releases the lock.
	return func() { _ = f.Close() }, nil
}
