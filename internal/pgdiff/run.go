package pgdiff

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/sql/ast"
)

// Runner runs cases on Postgres and on a fresh maragudb per case. Runs are serialized, because
// they share one table on the Postgres side.
type Runner struct {
	PG *sql.DB
	// New opens a fresh, empty maragudb.
	New func() (*sql.DB, error)

	mu sync.Mutex
}

// Report counts what a run did.
type Report struct {
	// Statements generated, of which Executed ran on both sides and Rejected were rejected by
	// maragudb with [maragudb.ErrUnsupported] and skipped on Postgres.
	Statements, Executed, Rejected int
	// RejectedFor counts the rejections by the reason in their message.
	RejectedFor map[string]int
	// OK counts statements that succeeded on both sides, and Errors those that failed on both.
	OK, Errors int
	// Checks counts the state checks after mutations.
	Checks int
	// Rows returned by successful SELECTs on maragudb.
	Rows int
	// Ordered counts SELECTs compared in order, as opposed to as multisets.
	Ordered int
	// Reordered counts statements that failed on both sides for reasons that only agree when
	// Postgres visits the rows in key order, as maragudb does.
	Reordered int
	// Transactions counts the explicit transactions begun on both sides.
	Transactions int
	// ErrorClasses counts the statements that failed on both sides, by maragudb error class and
	// SQLSTATE.
	ErrorClasses map[string]int
}

// Add another report's counts.
func (r *Report) Add(o Report) {
	r.Statements += o.Statements
	r.Executed += o.Executed
	r.Rejected += o.Rejected
	r.OK += o.OK
	r.Errors += o.Errors
	r.Checks += o.Checks
	r.Rows += o.Rows
	r.Ordered += o.Ordered
	r.Reordered += o.Reordered
	r.Transactions += o.Transactions
	if r.ErrorClasses == nil {
		r.ErrorClasses = map[string]int{}
	}
	for k, v := range o.ErrorClasses {
		r.ErrorClasses[k] += v
	}
	if r.RejectedFor == nil {
		r.RejectedFor = map[string]int{}
	}
	for k, v := range o.RejectedFor {
		r.RejectedFor[k] += v
	}
}

func (r Report) String() string {
	return fmt.Sprintf("%d statements: %d executed (%d ok on both, %d errors on both, %d of them agreeing in key order), %d rejected; %d transactions, %d state checks, %d rows, %d ordered comparisons; errors: %s; rejected for: %s",
		r.Statements, r.Executed, r.OK, r.Errors, r.Reordered, r.Rejected, r.Transactions, r.Checks, r.Rows, r.Ordered, counts(r.ErrorClasses), counts(r.RejectedFor))
}

func counts(m map[string]int) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// Mismatch between the two databases.
type Mismatch struct {
	Statement string
	Postgres  string
	Maragudb  string
	Detail    string
}

func (m *Mismatch) Error() string {
	return fmt.Sprintf("%s\n  postgres: %s\n  maragudb: %s\n  %s", m.Statement, m.Postgres, m.Maragudb, m.Detail)
}

// sides of a case: one connection to each database, so that explicit transactions span
// statements.
type sides struct {
	pg, m *sql.Conn
	pk    []string
	// inTx reports whether an explicit transaction is open on both sides, and readOnly whether
	// it is read-only.
	inTx, readOnly bool
}

// Run a case and compare both sides after every statement. Both run the same SQL text. A
// statement that maragudb rejects with [maragudb.ErrUnsupported] is not run on Postgres; inside
// a transaction, which the rejection aborts on maragudb, Postgres runs a failing statement
// instead, so that both transactions are aborted. The returned error describes the first
// mismatch.
func (r *Runner) Run(ctx context.Context, c Case) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := Report{ErrorClasses: map[string]int{}, RejectedFor: map[string]int{}}
	m, err := r.New()
	if err != nil {
		return rep, err
	}
	defer func() { _ = m.Close() }()
	if _, err := r.PG.ExecContext(ctx, "DROP TABLE IF EXISTS "+tableName); err != nil {
		return rep, fmt.Errorf("postgres: %w", err)
	}
	create := ast.Render(c.Create)
	if _, err := r.PG.ExecContext(ctx, create); err != nil {
		return rep, fmt.Errorf("postgres: %s: %w", create, err)
	}
	if _, err := m.ExecContext(ctx, create); err != nil {
		return rep, fmt.Errorf("maragudb: %s: %w", create, err)
	}
	s := &sides{pk: c.Create.PrimaryKey}
	if s.pg, err = r.PG.Conn(ctx); err != nil {
		return rep, fmt.Errorf("postgres: %w", err)
	}
	defer func() { _ = s.pg.Close() }()
	if s.m, err = m.Conn(ctx); err != nil {
		return rep, fmt.Errorf("maragudb: %w", err)
	}
	defer func() { _ = s.m.Close() }()
	defer func() {
		if s.inTx {
			_, _ = s.pg.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	stateCheck := &ast.Select{Items: []ast.SelectItem{{Star: true}}, From: tableName}
	for _, k := range c.Create.PrimaryKey {
		stateCheck.OrderBy = append(stateCheck.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Name: k}})
	}
	for _, st := range c.Statements {
		rep.Statements++
		executed, err := s.compare(ctx, st, &rep)
		if err != nil {
			return rep, err
		}
		if !executed {
			continue
		}
		switch st.(type) {
		case *ast.Select, *ast.Begin, *ast.Commit, *ast.Rollback:
		default:
			var check Report
			if _, err := s.compare(ctx, stateCheck, &check); err != nil {
				return rep, fmt.Errorf("state after %s: %w", ast.Render(st), err)
			}
			rep.Checks++
		}
	}
	if s.inTx {
		var end Report
		if _, err := s.compare(ctx, &ast.Rollback{}, &end); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

type result struct {
	err      error
	affected int64
	cols     []string
	rows     [][]any
}

// compare runs a statement on both sides and compares the outcomes. It reports whether the
// statement ran on both, as opposed to being rejected by maragudb.
func (s *sides) compare(ctx context.Context, st ast.Statement, rep *Report) (bool, error) {
	if rep.ErrorClasses == nil {
		rep.ErrorClasses = map[string]int{}
	}
	if rep.RejectedFor == nil {
		rep.RejectedFor = map[string]int{}
	}
	text := ast.Render(st)
	mg := run(ctx, s.m, text)
	if errors.Is(mg.err, maragudb.ErrUnsupported) {
		rep.Rejected++
		rep.RejectedFor[rejectionReason(mg.err)]++
		if s.inTx {
			if res := run(ctx, s.pg, "SELECT 1 / 0"); SQLState(res.err) != "22012" && SQLState(res.err) != "25P02" {
				return false, fmt.Errorf("postgres: aborting the transaction after a rejection: %v", res.err)
			}
		}
		return false, nil
	}
	rep.Executed++
	if s.inTx {
		if _, err := s.pg.ExecContext(ctx, "SAVEPOINT pgdiff"); err != nil && SQLState(err) != "25P02" {
			return true, fmt.Errorf("postgres: %w", err)
		}
	}
	pg := run(ctx, s.pg, text)
	mismatch := func(detail string) error {
		return &Mismatch{Statement: text, Postgres: describe(pg), Maragudb: describe(mg), Detail: detail}
	}
	if (pg.err != nil) != (mg.err != nil) {
		return true, mismatch("one side failed and the other succeeded")
	}
	if pg.err != nil {
		rep.Errors++
		if !SameClass(mg.err, pg.err) {
			if s.readOnly {
				// A read-only transaction cannot store the rows again, and the writes in it fail
				// before they read rows.
				return true, mismatch(fmt.Sprintf("both failed, for different reasons: postgres SQLSTATE %s, maragudb %s", SQLState(pg.err), className(mg.err)))
			}
			// Different rows can fail for different reasons, and maragudb visits rows in key order
			// where Postgres visits them in the order they are stored. Postgres gets the same
			// chance in key order before this counts as a mismatch.
			ordered, err := s.inKeyOrder(ctx, text)
			if err != nil {
				return true, err
			}
			if !SameClass(mg.err, ordered) {
				return true, mismatch(fmt.Sprintf("both failed, for different reasons: postgres SQLSTATE %s, maragudb %s", SQLState(pg.err), className(mg.err)))
			}
			rep.Reordered++
			pg.err = ordered
		}
		rep.ErrorClasses[ClassOf(mg.err).Name+"/"+SQLState(pg.err)]++
		return true, nil
	}
	rep.OK++
	switch st := st.(type) {
	case *ast.Begin:
		if !s.inTx {
			rep.Transactions++
			s.readOnly = st.ReadOnly
		}
		s.inTx = true
	case *ast.Commit, *ast.Rollback:
		s.inTx, s.readOnly = false, false
	}
	sel, isSelect := st.(*ast.Select)
	if !isSelect {
		if pg.affected != mg.affected {
			return true, mismatch(fmt.Sprintf("rows affected: postgres %d, maragudb %d", pg.affected, mg.affected))
		}
		return true, nil
	}
	rep.Rows += len(mg.rows)
	if strings.Join(pg.cols, ",") != strings.Join(mg.cols, ",") {
		return true, mismatch(fmt.Sprintf("columns: postgres %q, maragudb %q", pg.cols, mg.cols))
	}
	if len(pg.rows) != len(mg.rows) {
		return true, mismatch(fmt.Sprintf("row count: postgres %d, maragudb %d", len(pg.rows), len(mg.rows)))
	}
	pgRows, mgRows := canonical(pg.rows), canonical(mg.rows)
	if FullyOrdered(sel, s.pk) {
		rep.Ordered++
	} else {
		sort.Strings(pgRows)
		sort.Strings(mgRows)
	}
	for i := range pgRows {
		if pgRows[i] != mgRows[i] {
			return true, mismatch(fmt.Sprintf("row %d differs:\n    postgres: %s\n    maragudb: %s", i, pgRows[i], mgRows[i]))
		}
	}
	return true, nil
}

// rejectionReason is the message of an [maragudb.ErrUnsupported] error, cut before its first
// detail, with quoted text and numbers replaced, so that rejections of one kind count together.
func rejectionReason(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, "unsupported"); ok {
		msg = after
	}
	msg = strings.TrimLeft(msg, ": ")
	msg = strings.TrimPrefix(msg, "order-dependent: ")
	for _, sep := range []string{",", ";", " because"} {
		if i := strings.Index(msg, sep); i >= 0 {
			msg = msg[:i]
		}
	}
	msg = reasonDetail.ReplaceAllString(msg, "_")
	return strings.ReplaceAll(strings.TrimSpace(msg), " ", "_")
}

var reasonDetail = regexp.MustCompile(`"[^"]*"|'[^']*'|\$?\d+|at position _`)

// inKeyOrder runs a statement that failed on Postgres again, with the table's rows stored in
// primary key order so that a scan visits them in that order, and returns the statement's error,
// which is nil if it succeeded. Nothing it does is kept. Inside a transaction, it rolls back to
// the savepoint taken before the statement, reruns it, and aborts the transaction again.
func (s *sides) inKeyOrder(ctx context.Context, text string) (error, error) {
	var keys []string
	for _, k := range s.pk {
		keys = append(keys, ast.RenderExpr(&ast.ColumnRef{Name: k}))
	}
	steps := []string{"BEGIN"}
	if s.inTx {
		steps = []string{"ROLLBACK TO SAVEPOINT pgdiff"}
	}
	steps = append(steps,
		"CREATE TEMP TABLE sorted ON COMMIT DROP AS SELECT * FROM "+tableName,
		"DELETE FROM "+tableName,
		"INSERT INTO "+tableName+" SELECT * FROM sorted ORDER BY "+strings.Join(keys, ", "),
	)
	for _, q := range steps {
		if _, err := s.pg.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("postgres: %s: %w", q, err)
		}
	}
	err := run(ctx, s.pg, text).err
	end := "ROLLBACK"
	if s.inTx {
		end = "ROLLBACK TO SAVEPOINT pgdiff"
	}
	if _, rerr := s.pg.ExecContext(ctx, end); rerr != nil {
		return nil, fmt.Errorf("postgres: %s: %w", end, rerr)
	}
	if s.inTx {
		if res := run(ctx, s.pg, "SELECT 1 / 0"); SQLState(res.err) != "22012" {
			return nil, fmt.Errorf("postgres: aborting the transaction again: %v", res.err)
		}
	}
	return err, nil
}

func describe(r result) string {
	if r.err != nil {
		return "error: " + r.err.Error()
	}
	if r.rows == nil {
		return fmt.Sprintf("%d rows affected", r.affected)
	}
	return fmt.Sprintf("%d rows %q: %s", len(r.rows), r.cols, strings.Join(canonical(r.rows), "; "))
}

// querier is a database, a connection, or a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func run(ctx context.Context, db querier, sqlText string) result {
	if !strings.HasPrefix(sqlText, "SELECT") {
		res, err := db.ExecContext(ctx, sqlText)
		if err != nil {
			return result{err: err}
		}
		n, err := res.RowsAffected()
		return result{affected: n, err: err}
	}
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		return result{err: err}
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return result{err: err}
	}
	out := [][]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return result{err: err}
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return result{err: err}
	}
	return result{cols: cols, rows: out}
}

// canonical renders rows so that equal values render equally: floats by their bits with every
// NaN the same, times as UTC instants.
func canonical(rows [][]any) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		parts := make([]string, len(row))
		for j, v := range row {
			switch v := v.(type) {
			case nil:
				parts[j] = "NULL"
			case float64:
				if math.IsNaN(v) {
					parts[j] = "NaN"
				} else {
					parts[j] = fmt.Sprintf("%v(%x)", v, math.Float64bits(v))
				}
			case []byte:
				parts[j] = fmt.Sprintf("X'%x'", v)
			case time.Time:
				parts[j] = v.UTC().Format(time.RFC3339Nano)
			case string:
				parts[j] = fmt.Sprintf("%q", v)
			default:
				parts[j] = fmt.Sprintf("%T(%v)", v, v)
			}
		}
		out[i] = strings.Join(parts, " ")
	}
	return out
}

func className(err error) string {
	if c := ClassOf(err); c != nil {
		return c.Name
	}
	return "unclassified error"
}

// IsMismatch reports whether err is a mismatch, as opposed to an infrastructure failure.
func IsMismatch(err error) bool {
	var m *Mismatch
	return errors.As(err, &m)
}
