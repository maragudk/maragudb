package pgdiff

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/parser"
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
	// Ordered counts SELECTs compared in order, as opposed to as multisets, and
	// SortKeysOrdered those compared as multisets whose sort keys are also compared in order,
	// because ORDER BY sorts by output columns but does not determine the order completely.
	Ordered, SortKeysOrdered int
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
	r.SortKeysOrdered += o.SortKeysOrdered
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
	return fmt.Sprintf("%d statements: %d executed (%d ok on both, %d errors on both, %d of them agreeing in key order), %d rejected; %d transactions, %d state checks, %d rows, %d ordered comparisons, %d comparisons of sort keys in order; errors: %s; rejected for: %s",
		r.Statements, r.Executed, r.OK, r.Errors, r.Reordered, r.Rejected, r.Transactions, r.Checks, r.Rows, r.Ordered, r.SortKeysOrdered, counts(r.ErrorClasses), counts(r.RejectedFor))
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
	// keys maps every table that exists on both sides to its primary key columns.
	keys map[string][]string
	// inTx reports whether an explicit transaction is open on both sides, and readOnly whether
	// it is read-only.
	inTx, readOnly bool
}

// Session compares statements on one connection to each database, which share their state:
// the tables and an explicit transaction.
type Session struct {
	s sides
}

// NewSession on a connection to Postgres and one to maragudb, with no tables and no open
// transaction.
func NewSession(pg, m *sql.Conn) *Session {
	return &Session{s: sides{pg: pg, m: m, keys: map[string][]string{}}}
}

// Outcome of a statement on both sides.
type Outcome struct {
	// Executed reports whether the statement ran on both sides, as opposed to being rejected by
	// maragudb with [maragudb.ErrUnsupported].
	Executed bool
	// Rejection is the reason of the rejection, when it was rejected.
	Rejection string
	// Err is the Postgres error, when the statement failed on both sides.
	Err error
}

// Exec runs the statement on both sides and compares the outcomes, with the same rules as
// [Runner.Run], and adds what it did to the report. It returns a [*Mismatch] error when the
// sides disagree, and another error when a database fails in a way unrelated to the statement.
func (s *Session) Exec(ctx context.Context, text string, rep *Report) (Outcome, error) {
	st, err := parser.Parse(text)
	if err != nil {
		st = nil
	}
	rep.Statements++
	return s.s.compare(ctx, text, st, rep)
}

// InTx reports whether an explicit transaction is open on both sides.
func (s *Session) InTx() bool {
	return s.s.inTx
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
	for _, name := range []string{tableName, parentName} {
		if _, err := r.PG.ExecContext(ctx, "DROP TABLE IF EXISTS "+name+" CASCADE"); err != nil {
			return rep, fmt.Errorf("postgres: %w", err)
		}
	}
	s := &sides{keys: map[string][]string{tableName: c.Create.PrimaryKey}}
	checks := []*ast.Select{stateCheck(c.Create, c.NowColumn)}
	tables := []*ast.CreateTable{c.Create}
	if c.Parent != nil {
		tables = []*ast.CreateTable{c.Parent, c.Create}
		s.keys[parentName] = c.Parent.PrimaryKey
		checks = append(checks, stateCheck(c.Parent, ""))
	}
	for _, t := range tables {
		create := ast.Render(t)
		rep.Statements++
		if _, err := m.ExecContext(ctx, create); errors.Is(err, maragudb.ErrUnsupported) {
			// A table that maragudb rejects, such as one with a CHECK whose constant part fails,
			// ends the case.
			rep.Rejected++
			rep.RejectedFor[rejectionReason(err)]++
			return rep, nil
		} else if err != nil {
			// A table that both databases reject ends the case.
			_, pgErr := r.PG.ExecContext(ctx, create)
			if !SameClass(err, pgErr) {
				return rep, &Mismatch{Statement: create, Postgres: fmt.Sprint(pgErr), Maragudb: err.Error(), Detail: "creating the table"}
			}
			rep.Executed++
			rep.Errors++
			return rep, nil
		}
		if _, err := r.PG.ExecContext(ctx, create); err != nil {
			return rep, &Mismatch{Statement: create, Postgres: err.Error(), Maragudb: "ok", Detail: "creating the table"}
		}
		rep.Executed++
		rep.OK++
	}
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
	for _, st := range c.Statements {
		rep.Statements++
		o, err := s.compare(ctx, ast.Render(st), st, &rep)
		if err != nil {
			return rep, err
		}
		if !o.Executed {
			continue
		}
		switch st.(type) {
		case *ast.Select, *ast.Begin, *ast.Commit, *ast.Rollback:
		default:
			for _, check := range checks {
				var r Report
				if _, err := s.compare(ctx, ast.Render(check), check, &r); err != nil {
					return rep, fmt.Errorf("state after %s: %w", ast.Render(st), err)
				}
				rep.Checks++
			}
		}
	}
	if s.inTx {
		var end Report
		if _, err := s.compare(ctx, "ROLLBACK", &ast.Rollback{}, &end); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// stateCheck selects every row of the table in key order, with the column whose default is now()
// compared with now() instead of as it is.
func stateCheck(t *ast.CreateTable, nowColumn string) *ast.Select {
	sel := &ast.Select{From: t.Name}
	for _, c := range t.Columns {
		var e ast.Expr = &ast.ColumnRef{Name: c.Name}
		if c.Name == nowColumn {
			e = &ast.Binary{Op: ast.Le, Left: e, Right: &ast.Call{Name: "now"}}
		}
		sel.Items = append(sel.Items, ast.SelectItem{Expr: e, Alias: c.Name})
	}
	for _, k := range t.PrimaryKey {
		sel.OrderBy = append(sel.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Name: k}})
	}
	return sel
}

type result struct {
	err      error
	affected int64
	cols     []string
	rows     [][]any
}

// compare runs a statement on both sides and compares the outcomes. The statement is text,
// and st is its parse by maragudb, or nil when maragudb cannot parse it.
func (s *sides) compare(ctx context.Context, text string, st ast.Statement, rep *Report) (Outcome, error) {
	if rep.ErrorClasses == nil {
		rep.ErrorClasses = map[string]int{}
	}
	if rep.RejectedFor == nil {
		rep.RejectedFor = map[string]int{}
	}
	query := isQuery(text, st)
	mg := run(ctx, s.m, text, query)
	if errors.Is(mg.err, maragudb.ErrUnsupported) {
		reason := rejectionReason(mg.err)
		rep.Rejected++
		rep.RejectedFor[reason]++
		if s.inTx {
			if res := run(ctx, s.pg, "SELECT 1 / 0", true); SQLState(res.err) != "22012" && SQLState(res.err) != "25P02" {
				return Outcome{}, fmt.Errorf("postgres: aborting the transaction after a rejection: %v", res.err)
			}
		}
		return Outcome{Rejection: reason}, nil
	}
	rep.Executed++
	executed := Outcome{Executed: true}
	if s.inTx {
		if _, err := s.pg.ExecContext(ctx, "SAVEPOINT pgdiff"); err != nil && SQLState(err) != "25P02" {
			return executed, fmt.Errorf("postgres: %w", err)
		}
	}
	pg := run(ctx, s.pg, text, query)
	mismatch := func(detail string) error {
		return &Mismatch{Statement: text, Postgres: describe(pg), Maragudb: describe(mg), Detail: detail}
	}
	if (pg.err != nil) != (mg.err != nil) {
		return executed, mismatch("one side failed and the other succeeded")
	}
	if pg.err != nil {
		rep.Errors++
		executed.Err = pg.err
		if !SameClass(mg.err, pg.err) {
			if s.readOnly && !query || SQLState(pg.err) == "25P02" {
				// The writes in a read-only transaction fail before they read rows, and a statement
				// in an aborted transaction fails before it reads rows too.
				return executed, mismatch(fmt.Sprintf("both failed, for different reasons: postgres SQLSTATE %s, maragudb %s", SQLState(pg.err), className(mg.err)))
			}
			// Different rows can fail for different reasons, and maragudb visits rows in key order
			// where Postgres visits them in the order they are stored. Postgres gets the same
			// chance in key order before this counts as a mismatch.
			table := tableOf(st)
			if s.keys[table] == nil {
				return executed, mismatch(fmt.Sprintf("both failed, for different reasons: postgres SQLSTATE %s, maragudb %s", SQLState(pg.err), className(mg.err)))
			}
			ordered, err := s.inKeyOrder(ctx, text, table, query)
			if err != nil {
				return executed, err
			}
			if !SameClass(mg.err, ordered) {
				return executed, mismatch(fmt.Sprintf("both failed, for different reasons: postgres SQLSTATE %s, maragudb %s", SQLState(pg.err), className(mg.err)))
			}
			rep.Reordered++
			pg.err = ordered
		}
		rep.ErrorClasses[ClassOf(mg.err).Name+"/"+SQLState(pg.err)]++
		return executed, nil
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
	case *ast.CreateTable:
		// CREATE TABLE IF NOT EXISTS of a table that exists changes nothing. The keys of a
		// dropped table, or of one created in a transaction that rolls back, stay, so that a
		// DROP TABLE that rolls back keeps them; they are only read for a table that a statement
		// uses successfully, which then exists.
		if _, ok := s.keys[st.Name]; !ok || !st.IfNotExists {
			s.keys[st.Name] = st.PrimaryKey
		}
	}
	sel, isSelect := st.(*ast.Select)
	if !isSelect {
		if pg.affected != mg.affected {
			return executed, mismatch(fmt.Sprintf("rows affected: postgres %d, maragudb %d", pg.affected, mg.affected))
		}
		return executed, nil
	}
	rep.Rows += len(mg.rows)
	if strings.Join(pg.cols, ",") != strings.Join(mg.cols, ",") {
		return executed, mismatch(fmt.Sprintf("columns: postgres %q, maragudb %q", pg.cols, mg.cols))
	}
	if len(pg.rows) != len(mg.rows) {
		return executed, mismatch(fmt.Sprintf("row count: postgres %d, maragudb %d", len(pg.rows), len(mg.rows)))
	}
	pgRows, mgRows := canonical(pg.rows), canonical(mg.rows)
	// A table whose primary key is unknown is never fully ordered, but a SELECT without one has
	// one row.
	pk, known := s.keys[sel.From]
	if (known || sel.From == "") && FullyOrdered(sel, pk) {
		rep.Ordered++
	} else {
		if keys := sortedColumns(sel, mg.cols); len(keys) > 0 {
			rep.SortKeysOrdered++
			pgKeys, mgKeys := sortKeys(pg.rows, keys), sortKeys(mg.rows, keys)
			for i := range pgKeys {
				if pgKeys[i] != mgKeys[i] {
					return executed, mismatch(fmt.Sprintf("the sort keys of row %d differ:\n    postgres: %s\n    maragudb: %s", i, pgKeys[i], mgKeys[i]))
				}
			}
		}
		sort.Strings(pgRows)
		sort.Strings(mgRows)
	}
	for i := range pgRows {
		if pgRows[i] != mgRows[i] {
			return executed, mismatch(fmt.Sprintf("row %d differs:\n    postgres: %s\n    maragudb: %s", i, pgRows[i], mgRows[i]))
		}
	}
	return executed, nil
}

// isQuery reports whether a statement returns rows: when maragudb parses it as a SELECT, or
// when it cannot parse it and it starts like a query.
func isQuery(text string, st ast.Statement) bool {
	if st != nil {
		_, ok := st.(*ast.Select)
		return ok
	}
	head := strings.ToUpper(strings.TrimLeft(text, " \t\r\n("))
	for _, prefix := range []string{"SELECT", "VALUES", "WITH", "TABLE"} {
		if strings.HasPrefix(head, prefix) {
			return true
		}
	}
	return false
}

// tableOf the statement, the table it reads or writes, or "" when it has none.
func tableOf(st ast.Statement) string {
	switch st := st.(type) {
	case *ast.Select:
		return st.From
	case *ast.Insert:
		return st.Table
	case *ast.Update:
		return st.Table
	case *ast.Delete:
		return st.Table
	}
	return ""
}

// rejectionReason is the message of an [maragudb.ErrUnsupported] error, cut before its first
// detail, with quoted text and numbers replaced, so that rejections of one kind count together.
func rejectionReason(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, "unsupported"); ok {
		msg = after
	}
	msg = strings.TrimLeft(msg, ": ")
	msg = reasonPosition.ReplaceAllString(msg, "")
	msg = reasonCondition.ReplaceAllString(msg, "whether _ fails depends on the plan")
	msg = strings.TrimPrefix(msg, "order-dependent: ")
	for _, sep := range []string{",", ";", " because"} {
		if i := strings.Index(msg, sep); i >= 0 {
			msg = msg[:i]
		}
	}
	msg = reasonDetail.ReplaceAllString(msg, "_")
	return strings.ReplaceAll(strings.TrimSpace(msg), " ", "_")
}

var (
	reasonPosition = regexp.MustCompile(`^at position \d+: `)
	// reasonCondition is the condition in a rejection of UNS-PLAN-DEPENDENT-WHERE.
	reasonCondition = regexp.MustCompile(`whether .+? fails depends on the plan`)
	reasonDetail    = regexp.MustCompile(`"[^"]*"|'[^']*'|\$?\d+`)
)

// inKeyOrder runs a statement that failed on Postgres again, with the rows of its table stored
// in primary key order and its other indexes marked invalid, so that the planner scans the
// table or its primary key and visits the rows in that order, and returns the statement's error,
// which is nil if it succeeded. An invalid index is still kept up to date and still enforces
// uniqueness; the planner only does not use it. A scan of the primary key is one of the plans
// Postgres can pick, and maragudb fails a statement for certain only on a row that every plan
// reads. Nothing it does is kept. Inside a transaction, it rolls
// back to the savepoint taken before the statement, reruns it, and aborts the transaction
// again. A read-only transaction cannot store the rows again, but it has written nothing
// either, so its rows are the committed ones: the rerun ends it, runs in a transaction of its
// own, and then begins a transaction that it aborts, which behaves like the aborted read-only
// one until it ends.
func (s *sides) inKeyOrder(ctx context.Context, text, table string, query bool) (error, error) {
	inTx := s.inTx && !s.readOnly
	steps := []string{"BEGIN"}
	switch {
	case inTx:
		steps = []string{"ROLLBACK TO SAVEPOINT pgdiff"}
	case s.inTx:
		steps = []string{"ROLLBACK", "BEGIN"}
	}
	for _, q := range steps {
		if _, err := s.pg.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("postgres: %s: %w", q, err)
		}
	}
	// The rows of the statement's table, and of every other table, which foreign key actions
	// visit, are stored again in key order, with the referential triggers off, so that the rows
	// stay as they are.
	steps = []string{"SET LOCAL session_replication_role = replica"}
	tables := slices.Sorted(maps.Keys(s.keys))
	for i, name := range tables {
		var exists bool
		if err := s.pg.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r' AND relpersistence = 'p')", name).Scan(&exists); err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		if !exists {
			continue
		}
		var keys []string
		for _, k := range s.keys[name] {
			keys = append(keys, ast.RenderExpr(&ast.ColumnRef{Name: k}))
		}
		tableName := ast.RenderExpr(&ast.ColumnRef{Name: name})
		sorted := fmt.Sprint("sorted", i)
		steps = append(steps,
			"CREATE TEMP TABLE "+sorted+" ON COMMIT DROP AS SELECT * FROM "+tableName,
			"DELETE FROM "+tableName,
			"INSERT INTO "+tableName+" SELECT * FROM "+sorted+" ORDER BY "+strings.Join(keys, ", "),
		)
		if name == table {
			steps = append(steps, "UPDATE pg_index SET indisvalid = false WHERE indrelid = '"+strings.ReplaceAll(tableName, "'", "''")+"'::regclass AND NOT indisprimary")
		}
	}
	steps = append(steps, "SET LOCAL session_replication_role = origin")
	for _, q := range steps {
		if _, err := s.pg.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("postgres: %s: %w", q, err)
		}
	}
	err := run(ctx, s.pg, text, query).err
	end := []string{"ROLLBACK"}
	switch {
	case inTx:
		end = []string{"ROLLBACK TO SAVEPOINT pgdiff"}
	case s.inTx:
		end = []string{"ROLLBACK", "BEGIN"}
	}
	for _, q := range end {
		if _, rerr := s.pg.ExecContext(ctx, q); rerr != nil {
			return nil, fmt.Errorf("postgres: %s: %w", q, rerr)
		}
	}
	if s.inTx {
		if res := run(ctx, s.pg, "SELECT 1 / 0", true); SQLState(res.err) != "22012" {
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

// run a statement, as a query when it returns rows.
func run(ctx context.Context, db querier, sqlText string, query bool) result {
	if !query {
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
			parts[j] = canonicalValue(v)
		}
		out[i] = strings.Join(parts, " ")
	}
	return out
}

func canonicalValue(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case float64:
		if math.IsNaN(v) {
			return "NaN"
		}
		return fmt.Sprintf("%v(%x)", v, math.Float64bits(v))
	case []byte:
		return fmt.Sprintf("X'%x'", v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case string:
		return fmt.Sprintf("%q", v)
	default:
		return fmt.Sprintf("%T(%v)", v, v)
	}
}

// sortKeys renders the given columns of every row, with the values that sort as equal
// rendered equally, which only -0 and 0 do apart from equal values.
func sortKeys(rows [][]any, columns []int) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		parts := make([]string, len(columns))
		for j, c := range columns {
			v := row[c]
			if f, ok := v.(float64); ok && f == 0 {
				v = 0.0
			}
			parts[j] = canonicalValue(v)
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
