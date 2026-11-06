package sqllogictest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"

	"maragu.dev/maragudb/internal/pgdiff"
)

// engine whose onlyif conditions select records. Records only for other engines, such as
// those in MySQL's dialect, do not run. The skipif conditions of statements and queries are
// ignored, because Postgres is the oracle: a statement that the corpus skips on PostgreSQL
// runs anyway, and maragudb must agree with what Postgres does with it. A halt follows its
// conditions.
const engine = "postgresql"

// fileReport counts what the records of a test file did.
type fileReport struct {
	pgdiff.Report
	// Records in the file, of which OtherEngine were for other engines or after a halt, and
	// Dropped were dropped by [rewrite]. Every other record is a statement in the Report, which
	// ran on both sides, was rejected, or was Skipped.
	Records, OtherEngine, Dropped int
	// SetupFailed counts the statements that failed on both sides, which a query of the corpus
	// does not do, by the Postgres error. They are the corpus's own setup that Postgres does not
	// run as SQLite does, such as an insert of a NULL into a column that the primary key rewrite
	// made part of the key.
	SetupFailed map[string]int
	// Skipped counts the statements that were not run because they refer to a table or view
	// whose creation was rejected or failed, by the reason it was.
	Skipped    int
	SkippedFor map[string]int
	// Mismatch is the first difference between the databases, which ends the file.
	Mismatch error
}

func newFileReport() fileReport {
	return fileReport{
		Report:      pgdiff.Report{ErrorClasses: map[string]int{}, RejectedFor: map[string]int{}},
		SetupFailed: map[string]int{},
		SkippedFor:  map[string]int{},
	}
}

// add another report's counts.
func (r *fileReport) add(o fileReport) {
	r.Add(o.Report)
	r.Records += o.Records
	r.OtherEngine += o.OtherEngine
	r.Dropped += o.Dropped
	r.Skipped += o.Skipped
	for k, v := range o.SetupFailed {
		r.SetupFailed[k] += v
	}
	for k, v := range o.SkippedFor {
		r.SkippedFor[k] += v
	}
}

// fileRunner runs test files on Postgres and on a fresh maragudb for every file.
type fileRunner struct {
	// pg is the Postgres database. Every file runs in a schema of its own, named after the
	// worker that runs it, so that workers can run files at the same time.
	pg *sql.DB
	// newMaragudb opens a fresh, empty maragudb.
	newMaragudb func() (*sql.DB, error)
}

// run the records of a test file, and return what they did. A difference between the
// databases ends the file and is in the report's Mismatch; the error is for failures of the
// harness and the databases themselves.
func (r *fileRunner) run(ctx context.Context, worker int, name string, content []byte) (fileReport, error) {
	rep := newFileReport()
	records, err := parse(bytes.NewReader(content))
	if err != nil {
		return rep, fmt.Errorf("%s: %w", name, err)
	}
	rep.Records = countStatements(records)
	var applicable []record
	for i, rec := range records {
		if rec.kind == kindHalt {
			if rec.appliesTo(engine) {
				rep.OtherEngine += countStatements(records[i:])
				break
			}
			continue
		}
		if forOtherEngine(rec) {
			rep.OtherEngine++
			continue
		}
		applicable = append(applicable, rec)
	}
	applicable, rep.Dropped = rewrite(applicable)

	m, err := r.newMaragudb()
	if err != nil {
		return rep, err
	}
	defer func() { _ = m.Close() }()
	mConn, err := m.Conn(ctx)
	if err != nil {
		return rep, fmt.Errorf("maragudb: %w", err)
	}
	defer func() { _ = mConn.Close() }()
	pgConn, err := r.postgres(ctx, worker)
	if err != nil {
		return rep, err
	}
	defer func() { _ = pgConn.Close() }()
	session := pgdiff.NewSession(pgConn, mConn)
	// The connection goes back to the pool without a transaction, also after a mismatch in the
	// middle of one, which the session may not know is open.
	defer func() { _, _ = pgConn.ExecContext(context.Background(), "ROLLBACK") }()

	// missing maps the tables and views whose creation was rejected or failed to the reason.
	missing := map[string]string{}
	for _, rec := range applicable {
		toks := lex(rec.sql)
		created := createdName(toks)
		if cause := dependsOn(toks, missing, created); cause != "" {
			rep.Statements++
			rep.Skipped++
			rep.SkippedFor[cause]++
			if created != "" {
				missing[created] = cause
			}
			continue
		}
		o, err := session.Exec(ctx, rec.sql, &rep.Report)
		if err != nil {
			if pgdiff.IsMismatch(err) {
				rep.Mismatch = fmt.Errorf("%s:%d: %w", name, rec.line, err)
				return rep, nil
			}
			return rep, fmt.Errorf("%s:%d: %w", name, rec.line, err)
		}
		switch {
		case !o.Executed && created != "":
			missing[created] = "rejected: " + o.Rejection
		case o.Err != nil && created != "" && pgdiff.SQLState(o.Err) != "42P07":
			missing[created] = "failed on Postgres: " + pgReason(o.Err)
		case created != "":
			delete(missing, created)
		}
		if o.Err != nil && rec.kind == kindStatement {
			rep.SetupFailed[pgReason(o.Err)]++
		}
	}
	return rep, nil
}

// postgres returns a connection to Postgres in a fresh, empty schema of the worker, which is
// the connection's search path. The connection has the settings of every connection that
// [pgdiff.Postgres] gives, among them enable_seqscan=off.
func (r *fileRunner) postgres(ctx context.Context, worker int) (*sql.Conn, error) {
	conn, err := r.pg.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	schema := fmt.Sprintf("sqllogictest_%d", worker)
	for _, q := range []string{"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema, "SET search_path TO " + schema} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("postgres: %s: %w", q, err)
		}
	}
	return conn, nil
}

// forOtherEngine reports whether an onlyif condition of the record names another engine.
func forOtherEngine(r record) bool {
	for _, c := range r.conditions {
		if !c.skip && c.engine != engine {
			return true
		}
	}
	return false
}

func countStatements(records []record) int {
	n := 0
	for _, r := range records {
		if r.kind != kindHalt {
			n++
		}
	}
	return n
}

// createdName is the lowercased name of the table or view that a statement creates, or "".
func createdName(toks []token) string {
	var words []string
	for _, i := range significantIndexes(toks) {
		words = append(words, toks[i].text)
		if len(words) == 7 {
			break
		}
	}
	upper := func(i int) string {
		if i < len(words) {
			return strings.ToUpper(words[i])
		}
		return ""
	}
	if upper(0) != "CREATE" {
		return ""
	}
	i := 1
	for upper(i) == "TEMP" || upper(i) == "TEMPORARY" || upper(i) == "UNIQUE" {
		i++
	}
	switch upper(i) {
	case "TABLE", "VIEW", "TRIGGER", "INDEX":
	default:
		return ""
	}
	i++
	if upper(i) == "IF" && upper(i+1) == "NOT" && upper(i+2) == "EXISTS" {
		i += 3
	}
	if i < len(words) {
		return identifier(words[i])
	}
	return ""
}

// dependsOn returns the reason why a table or view that the statement refers to is missing,
// or "" when none is. The name that the statement creates does not count. Any word that is the
// name counts, also a column of the same name, so it skips too much rather than too little.
func dependsOn(toks []token, missing map[string]string, created string) string {
	if len(missing) == 0 {
		return ""
	}
	for _, t := range toks {
		if t.kind != tokWord && t.kind != tokQuoted {
			continue
		}
		name := identifier(t.text)
		if cause, ok := missing[name]; ok && name != created {
			return cause
		}
	}
	return ""
}

// identifier as Postgres names it: unquoted names in lower case.
func identifier(s string) string {
	if len(s) >= 2 && s[0] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
}

// pgReason is the SQLSTATE and message of a Postgres error, with names and numbers replaced,
// so that failures of one kind count together.
func pgReason(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err.Error()
	}
	return pgErr.Code + " " + reasonDetail.ReplaceAllString(pgErr.Message, "_")
}

var reasonDetail = regexp.MustCompile(`"[^"]*"|\b\d+\b`)

// topCounts formats the n largest counts, largest first.
func topCounts(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range m {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v != kvs[j].v {
			return kvs[i].v > kvs[j].v
		}
		return kvs[i].k < kvs[j].k
	})
	var parts []string
	for i, e := range kvs {
		if i == n {
			parts = append(parts, fmt.Sprintf("and %d more", len(kvs)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", e.k, e.v))
	}
	return strings.Join(parts, ", ")
}

// runFiles of the corpus on workers at the same time, and return the report of every file. It
// stops at the first failure of the harness or the databases, but not at a mismatch, which is
// in the file's report.
func (r *fileRunner) runFiles(ctx context.Context, c *corpus, files []string, workers int) (map[string]fileReport, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	reports := map[string]fileReport{}
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for name := range jobs {
				content, err := c.read(name)
				var rep fileReport
				if err == nil {
					rep, err = r.run(ctx, w, name, content)
				}
				mu.Lock()
				reports[name] = rep
				if err != nil && firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	for _, name := range files {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- name:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return reports, firstErr
}
