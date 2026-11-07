//go:build windows

package cache

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile at path exclusively without waiting, creating it if missing, and return the unlock.
// It fails with [ErrInUse] when another open file holds the lock. The lock goes away with the
// process.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrInUse
		}
		return nil, err
	}
	// Closing the file releases the lock.
	return func() { _ = f.Close() }, nil
}
