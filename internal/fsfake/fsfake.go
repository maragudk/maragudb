// Package fsfake is a flat directory of files in memory, an [cache.FS] for the disk cache in
// tests and the simulator. It is deterministic, and it can do what a real file system does to a
// cache: damage files, and cut a read short when the file is removed in the middle of it.
package fsfake

import (
	"bytes"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"time"

	"maragu.dev/maragudb/internal/cache"
)

// FS in memory. The zero value is an empty directory. It is safe for concurrent use.
type FS struct {
	// OnRead, when set, runs in the middle of every [FS.ReadFile] of a file that exists, with
	// the file's name, after the read has begun. It may change the directory.
	OnRead func(name string)
	// CutReads ends a read early when its file is removed or replaced while the read runs, as on
	// file systems that do not keep a removed file readable. Otherwise a read that has begun gets
	// the whole file it began on, as on POSIX file systems.
	CutReads bool
	// RemoveErr, when set, is the error of every [FS.Remove] of a file that exists, unless it
	// returns nil, in which case the file is removed.
	RemoveErr func(name string) error

	mu    sync.Mutex
	files map[string]file
	// clock orders the writes, and temps names the temporary files.
	clock, temps int64
}

type file struct {
	data []byte
	// written is the clock of the write, which is both the modification time and the identity of
	// the file under its name.
	written int64
}

var _ cache.FS = (*FS)(nil)

func notExist(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
}

// ReadFile returns a copy of the file's content.
func (f *FS) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	began, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		return nil, notExist("open", name)
	}
	if f.OnRead != nil {
		f.OnRead(name)
	}
	if f.CutReads {
		f.mu.Lock()
		now, ok := f.files[name]
		f.mu.Unlock()
		if !ok || now.written != began.written {
			return bytes.Clone(began.data[:len(began.data)/2]), nil
		}
	}
	return bytes.Clone(began.data), nil
}

// WriteTemp writes a new temporary file.
func (f *FS) WriteTemp(data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.temps++
	name := fmt.Sprint(cache.TempPrefix, f.temps)
	f.write(name, bytes.Clone(data))
	return name, nil
}

func (f *FS) write(name string, data []byte) {
	if f.files == nil {
		f.files = map[string]file{}
	}
	f.clock++
	f.files[name] = file{data: data, written: f.clock}
}

// Rename a file, replacing any file named to.
func (f *FS) Rename(from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[from]
	if !ok {
		return notExist("rename", from)
	}
	delete(f.files, from)
	f.files[to] = file
	return nil
}

// Remove a file.
func (f *FS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[name]; !ok {
		return notExist("remove", name)
	}
	if f.RemoveErr != nil {
		if err := f.RemoveErr(name); err != nil {
			return err
		}
	}
	delete(f.files, name)
	return nil
}

// List the files in the order of their names, with the clock of their last write as their
// modification time.
func (f *FS) List() ([]cache.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var infos []cache.FileInfo
	for _, name := range f.names() {
		file := f.files[name]
		infos = append(infos, cache.FileInfo{Name: name, Size: int64(len(file.data)), ModTime: time.Unix(0, file.written)})
	}
	return infos, nil
}

func (f *FS) names() []string {
	names := make([]string, 0, len(f.files))
	for name := range f.files {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Names of the files, in order.
func (f *FS) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names()
}

// File returns a copy of the content of the file name.
func (f *FS) File(name string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[name]
	return bytes.Clone(file.data), ok
}

// SetFile writes data to the file name directly, without a temporary file, as a damaged disk
// or a crash in the middle of a write leaves it.
func (f *FS) SetFile(name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(name, bytes.Clone(data))
}

// Bytes of the files as they count toward the bound of a disk cache, by [cache.FileBytes], and
// the largest file's.
func (f *FS) Bytes() (total, largest int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, file := range f.files {
		b := cache.FileBytes(int64(len(file.data)))
		total += b
		largest = max(largest, b)
	}
	return total, largest
}
