package cache_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/fsfake"
	"maragu.dev/maragudb/internal/verify"
)

func TestDisk(t *testing.T) {
	t.Run("returns what was put and survives a new instance on the same directory", func(t *testing.T) {
		dir := t.TempDir()
		d, closeDisk, err := cache.OpenDir(dir, 1<<20)
		is.NotError(t, err)
		d.Put("a/b", []byte("1"))
		v, ok := d.Get("a/b")
		is.True(t, ok)
		is.Equal(t, "1", string(v))
		_, ok = d.Get("missing")
		is.True(t, !ok)
		closeDisk()

		d2, closeDisk2, err := cache.OpenDir(dir, 1<<20)
		is.NotError(t, err)
		defer closeDisk2()
		is.True(t, d2 != d, "the last close released the directory")
		v, ok = d2.Get("a/b")
		is.True(t, ok)
		is.Equal(t, "1", string(v))
	})

	t.Run("evicts the least recently used entries when over the bound", func(t *testing.T) {
		fsys := &fsfake.FS{}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 3 * 4096})
		is.NotError(t, err)
		d.Put("a", []byte("1"))
		d.Put("b", []byte("2"))
		d.Put("c", []byte("3"))
		_, ok := d.Get("a")
		is.True(t, ok)
		d.Put("d", []byte("4"))
		_, ok = d.Get("b")
		is.True(t, !ok, "b was least recently used")
		for _, key := range []string{"a", "c", "d"} {
			_, ok := d.Get(key)
			is.True(t, ok, key)
		}
		is.Equal(t, 3, len(fsys.Names()), "the file of b is gone")
		is.Equal(t, cache.DiskStats{Bytes: 3 * 4096, Entries: 3, Evictions: 1}, d.Stats())
	})

	t.Run("does not keep an entry whose file alone exceeds the bound, and drops the older value of its key", func(t *testing.T) {
		fsys := &fsfake.FS{}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 4096})
		is.NotError(t, err)
		d.Put("a", []byte("1"))
		d.Put("a", make([]byte, 4096))
		_, ok := d.Get("a")
		is.True(t, !ok)
		is.Equal(t, 0, len(fsys.Names()))
		is.Equal(t, int64(0), d.Stats().Bytes)
	})

	t.Run("replaces the value of a key and counts only the new file", func(t *testing.T) {
		fsys := &fsfake.FS{}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 4 * 4096})
		is.NotError(t, err)
		d.Put("m", make([]byte, 100))
		d.Put("m", make([]byte, 5000))
		v, ok := d.Get("m")
		is.True(t, ok)
		is.Equal(t, 5000, len(v))
		total, _ := fsys.Bytes()
		is.Equal(t, int64(2*4096), total)
		is.Equal(t, total, d.Stats().Bytes)
	})

	t.Run("counts the files of an earlier process at open, oldest written first, removes its temporary files, and keeps other files", func(t *testing.T) {
		verify.Covers(t, "a disk cache counts the files that are in its directory at open, evicts the least recently written of them first, removes leftover temporary files, and evicts down to a lowered bound at open", "INV-DISK-CACHE-BOUND")
		fsys := &fsfake.FS{}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 4 * 4096})
		is.NotError(t, err)
		for _, key := range []string{"a", "b", "c"} {
			d.Put(key, []byte(key))
		}
		_, ok := d.Get("a")
		is.True(t, ok)
		fsys.SetFile(cache.TempPrefix+"crashed", make([]byte, 9000))
		fsys.SetFile("lock", nil)

		d2, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 4 * 4096})
		is.NotError(t, err)
		is.Equal(t, cache.DiskStats{Bytes: 3 * 4096, Entries: 3}, d2.Stats())
		is.True(t, !strings.Contains(strings.Join(fsys.Names(), " "), cache.TempPrefix), "the temporary file is gone")
		is.True(t, strings.Contains(strings.Join(fsys.Names(), " "), "lock"), "a file that is not an entry stays")
		d2.Put("d", []byte("d"))
		d2.Put("e", []byte("e"))
		_, ok = d2.Get("a")
		is.True(t, !ok, "a was written first, and reads before the restart are not remembered")
		_, ok = d2.Get("b")
		is.True(t, ok)

		d3, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 2 * 4096})
		is.NotError(t, err)
		is.Equal(t, int64(2*4096), d3.Stats().Bytes, "a lowered bound evicts at open")
		total, _ := fsys.Bytes()
		is.Equal(t, int64(2*4096), total)
		_, ok = d3.Get("e")
		is.True(t, ok, "the newest files stay")
	})

	t.Run("keeps counting a file whose removal failed until a later eviction removes it", func(t *testing.T) {
		var failing bool
		fsys := &fsfake.FS{RemoveErr: func(string) error {
			if failing {
				return errors.New("oh no")
			}
			return nil
		}}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 2 * 4096})
		is.NotError(t, err)
		d.Put("a", []byte("a"))
		d.Put("b", []byte("b"))
		failing = true
		d.Put("c", []byte("c"))
		total, _ := fsys.Bytes()
		is.Equal(t, int64(3*4096), total, "a stays")
		is.True(t, d.Stats().Bytes >= total, "and is counted")
		failing = false
		d.Put("d", []byte("d"))
		total, _ = fsys.Bytes()
		is.Equal(t, int64(2*4096), total)
		is.Equal(t, total, d.Stats().Bytes)
	})

	t.Run("never serves bytes that the latest Put of a key did not put, never holds more than the bound, and counts every file, through damage, restarts, and evictions and replacements in the middle of reads", func(t *testing.T) {
		verify.Covers(t, "random operations, damaged and cut files, restarts with leftover temporary files, and evictions and replacements in the middle of reads never make a disk cache serve other bytes than the latest Put of the key, and its files never exceed the bound and are all counted", "INV-DISK-CACHE-BOUND", "INV-CACHE-LOSS")
		hegel.Test(t, func(ht *hegel.T) {
			draw := func(lo, hi int) int { return hegel.Draw(ht, hegel.Integers(lo, hi)) }
			if err := runDiskOps(draw, cache.DiskMutations{}); err != nil {
				ht.Fatalf("%v", err)
			}
		}, hegel.WithTestCases(300))
	})

	t.Run("with the mutant that serves damaged files, the random operations serve bytes that were not put", func(t *testing.T) {
		verify.Covers(t, "with the mutant, the random operations on a disk cache find a Get that serves a truncated, corrupt, or misplaced file", "MUT-DISK-CACHE-UNVERIFIED")
		t.Log(findDiskMutant(t, cache.DiskMutations{Unverified: true}))
	})

	t.Run("with the mutant that serves cut reads, the random operations serve bytes that were not put", func(t *testing.T) {
		verify.Covers(t, "with the mutant, the random operations on a disk cache find a Get that serves a read that an eviction or a replacement cut short", "MUT-DISK-CACHE-SERVE-CUT-READ")
		t.Log(findDiskMutant(t, cache.DiskMutations{ServeCutReads: true}))
	})

	t.Run("with the mutant that does not count the files of an earlier process, the random operations find files that are not counted", func(t *testing.T) {
		verify.Covers(t, "with the mutant, the random operations on a disk cache find files that it does not count after a restart", "MUT-DISK-CACHE-UNCOUNTED")
		t.Log(findDiskMutant(t, cache.DiskMutations{UncountedAtOpen: true}))
	})

	t.Run("serves a read that an eviction overlaps whole or not at all", func(t *testing.T) {
		verify.Covers(t, "an eviction of an entry in the middle of its read gives the whole value on a file system that keeps removed files readable, and a miss on one that cuts the read short, never the bytes it cut", "INV-CACHE-LOSS")
		for _, cut := range []bool{false, true} {
			fsys := &fsfake.FS{CutReads: cut}
			d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 2 * 4096})
			is.NotError(t, err)
			want := bytes.Repeat([]byte("a"), 3000)
			d.Put("a", want)
			fsys.OnRead = func(string) {
				fsys.OnRead = nil
				d.Put("b", []byte("b"))
				d.Put("c", []byte("c"))
			}
			v, ok := d.Get("a")
			is.True(t, fsys.OnRead == nil, "the eviction ran in the middle of the read")
			is.Equal(t, !cut, ok)
			if ok {
				is.True(t, bytes.Equal(want, v))
			}
			_, ok = d.Get("a")
			is.True(t, !ok, "the evicted entry is gone")
			total, _ := fsys.Bytes()
			is.Equal(t, total, d.Stats().Bytes)
		}
	})

	t.Run("keeps counting a newer file of a key when a read of the older one fails in the middle", func(t *testing.T) {
		verify.Covers(t, "a read that fails because its entry was evicted and put again in the middle of it leaves the new entry in the cache and counted", "INV-DISK-CACHE-BOUND")
		fsys := &fsfake.FS{CutReads: true}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 4 * 4096})
		is.NotError(t, err)
		d.Put("a", []byte("old value"))
		fsys.OnRead = func(string) {
			fsys.OnRead = nil
			d.Put("a", []byte("new value"))
		}
		_, ok := d.Get("a")
		is.True(t, !ok, "the replacement cut the read short")
		v, ok := d.Get("a")
		is.True(t, ok)
		is.Equal(t, "new value", string(v))
		total, _ := fsys.Bytes()
		is.Equal(t, int64(4096), total)
		is.Equal(t, total, d.Stats().Bytes)
	})

	t.Run("refuses every damaged file and removes it", func(t *testing.T) {
		verify.Covers(t, "a disk cache file that is empty, truncated at any length, corrupt in any byte, of another key, or of the format without a header is a miss, and leaves the cache", "INV-CACHE-LOSS")
		fsys := &fsfake.FS{}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 1 << 20})
		is.NotError(t, err)
		d.Put("other", []byte("another value"))
		otherFile, _ := fsys.File(fsys.Names()[0])
		value := []byte("the value of the key")
		d.Put("key", value)
		var name string
		for _, n := range fsys.Names() {
			if b, _ := fsys.File(n); !bytes.Equal(b, otherFile) {
				name = n
			}
		}
		good, _ := fsys.File(name)
		damaged := [][]byte{nil, otherFile, value}
		for i := range good {
			damaged = append(damaged, good[:i])
			for bit := range 8 {
				b := bytes.Clone(good)
				b[i] ^= 1 << bit
				damaged = append(damaged, b)
			}
		}
		damaged = append(damaged, append(bytes.Clone(good), 'x'))
		for i, b := range damaged {
			// The damage happens behind the cache's back, to an entry that it counts.
			d.Put("key", value)
			fsys.SetFile(name, b)
			_, ok := d.Get("key")
			is.True(t, !ok, fmt.Sprint("damage ", i))
			_, ok = fsys.File(name)
			is.True(t, !ok, "the damaged file is removed")
			total, _ := fsys.Bytes()
			is.Equal(t, total, d.Stats().Bytes)
		}
		fsys.SetFile(name, good)
		d2, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: 1 << 20})
		is.NotError(t, err)
		v, ok := d2.Get("key")
		is.True(t, ok, "the undamaged file is a hit")
		is.True(t, bytes.Equal(value, v))
	})

	t.Run("keeps its files within the bound and recently used entries cached, through a realistic workload with restarts", func(t *testing.T) {
		verify.Covers(t, "a read-through workload of segment blocks with a hot set, commits that rewrite the manifest, merges that make new segments, and restarts fills a disk cache to many times its bound, and after every operation its files are within the bound and all counted, every entry used within the last bound's worth of entries is a hit, and most hot reads hit", "INV-DISK-CACHE-BOUND")
		const bound = 1 << 20
		fsys := &fsfake.FS{}
		open := func() *cache.Disk {
			d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: bound})
			is.NotError(t, err)
			return d
		}
		d := open()
		var peak int64
		res := runWorkload(t, 20_000, bound, func() *cache.Disk { return d }, func() { d = open() }, func() {
			total, _ := fsys.Bytes()
			peak = max(peak, total)
			if total > bound {
				t.Fatalf("the files take %d bytes, more than the bound %d", total, bound)
			}
			if got := d.Stats().Bytes; got != total {
				t.Fatalf("the cache counts %d bytes, and its files take %d", got, total)
			}
		})
		t.Logf("%+v, at most %d bytes in files", res, peak)
		is.True(t, res.written > 20*bound, "the workload writes many times the bound")
		is.True(t, res.leafHits*10 >= res.leafReads*6, fmt.Sprintf("hot reads hit %d of %d times", res.leafHits, res.leafReads))
	})

	t.Run("keeps its files within the bound in a directory of the operating system", func(t *testing.T) {
		verify.Covers(t, "the realistic workload on a disk cache directory of the operating system, with restarts that close and open it again, keeps its files within the bound after every operation", "INV-DISK-CACHE-BOUND")
		const bound = 256 << 10
		dir := t.TempDir()
		open := func() (*cache.Disk, func()) {
			d, closeDisk, err := cache.OpenDir(dir, bound)
			is.NotError(t, err)
			return d, closeDisk
		}
		d, closeDisk := open()
		defer func() { closeDisk() }()
		res := runWorkload(t, 3_000, bound, func() *cache.Disk { return d }, func() {
			closeDisk()
			d, closeDisk = open()
		}, func() {
			total, _ := dirBytes(t, dir)
			if total > bound {
				t.Fatalf("the files take %d bytes, more than the bound %d", total, bound)
			}
		})
		is.True(t, res.written > 5*bound, "the workload writes many times the bound")
	})

	t.Run("keeps its files within the bound by one entry under concurrent reads and writes", func(t *testing.T) {
		verify.Covers(t, "concurrent read-through workloads on one disk cache never make its files exceed the bound by more than one entry, checked after every operation", "INV-DISK-CACHE-BOUND")
		const bound = 256 << 10
		fsys := &fsfake.FS{CutReads: true}
		d, err := cache.NewDisk(fsys, cache.DiskOptions{MaxBytes: bound})
		is.NotError(t, err)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				runConcurrentWorkload(t, 1_000, bound, d, func() {
					total, largest := fsys.Bytes()
					if total > bound+largest {
						t.Errorf("the files take %d bytes, more than the bound %d and an entry of %d", total, bound, largest)
					}
				})
			})
		}
		wg.Wait()
		total, _ := fsys.Bytes()
		is.Equal(t, total, d.Stats().Bytes)
	})
}

func TestOpenDir(t *testing.T) {
	t.Run("refuses a directory that another process has open until that process ends, also by a crash, and shares one within a process", func(t *testing.T) {
		verify.Covers(t, "a second process that opens a disk cache directory fails with ErrInUse while the first has it open, and opens it once the first closed it or crashed; within one process, the opens of a directory share one cache", "INV-DISK-CACHE-BOUND")
		dir := t.TempDir()
		helper := startHelper(t, dir)
		is.Equal(t, "open", helper.line)
		_, _, err := cache.OpenDir(dir, 1<<20)
		is.Error(t, cache.ErrInUse, err)
		is.True(t, strings.Contains(err.Error(), "DiskCacheDir"), err.Error())
		helper.crash()

		d, closeDisk, err := cache.OpenDir(dir, 1<<20)
		is.NotError(t, err)
		d2, closeDisk2, err := cache.OpenDir(filepath.Join(dir, "."), 1<<20)
		is.NotError(t, err)
		is.True(t, d == d2, "one cache per directory in a process")
		is.Equal(t, "in use", startHelper(t, dir).line)
		closeDisk()
		closeDisk()
		is.Equal(t, "in use", startHelper(t, dir).line, "a second close of one open does not release another")
		closeDisk2()
		is.Equal(t, "open", startHelper(t, dir).line)
	})

	t.Run("rejects a bound that is not positive", func(t *testing.T) {
		_, _, err := cache.OpenDir(t.TempDir(), 0)
		is.True(t, err != nil)
	})
}

// findDiskMutant runs sequences of random operations with the mutations until one fails, and
// returns the failure.
func findDiskMutant(t *testing.T, mutations cache.DiskMutations) error {
	t.Helper()
	for seed := uint64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0))
		if err := runDiskOps(func(lo, hi int) int { return lo + rng.IntN(hi-lo+1) }, mutations); err != nil {
			return err
		}
	}
	t.Fatal("no sequence of operations found the mutant")
	return nil
}

// helperDirEnv names the directory that [TestHelperProcess] opens.
const helperDirEnv = "MARAGUDB_CACHE_HELPER_DIR"

// TestHelperProcess opens the disk cache directory of [helperDirEnv] in a process of its own,
// prints whether it could, and keeps it open until its standard input closes.
func TestHelperProcess(t *testing.T) {
	dir := os.Getenv(helperDirEnv)
	if dir == "" {
		t.Skip("only runs as a helper process")
	}
	_, closeDisk, err := cache.OpenDir(dir, 1<<20)
	switch {
	case errors.Is(err, cache.ErrInUse):
		fmt.Println("in use")
		return
	case err != nil:
		fmt.Println(err)
		return
	}
	defer closeDisk()
	fmt.Println("open")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

type helperProcess struct {
	line  string
	crash func()
}

// startHelper process on dir and read its first line.
func startHelper(t *testing.T, dir string) helperProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "MARAGUDB_VERIFY_DIR=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, helperDirEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	is.NotError(t, err)
	stdout, err := cmd.StdoutPipe()
	is.NotError(t, err)
	is.NotError(t, cmd.Start())
	var once sync.Once
	crash := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = stdin.Close()
		})
	}
	t.Cleanup(crash)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	is.NotError(t, err)
	line = strings.TrimSpace(line)
	if line != "open" {
		crash()
	}
	return helperProcess{line: line, crash: crash}
}

// dirBytes of the files in dir as they count toward the bound of a disk cache, and the largest
// file's.
func dirBytes(t *testing.T, dir string) (total, largest int64) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	is.NotError(t, err)
	for _, e := range entries {
		info, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		is.NotError(t, err)
		b := cache.FileBytes(info.Size())
		total += b
		largest = max(largest, b)
	}
	return total, largest
}

// diskValue of a size for a key and a version, so that a value tells which Put put it.
func diskValue(key, version, size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(key*31 + version*7 + i)
	}
	return b
}

// runDiskOps runs a sequence of operations on a disk cache over a fake file system, with the
// choices that draw makes, and returns the first violation: a Get that returns other bytes than
// the latest Put of the key, files that take more bytes than the cache counts, or a count over
// the bound.
func runDiskOps(draw func(lo, hi int) int, mutations cache.DiskMutations) error {
	bound := int64(draw(1, 6)) * 4096
	fsys := &fsfake.FS{CutReads: draw(0, 1) == 1}
	opts := cache.DiskOptions{MaxBytes: bound, Mutations: mutations}
	d, err := cache.NewDisk(fsys, opts)
	if err != nil {
		return err
	}
	latest := map[int][]byte{}
	version := 0
	key := func(k int) string { return fmt.Sprint("key-", k) }
	put := func(k, size int) {
		version++
		latest[k] = diskValue(k, version, size)
		d.Put(key(k), latest[k])
	}
	get := func(k int, older []byte) error {
		v, ok := d.Get(key(k))
		if !ok || bytes.Equal(v, latest[k]) || (older != nil && bytes.Equal(v, older)) {
			return nil
		}
		return fmt.Errorf("a Get of %s returned %d bytes that are not the value of its latest Put", key(k), len(v))
	}
	for range draw(1, 60) {
		k := draw(0, 4)
		switch draw(0, 7) {
		case 0, 1:
			put(k, draw(0, 9000))
		case 2, 3:
			if err := get(k, nil); err != nil {
				return err
			}
		case 4:
			// Damage a file behind the cache's back, without making it larger.
			names := fsys.Names()
			if len(names) == 0 {
				continue
			}
			name := names[draw(0, len(names)-1)]
			b, _ := fsys.File(name)
			switch draw(0, 3) {
			case 0:
				fsys.SetFile(name, b[:draw(0, max(0, len(b)-1))])
			case 1:
				if len(b) > 0 {
					b[draw(0, len(b)-1)] ^= byte(1 << draw(0, 7))
					fsys.SetFile(name, b)
				}
			case 2:
				other, _ := fsys.File(names[draw(0, len(names)-1)])
				if cache.FileBytes(int64(len(other))) <= cache.FileBytes(int64(len(b))) {
					fsys.SetFile(name, other)
				}
			case 3:
				_ = fsys.Remove(name)
			}
		case 5:
			// A crash, which may leave the temporary file of a write, and a restart.
			if draw(0, 1) == 1 {
				fsys.SetFile(cache.TempPrefix+"crashed", diskValue(k, 0, draw(0, 5000)))
			}
			if d, err = cache.NewDisk(fsys, opts); err != nil {
				return err
			}
		case 6:
			// Entries put in the middle of a read evict the one that is read.
			fsys.OnRead = func(string) {
				fsys.OnRead = nil
				for j := range 3 {
					put(10+j, draw(3000, 9000))
				}
			}
			err := get(k, nil)
			fsys.OnRead = nil
			if err != nil {
				return err
			}
		case 7:
			// A Put of the same key in the middle of a read replaces the file that is read.
			older := latest[k]
			size := draw(0, 9000)
			fsys.OnRead = func(string) {
				fsys.OnRead = nil
				put(k, size)
			}
			err := get(k, older)
			fsys.OnRead = nil
			if err != nil {
				return err
			}
		}
		total, largest := fsys.Bytes()
		counted := d.Stats().Bytes
		switch {
		case total > bound+largest:
			return fmt.Errorf("the files take %d bytes, more than the bound %d and an entry of %d", total, bound, largest)
		case total > counted:
			return fmt.Errorf("the files take %d bytes, and the cache counts %d", total, counted)
		case counted > bound:
			return fmt.Errorf("the cache counts %d bytes, more than the bound %d", counted, bound)
		}
	}
	return nil
}

type workloadResult struct {
	written, leafReads, leafHits int64
}

// runWorkload reads through the disk cache that disk returns as the engine reads a database: a
// read gets the root block of every live segment and a leaf block of the oldest, the largest,
// with a skew towards a hot set. A commit adds a small segment and puts its root block and the
// manifest, which is one key that every commit rewrites, and merges replace the small segments
// with one, and now and then the whole database with a new largest segment. A hit must be the
// value that was put, and an entry that was used within the last bound's worth of the largest
// entries must be a hit, since the cache evicts the least recently used. Every restartEvery
// operations, restart opens the cache again, when set. check runs after every operation.
func runWorkload(t *testing.T, ops int, bound int64, disk func() *cache.Disk, restart, check func()) workloadResult {
	return runWorkloadIn(t, ops, bound, disk, restart, check, false)
}

// runConcurrentWorkload is [runWorkload] in one of several goroutines on one cache, which
// evict each other's entries, so that an entry that this one used recently may miss, and which
// reports failures without stopping the test.
func runConcurrentWorkload(t *testing.T, ops int, bound int64, d *cache.Disk, check func()) {
	runWorkloadIn(t, ops, bound, func() *cache.Disk { return d }, nil, check, true)
}

func runWorkloadIn(t *testing.T, ops int, bound int64, disk func() *cache.Disk, restart, check func(), concurrent bool) workloadResult {
	t.Helper()
	fatalf := t.Fatalf
	if concurrent {
		fatalf = t.Errorf
	}
	const restartEvery = 2_500
	rng := rand.New(rand.NewPCG(uint64(ops), uint64(bound)))
	type segment struct {
		id, root int
		leaves   []int
	}
	nextID := 0
	newSegment := func(leaves, minSize, maxSize int) segment {
		nextID++
		s := segment{id: nextID, root: 300 + rng.IntN(1500)}
		for range leaves {
			s.leaves = append(s.leaves, minSize+rng.IntN(maxSize-minSize+1))
		}
		return s
	}
	const leaves = 400
	base := newSegment(leaves, 4096, 16384)
	var small []segment
	zipf := rand.NewZipf(rng, 1.1, 1, leaves-1)
	value := func(key string, size int) []byte {
		h := fnv.New64a()
		_, _ = h.Write([]byte(key))
		return diskValue(int(h.Sum64()%1000), 0, size)
	}
	// A key used within the last `recent` distinct keys must be a hit.
	largestFile := cache.FileBytes(16 + 64 + 16384)
	recent := int(bound / largestFile)
	var order []string
	used := func(key string) bool {
		for i, k := range order {
			if k == key {
				order = append(order[:i], order[i+1:]...)
				order = append([]string{key}, order...)
				return true
			}
		}
		order = append([]string{key}, order...)
		if len(order) > recent {
			order = order[:recent]
		}
		return false
	}
	var res workloadResult
	put := func(key string, v []byte) {
		disk().Put(key, v)
		res.written += int64(len(v))
		used(key)
		check()
	}
	read := func(key string, size int) bool {
		v := value(key, size)
		wasRecent := used(key)
		got, ok := disk().Get(key)
		switch {
		case ok && !bytes.Equal(got, v):
			fatalf("a hit of %s returned other bytes than were put", key)
		case !ok && wasRecent && !concurrent:
			fatalf("%s missed, but was used within the last %d entries", key, recent)
		case !ok:
			disk().Put(key, v)
			res.written += int64(len(v))
		}
		check()
		return ok
	}
	blockKey := func(s segment, i int) string { return fmt.Sprintf("db/seg/%d@%d", s.id, i) }
	commits := 0
	for op := range ops {
		if restart != nil && op > 0 && op%restartEvery == 0 {
			restart()
			order = nil
			check()
		}
		if op%10 != 9 {
			read(blockKey(base, -1), base.root)
			for _, s := range small {
				read(blockKey(s, -1), s.root)
			}
			leaf := int(zipf.Uint64())
			res.leafReads++
			if read(blockKey(base, leaf), base.leaves[leaf]) {
				res.leafHits++
			}
			continue
		}
		commits++
		s := newSegment(0, 0, 0)
		put(blockKey(s, -1), value(blockKey(s, -1), s.root))
		small = append(small, s)
		put("db/manifest", value(fmt.Sprint("db/manifest", commits), 500+100*len(small)))
		switch {
		case commits%400 == 0:
			// A merge of the whole database makes every block of the largest segment new.
			base = newSegment(leaves, 4096, 16384)
			small = nil
		case len(small) == 4:
			small = []segment{newSegment(0, 0, 0)}
		}
	}
	return res
}

// FuzzDisk reads a file of arbitrary bytes as the entry of a key, which never panics, and is a
// hit only when the file is exactly what a Put of the key and the value it returns writes.
func FuzzDisk(f *testing.F) {
	var files fsfake.FS
	d, err := cache.NewDisk(&files, cache.DiskOptions{MaxBytes: 1 << 20})
	if err != nil {
		f.Fatal(err)
	}
	d.Put("db/seg/a@0+10", []byte("0123456789"))
	for _, name := range files.Names() {
		b, _ := files.File(name)
		f.Add("db/seg/a@0+10", b)
		f.Add("db/seg/b@0+10", b)
		f.Add("db/seg/a@0+10", b[:len(b)-1])
	}
	f.Add("k", []byte{})
	f.Fuzz(func(t *testing.T, key string, file []byte) {
		var files fsfake.FS
		d, err := cache.NewDisk(&files, cache.DiskOptions{MaxBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		d.Put(key, nil)
		names := files.Names()
		if len(names) != 1 {
			t.Fatalf("a Put left %d files", len(names))
		}
		files.SetFile(names[0], file)
		v, ok := d.Get(key)
		if !ok {
			return
		}
		var again fsfake.FS
		d2, err := cache.NewDisk(&again, cache.DiskOptions{MaxBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		d2.Put(key, v)
		want, _ := again.File(names[0])
		if !bytes.Equal(file, want) {
			t.Fatalf("a hit of %q returned %q from a file that a Put of it does not write", key, v)
		}
	})
}
