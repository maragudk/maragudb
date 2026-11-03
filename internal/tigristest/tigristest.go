// Package tigristest configures integration tests against a real Tigris bucket from the
// environment, loading a .env file from the repository root when present.
package tigristest

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/maragudb/internal/s3"
)

// Config for a Tigris test.
type Config struct {
	Bucket   string
	Endpoint string
	Region   string
	// Prefix is unique to the test run.
	Prefix string
}

var loadOnce sync.Once

// loadEnv sets variables from the repository's .env file, without overriding the environment.
func loadEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
	f, err := os.Open(filepath.Join(dir, ".env"))
	if err != nil {
		return
	}
	defer func() {
		_ = f.Close()
	}()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

// Load the configuration. It fails the test and names the missing variables when any of
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_ENDPOINT_URL_S3, or BUCKET is unset.
// AWS_REGION is optional and defaults to "auto".
func Load(t testing.TB) Config {
	t.Helper()
	loadOnce.Do(loadEnv)
	if missing := Missing(os.Getenv); len(missing) > 0 {
		t.Fatalf("Tigris integration test cannot run: missing environment variables %s (set them or put them in .env at the repository root)", strings.Join(missing, ", "))
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "auto"
	}
	return Config{
		Bucket:   os.Getenv("BUCKET"),
		Endpoint: os.Getenv("AWS_ENDPOINT_URL_S3"),
		Region:   region,
		Prefix:   fmt.Sprintf("maragudb-test/%s-%d", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), time.Now().UnixNano()),
	}
}

// Missing required variables, looked up with getenv.
func Missing(getenv func(string) string) []string {
	var missing []string
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_ENDPOINT_URL_S3", "BUCKET"} {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// NewClient for the bucket. It cleans up every object under the prefix when the test ends.
func NewClient(t testing.TB) (*s3.AWSClient, Config) {
	t.Helper()
	cfg := Load(t)
	client, err := s3.NewAWSClient(context.Background(), s3.AWSOptions{Bucket: cfg.Bucket, Endpoint: cfg.Endpoint, Region: cfg.Region})
	if err != nil {
		t.Fatalf("creating Tigris client: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		infos, err := client.List(ctx, cfg.Prefix)
		if err != nil {
			t.Logf("cleanup: listing %q: %v", cfg.Prefix, err)
			return
		}
		for _, info := range infos {
			if err := client.Delete(ctx, info.Key); err != nil {
				t.Logf("cleanup: deleting %q: %v", info.Key, err)
			}
		}
	})
	return client, cfg
}
