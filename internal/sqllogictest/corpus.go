package sqllogictest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// The corpus is a pinned commit of a GitHub mirror of SQLite's sqllogictest repository, which
// keeps its COPYRIGHT.md.
const (
	mirror     = "https://github.com/gregrahn/sqllogictest"
	commit     = "c93ac416f05635ff5cc490009add936d9753d01d"
	archiveURL = "https://codeload.github.com/gregrahn/sqllogictest/tar.gz/" + commit
)

// manifest has the SHA-256 of COPYRIGHT.md and of every test file of the commit, in the format
// of sha256sum, with the test files named relative to the test directory.
//
//go:embed testdata/manifest.txt
var manifest []byte

// license is the manifest name of the corpus's license file.
const license = "COPYRIGHT.md"

// corpus is the downloaded and verified corpus in a directory.
type corpus struct {
	dir string
	// sums maps every file of the manifest to its SHA-256.
	sums map[string]string
	// files are the test files, sorted.
	files []string
}

// parseManifest into a map from name to SHA-256 and the sorted test files.
func parseManifest(b []byte) (map[string]string, []string, error) {
	sums := map[string]string{}
	var files []string
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		sum, name, ok := strings.Cut(scanner.Text(), "  ")
		if !ok || len(sum) != 64 || name == "" || !fs.ValidPath(name) {
			return nil, nil, fmt.Errorf("sqllogictest: bad manifest line %q", scanner.Text())
		}
		sums[name] = sum
		if name != license {
			files = append(files, name)
		}
	}
	if _, ok := sums[license]; !ok {
		return nil, nil, errors.New("sqllogictest: the manifest has no " + license)
	}
	sort.Strings(files)
	return sums, files, scanner.Err()
}

// cacheDir is MARAGUDB_SQLLOGICTEST_DIR when set, and otherwise a directory for the commit in
// the user cache directory.
func cacheDir() (string, error) {
	if dir := os.Getenv("MARAGUDB_SQLLOGICTEST_DIR"); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("sqllogictest: no user cache directory, set MARAGUDB_SQLLOGICTEST_DIR: %w", err)
	}
	return filepath.Join(base, "maragudb", "sqllogictest", commit), nil
}

// markerName is the file that marks a complete cache directory. It holds the SHA-256 of the
// manifest that the files were checked against.
const markerName = ".complete"

// loadCorpus from the cache, after downloading it there when it is not, or not complete. It
// fails, with a message that says what to do, when the corpus is neither cached nor
// downloadable.
func loadCorpus(ctx context.Context) (*corpus, error) {
	sums, files, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}
	dir, err := cacheDir()
	if err != nil {
		return nil, err
	}
	c := &corpus{dir: dir, sums: sums, files: files}
	want := manifestSum()
	if b, err := os.ReadFile(filepath.Join(dir, markerName)); err == nil && strings.TrimSpace(string(b)) == want {
		return c, nil
	}
	if err := download(ctx, dir, sums, want); err != nil {
		return nil, fmt.Errorf("sqllogictest: the corpus is not in the cache at %s, and downloading it failed: %w\n"+
			"The corpus is %s at commit %s. Connect to the network once to cache it, or set MARAGUDB_SQLLOGICTEST_DIR to a directory with the corpus", dir, err, mirror, commit)
	}
	return c, nil
}

func manifestSum() string {
	sum := sha256.Sum256(manifest)
	return hex.EncodeToString(sum[:])
}

// download the archive of the commit and unpack the files of the manifest into dir, checking
// each against its SHA-256. It unpacks into a temporary directory next to dir and renames it,
// so that dir is either complete or absent.
func download(ctx context.Context, dir string, sums map[string]string, marker string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", archiveURL, res.Status)
	}
	if err := unpack(res.Body, tmp, sums); err != nil {
		return fmt.Errorf("unpacking %s: %w", archiveURL, err)
	}
	if err := os.WriteFile(filepath.Join(tmp, markerName), []byte(marker+"\n"), 0o644); err != nil {
		return err
	}
	// Another process may have filled the cache meanwhile, or left an incomplete one.
	complete := func() bool {
		b, err := os.ReadFile(filepath.Join(dir, markerName))
		return err == nil && strings.TrimSpace(string(b)) == marker
	}
	if complete() {
		return nil
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil && !complete() {
		return err
	}
	return nil
}

// unpack the files of the manifest from a gzipped tar archive into dir, and fail when a file
// is missing or does not match its SHA-256. Every other entry of the archive is skipped, so
// that only names from the manifest are ever written.
func unpack(r io.Reader, dir string, sums map[string]string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// Names in the archive start with a directory named after the repository and commit.
		_, name, ok := strings.Cut(h.Name, "/")
		if !ok {
			continue
		}
		if name != license {
			if name, ok = strings.CutPrefix(name, "test/"); !ok {
				continue
			}
		}
		want, ok := sums[name]
		if !ok {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if got := sha256Hex(b); got != want {
			return fmt.Errorf("%s has SHA-256 %s, want %s", name, got, want)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return err
		}
		found[name] = true
	}
	for name := range sums {
		if !found[name] {
			return fmt.Errorf("%s is missing from the archive", name)
		}
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// read a test file and check it against the manifest.
func (c *corpus) read(name string) ([]byte, error) {
	want, ok := c.sums[name]
	if !ok {
		return nil, fmt.Errorf("sqllogictest: %s is not in the manifest", name)
	}
	b, err := os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(name)))
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(b); got != want {
		return nil, fmt.Errorf("sqllogictest: %s in %s has SHA-256 %s, want %s; delete the directory to download the corpus again", name, c.dir, got, want)
	}
	return b, nil
}

// largestFirst returns the files ordered by size, largest first, and by name among files of one
// size, with the files that cannot be read last. Workers that take the files in this order end
// close together, because the largest files, which take longest, do not start last.
func (c *corpus) largestFirst(files []string) []string {
	size := map[string]int64{}
	for _, name := range files {
		size[name] = -1
		if info, err := os.Stat(filepath.Join(c.dir, filepath.FromSlash(name))); err == nil {
			size[name] = info.Size()
		}
	}
	out := slices.Clone(files)
	slices.SortStableFunc(out, func(a, b string) int {
		if size[a] != size[b] {
			return cmp.Compare(size[b], size[a])
		}
		return strings.Compare(a, b)
	})
	return out
}

// match the test files against comma-separated globs, where * matches within a path element
// and ** matches any number of them, such as "index/between/**,random/expr/slt_good_1*.test".
// Every glob must match a file.
func (c *corpus) match(globs string) ([]string, error) {
	var out []string
	for _, glob := range strings.Split(globs, ",") {
		glob = strings.TrimSpace(glob)
		n := 0
		for _, f := range c.files {
			ok, err := globMatch(glob, f)
			if err != nil {
				return nil, err
			}
			if ok && !slices.Contains(out, f) {
				out = append(out, f)
			}
			if ok {
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("sqllogictest: no test file matches %q", glob)
		}
	}
	sort.Strings(out)
	return out, nil
}

// globMatch reports whether name matches the glob of [corpus.match].
func globMatch(glob, name string) (bool, error) {
	g, n := strings.Split(glob, "/"), strings.Split(name, "/")
	var matchFrom func(i, j int) (bool, error)
	matchFrom = func(i, j int) (bool, error) {
		if i == len(g) {
			return j == len(n), nil
		}
		if g[i] == "**" {
			for k := j; k <= len(n); k++ {
				if ok, err := matchFrom(i+1, k); ok || err != nil {
					return ok, err
				}
			}
			return false, nil
		}
		if j == len(n) {
			return false, nil
		}
		ok, err := path.Match(g[i], n[j])
		if !ok || err != nil {
			return false, err
		}
		return matchFrom(i+1, j+1)
	}
	return matchFrom(0, 0)
}
