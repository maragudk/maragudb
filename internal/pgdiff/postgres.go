// Package pgdiff is the differential test harness against Postgres: hegel generates schemas,
// rows, and statements as an AST, which is rendered to both dialects and run on both databases,
// and the results must agree.
package pgdiff

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Postgres driver for database/sql.
)

// container name of the local Postgres.
const container = "maragudb-pgdiff"

// Postgres returns a connection to a database collated with "C" for the harness. It uses
// MARAGUDB_PGDIFF_URL when set, the CI service when CI is set, and otherwise starts a
// postgres:18 Docker container, which stays running for later runs. It fails the test, never
// skips, when Postgres is unavailable. Every connection runs with enable_seqscan=off, so that
// Postgres scans through the primary key index wherever it can, as [replaceDatabase] explains.
//
// Every test process gets a database of its own, created on first use from template0 under a
// unique name, so that test processes sharing one server never meet. [DropDatabase] drops it at
// the end of the process, and databases that earlier processes left behind are dropped once
// they are a day old.
func Postgres(t *testing.T) *sql.DB {
	t.Helper()
	processDB.once.Do(func() {
		processDB.url, processDB.name, processDB.admin, processDB.err = createDatabase(t.Context())
	})
	if processDB.err != nil {
		t.Fatal(processDB.err)
	}
	db, err := sql.Open("pgx", processDB.url)
	if err != nil {
		t.Fatalf("pgdiff: opening %s: %v", processDB.url, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var collate string
	if err := db.QueryRowContext(t.Context(), "SELECT datcollate FROM pg_database WHERE datname = $1", processDB.name).Scan(&collate); err != nil {
		t.Fatalf("pgdiff: reading collation: %v", err)
	}
	if collate != "C" {
		t.Fatalf("pgdiff: database %s has collation %q, want C", processDB.name, collate)
	}
	return db
}

// processDB is the database of this test process.
var processDB struct {
	once             sync.Once
	url, name, admin string
	err              error
}

// databasePrefix of the names of the harness's databases.
const databasePrefix = "maragudb_diff_"

// createDatabase finds Postgres, drops stale databases of the harness, and creates this
// process's database. It returns the database's URL, its name, and the admin URL.
func createDatabase(ctx context.Context) (string, string, string, error) {
	url := os.Getenv("MARAGUDB_PGDIFF_URL")
	switch {
	case url != "":
	case os.Getenv("CI") != "":
		url = "postgres://test:test@localhost:5433/postgres"
	default:
		var err error
		if url, err = startContainer(ctx); err != nil {
			return "", "", "", fmt.Errorf("pgdiff: Postgres is required and Docker could not provide it: %w\nSet MARAGUDB_PGDIFF_URL to a Postgres superuser URL to use another server", err)
		}
	}
	admin, err := sql.Open("pgx", url)
	if err != nil {
		return "", "", "", fmt.Errorf("pgdiff: opening %s: %w", url, err)
	}
	defer func() { _ = admin.Close() }()
	if err := waitReady(ctx, admin); err != nil {
		return "", "", "", fmt.Errorf("pgdiff: Postgres at %s did not answer: %w", url, err)
	}
	dropStale(ctx, admin, time.Now().Add(-24*time.Hour))
	random := make([]byte, 4)
	_, _ = rand.Read(random)
	name := fmt.Sprintf("%s%d_%x", databasePrefix, time.Now().Unix(), random)
	// A fresh template with the C locale, so that strings compare by byte order.
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name+" TEMPLATE template0 ENCODING 'UTF8' LOCALE 'C'"); err != nil {
		return "", "", "", fmt.Errorf("pgdiff: creating database %s: %w", name, err)
	}
	// Plans are not cached across statements, because every case recreates the table with a
	// different row type.
	return replaceDatabase(url, name), name, url, nil
}

// dropStale drops the harness's databases created before the cutoff, which processes that did
// not end cleanly left behind, and the single shared database of earlier versions of the
// harness. A database still in use is left alone.
func dropStale(ctx context.Context, admin *sql.DB, cutoff time.Time) {
	rows, err := admin.QueryContext(ctx, "SELECT datname FROM pg_database WHERE datname LIKE 'maragudb\\_diff%'")
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		if name == "maragudb_diff" {
			stale = append(stale, name)
			continue
		}
		created, _, ok := strings.Cut(strings.TrimPrefix(name, databasePrefix), "_")
		if unix, err := strconv.ParseInt(created, 10, 64); ok && err == nil && time.Unix(unix, 0).Before(cutoff) {
			stale = append(stale, name)
		}
	}
	_ = rows.Close()
	for _, name := range stale {
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name)
	}
}

// DropDatabase drops the database of this test process, if it created one. Call it once at the
// end of the process, after every test.
func DropDatabase() {
	if processDB.name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", processDB.admin)
	if err != nil {
		return
	}
	defer func() { _ = admin.Close() }()
	_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+processDB.name+" WITH (FORCE)")
}

// replaceDatabase in a Postgres URL, keeping its query and adding the pgx mode that describes
// every statement instead of caching plans, and the setting enable_seqscan=off, which pgx sends
// when it opens a connection, so that it is the default of every connection of the pool.
//
// With sequential scans off, Postgres scans a table through its primary key index whenever a
// condition of the WHERE clause can use the index, and evaluates the other conditions only on
// the rows the index finds, as maragudb does. Bitmap scans stay on: a bitmap heap scan also
// checks the index conditions before the other ones, and without it Postgres would choose by
// cost between two disabled plans for an OR of key ranges.
func replaceDatabase(url, name string) string {
	const params = "default_query_exec_mode=describe_exec&enable_seqscan=off"
	i := strings.LastIndex(url, "/")
	if j := strings.Index(url[i:], "?"); j >= 0 {
		return url[:i+1] + name + url[i+j:] + "&" + params
	}
	return url[:i+1] + name + "?" + params
}

func waitReady(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(60 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = db.PingContext(ctx); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// startContainer starts the Postgres container when it is not running and returns its URL.
func startContainer(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", errors.New("docker is not installed")
	}
	port, err := containerPort(ctx)
	if err != nil {
		// Remove any stopped container of the same name, then start a new one on a free port.
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", container).Run()
		out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", container,
			"-e", "POSTGRES_USER=test", "-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=template1",
			"-p", "127.0.0.1:0:5432", "postgres:18").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("docker run postgres:18: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if port, err = containerPort(ctx); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("postgres://test:test@127.0.0.1:%s/postgres", port), nil
}

// containerPort of the running container, or an error when it is not running.
func containerPort(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", `{{.State.Running}} {{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}`, container).Output()
	if err != nil {
		return "", fmt.Errorf("container %s is not running", container)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != "true" {
		return "", fmt.Errorf("container %s is not running", container)
	}
	return fields[1], nil
}
