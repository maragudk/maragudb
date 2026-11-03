// Package pgdiff is the differential test harness against Postgres: hegel generates schemas,
// rows, and statements as an AST, which is rendered to both dialects and run on both databases,
// and the results must agree.
package pgdiff

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Postgres driver for database/sql.
)

// container name of the local Postgres.
const container = "maragudb-pgdiff"

// Postgres returns a connection to a database collated with "C" for the harness. It uses
// MARAGUDB_PGDIFF_URL when set, the CI service when CI is set, and otherwise starts a
// postgres:18 Docker container, which stays running for later runs. It fails the test, never
// skips, when Postgres is unavailable.
func Postgres(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("MARAGUDB_PGDIFF_URL")
	switch {
	case url != "":
	case os.Getenv("CI") != "":
		url = "postgres://test:test@localhost:5433/postgres"
	default:
		var err error
		if url, err = startContainer(t.Context()); err != nil {
			t.Fatalf("pgdiff: Postgres is required and Docker could not provide it: %v\nSet MARAGUDB_PGDIFF_URL to a Postgres superuser URL to use another server.", err)
		}
	}
	admin, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("pgdiff: opening %s: %v", url, err)
	}
	defer func() { _ = admin.Close() }()
	if err := waitReady(t.Context(), admin); err != nil {
		t.Fatalf("pgdiff: Postgres at %s did not answer: %v", url, err)
	}
	const name = "maragudb_diff"
	var exists bool
	if err := admin.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("pgdiff: checking for database: %v", err)
	}
	if !exists {
		// A fresh template with the C locale, so that strings compare by byte order.
		if _, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+name+" TEMPLATE template0 ENCODING 'UTF8' LOCALE 'C'"); err != nil {
			t.Fatalf("pgdiff: creating database: %v", err)
		}
	}
	// Plans are not cached across statements, because every case recreates the table with a
	// different row type.
	dbURL := replaceDatabase(url, name)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("pgdiff: opening %s: %v", dbURL, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var collate string
	if err := db.QueryRowContext(t.Context(), "SELECT datcollate FROM pg_database WHERE datname = $1", name).Scan(&collate); err != nil {
		t.Fatalf("pgdiff: reading collation: %v", err)
	}
	if collate != "C" {
		t.Fatalf("pgdiff: database %s has collation %q, want C", name, collate)
	}
	return db
}

// replaceDatabase in a Postgres URL, keeping its query and adding the pgx mode that describes
// every statement instead of caching plans.
func replaceDatabase(url, name string) string {
	i := strings.LastIndex(url, "/")
	if j := strings.Index(url[i:], "?"); j >= 0 {
		return url[:i+1] + name + url[i+j:] + "&default_query_exec_mode=describe_exec"
	}
	return url[:i+1] + name + "?default_query_exec_mode=describe_exec"
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
