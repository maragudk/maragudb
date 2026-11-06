package sqllogictest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"
)

// archive is a gzipped tar archive with the files, under a top directory as GitHub makes it.
func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	is.NotError(t, tw.WriteHeader(&tar.Header{Name: "repo-sha/", Typeflag: tar.TypeDir, Mode: 0o755}))
	for name, content := range files {
		is.NotError(t, tw.WriteHeader(&tar.Header{Name: "repo-sha/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}))
		_, err := tw.Write([]byte(content))
		is.NotError(t, err)
	}
	is.NotError(t, tw.Close())
	is.NotError(t, gz.Close())
	return buf.Bytes()
}

func TestUnpack(t *testing.T) {
	sums := map[string]string{
		license:      sha256Hex([]byte("MIT")),
		"a/one.test": sha256Hex([]byte("statement ok\nSELECT 1\n")),
		"b/two.test": sha256Hex([]byte("query I\nSELECT 2\n")),
	}

	t.Run("writes the files of the manifest and nothing else", func(t *testing.T) {
		dir := t.TempDir()
		a := archive(t, map[string]string{
			license:            "MIT",
			"test/a/one.test":  "statement ok\nSELECT 1\n",
			"test/b/two.test":  "query I\nSELECT 2\n",
			"test/c/more.test": "statement ok\nSELECT 3\n",
			"src/run.c":        "int main;",
			"../escape.test":   "x",
		})
		is.NotError(t, unpack(bytes.NewReader(a), dir, sums))
		b, err := os.ReadFile(filepath.Join(dir, "a", "one.test"))
		is.NotError(t, err)
		is.Equal(t, "statement ok\nSELECT 1\n", string(b))
		_, err = os.Stat(filepath.Join(dir, "c", "more.test"))
		is.True(t, os.IsNotExist(err), "a file outside the manifest is not written")
		_, err = os.Stat(filepath.Join(dir, "src"))
		is.True(t, os.IsNotExist(err), "a file outside the test directory is not written")
	})

	t.Run("fails when a file does not match its checksum", func(t *testing.T) {
		a := archive(t, map[string]string{license: "MIT", "test/a/one.test": "statement ok\nSELECT 666\n", "test/b/two.test": "query I\nSELECT 2\n"})
		err := unpack(bytes.NewReader(a), t.TempDir(), sums)
		is.True(t, err != nil && strings.Contains(err.Error(), "a/one.test has SHA-256"), "checksum mismatch")
	})

	t.Run("fails when a file of the manifest is missing", func(t *testing.T) {
		a := archive(t, map[string]string{license: "MIT", "test/a/one.test": "statement ok\nSELECT 1\n"})
		err := unpack(bytes.NewReader(a), t.TempDir(), sums)
		is.True(t, err != nil && strings.Contains(err.Error(), "b/two.test is missing"), "missing file")
	})
}

func TestParseManifest(t *testing.T) {
	t.Run("the committed manifest names the license and hundreds of test files", func(t *testing.T) {
		sums, files, err := parseManifest(manifest)
		is.NotError(t, err)
		is.Equal(t, 622, len(files))
		is.Equal(t, 623, len(sums))
		is.Equal(t, "evidence/in1.test", files[0])
	})

	t.Run("fails on a bad line", func(t *testing.T) {
		_, _, err := parseManifest([]byte("abc  ../x.test\n"))
		is.True(t, err != nil, "bad line")
	})
}

func TestCorpus_read(t *testing.T) {
	t.Run("reads a file that matches the manifest, and fails on one that does not", func(t *testing.T) {
		dir := t.TempDir()
		is.NotError(t, os.WriteFile(filepath.Join(dir, "good.test"), []byte("good"), 0o644))
		is.NotError(t, os.WriteFile(filepath.Join(dir, "bad.test"), []byte("bad"), 0o644))
		c := &corpus{dir: dir, sums: map[string]string{"good.test": sha256Hex([]byte("good")), "bad.test": sha256Hex([]byte("worse"))}}
		b, err := c.read("good.test")
		is.NotError(t, err)
		is.Equal(t, "good", string(b))
		_, err = c.read("bad.test")
		is.True(t, err != nil, "checksum mismatch")
		_, err = c.read("other.test")
		is.True(t, err != nil, "not in the manifest")
	})
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		glob, name string
		want       bool
	}{
		{"index/between/**", "index/between/10/slt_good_0.test", true},
		{"index/**/slt_good_0.test", "index/in/10/slt_good_0.test", true},
		{"**", "select1.test", true},
		{"random/expr/slt_good_1*.test", "random/expr/slt_good_12.test", true},
		{"random/expr/slt_good_1*.test", "random/expr/slt_good_2.test", false},
		{"index/*", "index/between/10/slt_good_0.test", false},
		{"select1.test", "select1.test", true},
	}
	for _, test := range tests {
		t.Run(test.glob+" "+test.name, func(t *testing.T) {
			got, err := globMatch(test.glob, test.name)
			is.NotError(t, err)
			is.Equal(t, test.want, got)
		})
	}
}
