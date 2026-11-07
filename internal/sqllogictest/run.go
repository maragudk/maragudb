package sqllogictest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	// Records in the file, of which OtherEngine were for other engines or after a halt. Every
	// other record is a statement in the Report, which ran on both sides, was rejected, or was
	// Skipped.
	Records, OtherEngine int
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
	// shardQueries, when positive, is the most read-only queries at the end of a file that run
	// one after another. A file with more of them, and without transactions, splits them into
	// shards of at most that many, which run at the same time, each on databases of its own. Zero
	// runs every file in one sequence.
	shardQueries int
}

// run the records of a test file, and return what they did. A difference between the
// databases ends the file and is in the report's Mismatch; the error is for failures of the
// harness and the databases themselves. slots, when not nil, bounds the shards that run at the
// same time, across the files of a run.
//
// A read-only query outside a transaction changes nothing, so it gives the same result on any
// databases that ran the records before it. [shardStarts] splits the read-only queries at the
// end of a file into shards, which run at the same time: the first after the records before
// the queries, and every other on fresh databases that first run those records again, without
// the read-only queries among them and without counting them again. A shard ends at its first
// difference. The report has the difference of the earliest shard, and counts what a run
// in one sequence counts: the shards up to that one.
func (r *fileRunner) run(ctx context.Context, worker int, name string, content []byte, slots chan struct{}) (fileReport, error) {
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
	applicable = rewrite(applicable)

	starts := shardStarts(applicable, r.shardQueries)
	// setup are the records that every shard after the first runs again.
	var setup []record
	if len(starts) > 1 {
		for _, rec := range applicable[:starts[1]] {
			if !readOnly(rec) {
				setup = append(setup, rec)
			}
		}
	}
	reports := make([]fileReport, len(starts))
	errs := make([]error, len(starts))
	var wg sync.WaitGroup
	for i, start := range starts {
		end := len(applicable)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		reports[i] = newFileReport()
		wg.Go(func() {
			if slots != nil {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					errs[i] = ctx.Err()
					return
				}
			}
			schema := fmt.Sprintf("sqllogictest_%d", worker)
			if i > 0 {
				schema = fmt.Sprintf("sqllogictest_%d_%d", worker, i)
			}
			d, err := r.open(ctx, schema)
			if err != nil {
				errs[i] = err
				return
			}
			defer d.close()
			missing := map[string]string{}
			if i > 0 {
				// The first shard counts these records.
				again := newFileReport()
				if done, err := d.exec(ctx, name, setup, missing, &again); done || err != nil {
					reports[i].Mismatch, errs[i] = again.Mismatch, err
					return
				}
			}
			_, errs[i] = d.exec(ctx, name, applicable[start:end], missing, &reports[i])
		})
	}
	wg.Wait()
	for i := range starts {
		rep.add(reports[i])
		if rep.Mismatch = reports[i].Mismatch; rep.Mismatch != nil {
			break
		}
	}
	return rep, errors.Join(errs...)
}

// shardStarts returns the index of the first record of every shard of the records, in order,
// which is [0] for records that run in one sequence. Records that end with more than size
// read-only queries, and that have no statement that begins or ends a transaction, split those
// queries into the fewest shards of at most size each, as even as can be. The first shard also
// has the records before the queries.
func shardStarts(records []record, size int) []int {
	if size <= 0 || slices.ContainsFunc(records, transactional) {
		return []int{0}
	}
	end := len(records)
	for end > 0 && readOnly(records[end-1]) {
		end--
	}
	queries := len(records) - end
	if queries <= size {
		return []int{0}
	}
	n := (queries + size - 1) / size
	starts := []int{0}
	for i := 1; i < n; i++ {
		starts = append(starts, end+i*queries/n)
	}
	return starts
}

// readOnly reports whether the record is a query that only reads: a SELECT without INTO.
// maragudb rejects the functions that change state, such as nextval, and a statement that it
// rejects does not run on Postgres, so such a query changes nothing on either side.
func readOnly(rec record) bool {
	if rec.kind != kindQuery {
		return false
	}
	toks := lex(rec.sql)
	if words := keywords(toks); len(words) == 0 || words[0] != "SELECT" {
		return false
	}
	for _, tok := range toks {
		if tok.kind == tokWord && strings.EqualFold(tok.text, "INTO") {
			return false
		}
	}
	return true
}

// transactional reports whether the record begins or ends a transaction or works on one.
func transactional(rec record) bool {
	words := keywords(lex(rec.sql))
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE":
		return true
	}
	return false
}

// databases on which a file, or a shard of one, runs: a fresh maragudb and Postgres in a fresh
// schema, compared by a session.
type databases struct {
	m       *sql.DB
	mConn   *sql.Conn
	pgConn  *sql.Conn
	session *pgdiff.Session
}

// open fresh databases, with Postgres in a fresh, empty schema of the name.
func (r *fileRunner) open(ctx context.Context, schema string) (*databases, error) {
	m, err := r.newMaragudb()
	if err != nil {
		return nil, err
	}
	mConn, err := m.Conn(ctx)
	if err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("maragudb: %w", err)
	}
	pgConn, err := r.postgres(ctx, schema)
	if err != nil {
		_ = mConn.Close()
		_ = m.Close()
		return nil, err
	}
	return &databases{m: m, mConn: mConn, pgConn: pgConn, session: pgdiff.NewSession(pgConn, mConn)}, nil
}

// close the databases. The Postgres connection goes back to the pool without a transaction,
// also after a mismatch in the middle of one, which the session may not know is open.
func (d *databases) close() {
	_, _ = d.pgConn.ExecContext(context.Background(), "ROLLBACK")
	_ = d.pgConn.Close()
	_ = d.mConn.Close()
	_ = d.m.Close()
}

// exec the records on the databases, and count what they did in the report. missing maps the
// tables and views whose creation was rejected or failed to the reason, and exec updates it. A
// difference between the databases ends the records, is in the report's Mismatch, and makes
// done true.
func (d *databases) exec(ctx context.Context, name string, records []record, missing map[string]string, rep *fileReport) (done bool, err error) {
	for _, rec := range records {
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
		o, err := d.session.Exec(ctx, rec.sql, &rep.Report)
		if err != nil {
			if pgdiff.IsMismatch(err) {
				rep.Mismatch = fmt.Errorf("%s:%d: %w", name, rec.line, err)
				return true, nil
			}
			return true, fmt.Errorf("%s:%d: %w", name, rec.line, err)
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
	return false, nil
}

// postgres returns a connection to Postgres in a fresh, empty schema of the name, which is the
// connection's search path. The connection has the settings of every connection that
// [pgdiff.Postgres] gives, among them enable_seqscan=off.
func (r *fileRunner) postgres(ctx context.Context, schema string) (*sql.Conn, error) {
	conn, err := r.pg.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
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

// runFiles of the corpus on workers at the same time, largest first, and return the report of
// every file. It stops at the first failure of the harness or the databases, but not at a
// mismatch, which is in the file's report.
func (r *fileRunner) runFiles(ctx context.Context, c *corpus, files []string, workers int) (map[string]fileReport, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	// slots bound the shards that run at the same time, and so the connections to Postgres, to
	// the number of workers.
	slots := make(chan struct{}, workers)
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
					rep, err = r.run(ctx, w, name, content, slots)
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
	for _, name := range c.largestFirst(files) {
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
