package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// ErrInUse is a disk cache directory that another process has open.
var ErrInUse = errors.New("cache: the disk cache directory is in use by another process")

// dirFS is an [FS] on a directory of the operating system.
type dirFS struct {
	dir string
}

func (f dirFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.dir, name))
}

func (f dirFS) WriteTemp(data []byte) (string, error) {
	tmp, err := os.CreateTemp(f.dir, TempPrefix+"*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return filepath.Base(tmp.Name()), nil
}

func (f dirFS) Rename(from, to string) error {
	return os.Rename(filepath.Join(f.dir, from), filepath.Join(f.dir, to))
}

func (f dirFS) Remove(name string) error {
	return os.Remove(filepath.Join(f.dir, name))
}

func (f dirFS) List() ([]FileInfo, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	var files []FileInfo
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files = append(files, FileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	return files, nil
}

// lockName is the file in a disk cache directory that the process with the directory open
// holds a lock on.
const lockName = "lock"

// openDirs are the disk cache directories that this process has open, by path.
var openDirs = struct {
	sync.Mutex
	m map[string]*openDir
}{m: map[string]*openDir{}}

type openDir struct {
	disk   *Disk
	unlock func()
	refs   int
}

// OpenDir opens the disk cache in dir, which is created if missing, with at most maxBytes in its
// files, as [NewDisk] counts them. Only one process can have a directory open, and OpenDir fails
// with [ErrInUse] while another process has it, because the bound of a cache counts the files of
// its own process only. The lock is on a file in dir, and it goes away with the process that holds
// it, also when it crashes. Within one process, the OpenDirs of one directory share one [Disk],
// with the bound of the first. The returned function closes the cache, and the last close of a
// directory releases its lock.
func OpenDir(dir string, maxBytes int64) (*Disk, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("cache: creating directory %q: %w", dir, err)
	}
	path, err := filepath.Abs(dir)
	if err == nil {
		path, err = filepath.EvalSymlinks(path)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cache: resolving directory %q: %w", dir, err)
	}
	openDirs.Lock()
	defer openDirs.Unlock()
	od, ok := openDirs.m[path]
	if !ok {
		unlock, err := lockFile(filepath.Join(path, lockName))
		if errors.Is(err, ErrInUse) {
			return nil, nil, fmt.Errorf("%w: %q; give each process a DiskCacheDir of its own", ErrInUse, path)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("cache: locking directory %q: %w", path, err)
		}
		disk, err := NewDisk(dirFS{dir: path}, DiskOptions{MaxBytes: maxBytes})
		if err != nil {
			unlock()
			return nil, nil, err
		}
		od = &openDir{disk: disk, unlock: unlock}
		openDirs.m[path] = od
	}
	od.refs++
	var once sync.Once
	return od.disk, func() {
		once.Do(func() {
			openDirs.Lock()
			defer openDirs.Unlock()
			od.refs--
			if od.refs == 0 {
				delete(openDirs.m, path)
				od.unlock()
			}
		})
	}, nil
}
