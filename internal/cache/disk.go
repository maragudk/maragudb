package cache

import (
	"cmp"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// FS is the flat directory of files under a [Disk]. It must be safe for concurrent use.
type FS interface {
	// ReadFile returns the whole content of the file name.
	ReadFile(name string) ([]byte, error)
	// WriteTemp writes data to a new file whose name begins with [TempPrefix], and returns the
	// name.
	WriteTemp(data []byte) (string, error)
	// Rename the file from to to. A file named to is replaced atomically, so that a reader of to
	// sees either the old file or the new one.
	Rename(from, to string) error
	// Remove the file name. Removing a missing file returns an error that wraps
	// [fs.ErrNotExist].
	Remove(name string) error
	// List every file of the directory.
	List() ([]FileInfo, error)
}

// FileInfo of a file in an [FS].
type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// TempPrefix begins the name of every file that [FS.WriteTemp] writes.
const TempPrefix = "tmp-"

// FileBytes is what a file of size bytes counts for toward the bound of a [Disk]: its size
// rounded up to a whole block of 4 KiB, which is what most file systems allocate for it.
func FileBytes(size int64) int64 {
	return (size + 4095) / 4096 * 4096
}

// DiskOptions for [NewDisk].
type DiskOptions struct {
	// MaxBytes bounds the [FileBytes] of the files of the cache. It must be positive.
	MaxBytes int64
	// Mutations to break the cache with, for the tests that must detect them.
	Mutations DiskMutations
}

// DiskMutations are deliberate bugs in a [Disk].
type DiskMutations struct {
	// Unverified serves the bytes of a file after its header without checking the header, the
	// key, the length, or the checksum, so that a truncated, corrupt, or misplaced file is served.
	Unverified bool
	// ServeCutReads serves a file that is shorter than its header says as it is, without its
	// checksum, as if the end of the file were the end of the value. A read that an eviction
	// cuts short, on a file system that does not keep a removed file readable, leaves such a file.
	ServeCutReads bool
	// UncountedAtOpen leaves the files that are in the directory at open out of the cache, so
	// that they count toward no bound and are never evicted.
	UncountedAtOpen bool
}

// DiskStats of a [Disk].
type DiskStats struct {
	// Bytes are the [FileBytes] of the files the cache counts, and Entries the number of entries.
	Bytes   int64
	Entries int
	// Evictions counts the entries that were removed to stay within the bound, and Refused the
	// files that a read found missing, cut short, or damaged, which were misses.
	Evictions, Refused int64
}

// Disk is a cache of files in the flat directory of an [FS], one file per entry, named by a
// hash of its key. The files together take up at most a bound of bytes, counted by
// [FileBytes], and the least recently used entries are evicted to stay within it. A [Disk.Put]
// writes a temporary file and renames it into place, so at most one file beyond the bound
// exists, while a Put runs. Every file holds its key, its length, and a CRC-32C checksum, and a
// read of a file that is missing, of another length, or damaged is a miss, so the cache never
// serves bytes that were not put for the key, even when an entry vanishes in the middle of a
// read.
//
// At open, the files already in the directory are counted, in the order of their modification
// times, so the least recently written go first. Leftover temporary files are removed. Opening
// reads no file, and lists and stats every file, of which a cache with the same bound leaves at
// most one per 4 KiB of the bound.
type Disk struct {
	fs        FS
	maxBytes  int64
	mutations DiskMutations
	// putMu serializes the writes of [Disk.Put], so that only one file beyond the bound exists.
	putMu sync.Mutex

	mu    sync.Mutex
	bytes int64
	// order of the entries, most recently used first, and the entries by file name.
	order   *list.List
	entries map[string]*list.Element
	// stuck are files that eviction failed to remove, with their sizes. They count toward the
	// bound until a later eviction removes them.
	stuck              map[string]int64
	evictions, refused int64
}

type diskEntry struct {
	name  string
	bytes int64
}

// NewDisk cache on fsys.
func NewDisk(fsys FS, opts DiskOptions) (*Disk, error) {
	if opts.MaxBytes <= 0 {
		return nil, fmt.Errorf("cache: the bound of a disk cache must be positive, not %d", opts.MaxBytes)
	}
	files, err := fsys.List()
	if err != nil {
		return nil, fmt.Errorf("cache: listing the disk cache: %w", err)
	}
	d := &Disk{fs: fsys, maxBytes: opts.MaxBytes, mutations: opts.Mutations, order: list.New(), entries: map[string]*list.Element{}, stuck: map[string]int64{}}
	var kept []FileInfo
	for _, f := range files {
		switch {
		case strings.HasPrefix(f.Name, TempPrefix):
			// The write of a process that stopped before its rename.
			_ = fsys.Remove(f.Name)
		case isEntryName(f.Name) && !d.mutations.UncountedAtOpen:
			kept = append(kept, f)
		}
	}
	slices.SortFunc(kept, func(a, b FileInfo) int {
		return cmp.Or(a.ModTime.Compare(b.ModTime), strings.Compare(a.Name, b.Name))
	})
	for _, f := range kept {
		e := &diskEntry{name: f.Name, bytes: FileBytes(f.Size)}
		d.entries[f.Name] = d.order.PushFront(e)
		d.bytes += e.bytes
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evict()
	return d, nil
}

// isEntryName reports whether name is that of an entry: a SHA-256 in lowercase hex.
func isEntryName(name string) bool {
	if len(name) != 2*sha256.Size {
		return false
	}
	for _, c := range []byte(name) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func entryName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// The file of an entry is a header of four little-endian uint32s, the magic number, a CRC-32C of
// everything after it, the length of the key, and the length of the value, then the key, and
// then the value.
const (
	diskMagic  = 0x3163646d // "mdc1"
	diskHeader = 16
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func encodeEntry(key string, value []byte) []byte {
	b := make([]byte, diskHeader+len(key)+len(value))
	binary.LittleEndian.PutUint32(b[0:], diskMagic)
	binary.LittleEndian.PutUint32(b[8:], uint32(len(key)))
	binary.LittleEndian.PutUint32(b[12:], uint32(len(value)))
	copy(b[diskHeader:], key)
	copy(b[diskHeader+len(key):], value)
	binary.LittleEndian.PutUint32(b[4:], crc32.Checksum(b[8:], castagnoli))
	return b
}

// decodeEntry returns the value in the file b of key, and false when b is not a whole and
// undamaged file of key.
func (d *Disk) decodeEntry(key string, b []byte) ([]byte, bool) {
	if len(b) < diskHeader {
		return nil, false
	}
	keyLen := int64(binary.LittleEndian.Uint32(b[8:]))
	valueLen := int64(binary.LittleEndian.Uint32(b[12:]))
	start := diskHeader + keyLen
	if d.mutations.Unverified && start <= int64(len(b)) {
		return b[start:], true
	}
	if binary.LittleEndian.Uint32(b[0:]) != diskMagic {
		return nil, false
	}
	if int64(len(b)) != start+valueLen {
		if d.mutations.ServeCutReads && start <= int64(len(b)) && int64(len(b)) < start+valueLen {
			return b[start:], true
		}
		return nil, false
	}
	if crc32.Checksum(b[8:], castagnoli) != binary.LittleEndian.Uint32(b[4:]) || string(b[diskHeader:start]) != key {
		return nil, false
	}
	return b[start:], true
}

// Get the value of key. A file that is missing, cut short, or damaged is a miss, and leaves the
// cache.
func (d *Disk) Get(key string) ([]byte, bool) {
	name := entryName(key)
	d.mu.Lock()
	e, ok := d.entries[name]
	if ok {
		d.order.MoveToFront(e)
	}
	d.mu.Unlock()
	if !ok {
		return nil, false
	}
	// The read runs without the lock, so an eviction or a Put of the same key can remove or
	// replace the file meanwhile. A read of a replaced file gets the old file or the new one, and
	// the check below refuses one that the removal cut short.
	b, err := d.fs.ReadFile(name)
	if err == nil {
		if v, ok := d.decodeEntry(key, b); ok {
			return v, true
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refused++
	// Only the entry that was read leaves: a Put may have replaced it meanwhile with a new file,
	// which must stay counted.
	if d.entries[name] == e {
		d.drop(name)
	}
	return nil, false
}

// Put the value of key. When its file alone would exceed the bound, or the write fails, the
// key leaves the cache instead, so that a Get never returns a value that an older Put put.
func (d *Disk) Put(key string, value []byte) {
	name := entryName(key)
	d.putMu.Lock()
	defer d.putMu.Unlock()
	tmp, size, ok := d.writeTemp(key, value)
	d.mu.Lock()
	defer d.mu.Unlock()
	if ok && d.fs.Rename(tmp, name) != nil {
		_ = d.fs.Remove(tmp)
		ok = false
	}
	if !ok {
		d.drop(name)
		return
	}
	// The rename replaced the old file of the key, whether it was an entry or stuck.
	if old, ok := d.entries[name]; ok {
		d.order.Remove(old)
		d.bytes -= old.Value.(*diskEntry).bytes
	}
	if bytes, ok := d.stuck[name]; ok {
		delete(d.stuck, name)
		d.bytes -= bytes
	}
	d.entries[name] = d.order.PushFront(&diskEntry{name: name, bytes: size})
	d.bytes += size
	victims := d.evictEntries()
	d.mu.Unlock()
	// The files of the evicted entries are removed without the lock, so that reads need not wait
	// for the removals. Only a Put could write one of these names again, and the Puts take turns.
	var stuck []*diskEntry
	for _, e := range victims {
		if err := d.fs.Remove(e.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			stuck = append(stuck, e)
		}
	}
	d.mu.Lock()
	for _, e := range stuck {
		d.stuck[e.name] = e.bytes
		d.bytes += e.bytes
	}
}

// writeTemp the file of an entry, unless it alone would exceed the bound, and return its name and
// the bytes it counts for.
func (d *Disk) writeTemp(key string, value []byte) (string, int64, bool) {
	if int64(len(key)) > math.MaxUint32 || int64(len(value)) > math.MaxUint32 {
		return "", 0, false
	}
	data := encodeEntry(key, value)
	size := FileBytes(int64(len(data)))
	if size > d.maxBytes {
		return "", 0, false
	}
	tmp, err := d.fs.WriteTemp(data)
	return tmp, size, err == nil
}

// drop the entry of the file name, if any, and remove its file. d.mu must be held.
func (d *Disk) drop(name string) {
	e, ok := d.entries[name]
	if !ok {
		return
	}
	d.order.Remove(e)
	delete(d.entries, name)
	d.bytes -= e.Value.(*diskEntry).bytes
	d.remove(name, e.Value.(*diskEntry).bytes)
}

// evict the least recently used entries until the cache is within its bound, and remove their
// files. d.mu must be held.
func (d *Disk) evict() {
	for _, e := range d.evictEntries() {
		d.remove(e.name, e.bytes)
	}
}

// evictEntries takes the least recently used entries out of the cache until it is within its
// bound, and returns them, so that the caller removes their files. Stuck files are removed first,
// and count again when they still fail. d.mu must be held.
func (d *Disk) evictEntries() []*diskEntry {
	for _, name := range slices.Sorted(maps.Keys(d.stuck)) {
		if d.bytes <= d.maxBytes {
			return nil
		}
		bytes := d.stuck[name]
		delete(d.stuck, name)
		d.bytes -= bytes
		d.remove(name, bytes)
	}
	var victims []*diskEntry
	for d.bytes > d.maxBytes && d.order.Len() > 0 {
		oldest := d.order.Back()
		e := oldest.Value.(*diskEntry)
		d.order.Remove(oldest)
		delete(d.entries, e.name)
		d.bytes -= e.bytes
		d.evictions++
		victims = append(victims, e)
	}
	return victims
}

// remove the file of an entry that left the cache, and keep counting its bytes as stuck when the
// file stays. d.mu must be held.
func (d *Disk) remove(name string, bytes int64) {
	if err := d.fs.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.stuck[name] = bytes
		d.bytes += bytes
	}
}

// Stats of the cache.
func (d *Disk) Stats() DiskStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return DiskStats{Bytes: d.bytes, Entries: d.order.Len(), Evictions: d.evictions, Refused: d.refused}
}
