package pgdiff

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

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
	Statements int
	// OK counts statements that succeeded on both sides, and Errors those that failed on both.
	OK, Errors int
	// Checks counts the state checks after mutations.
	Checks int
	// Rows returned by successful SELECTs on maragudb.
	Rows int
	// Ordered counts SELECTs compared in order, as opposed to as multisets.
	Ordered int
	// ErrorClasses counts the maragudb error messages of statements that failed on both sides,
	// by their first words.
	ErrorClasses map[string]int
}

// Add another report's counts.
func (r *Report) Add(o Report) {
	r.Statements += o.Statements
	r.OK += o.OK
	r.Errors += o.Errors
	r.Checks += o.Checks
	r.Rows += o.Rows
	r.Ordered += o.Ordered
	if r.ErrorClasses == nil {
		r.ErrorClasses = map[string]int{}
	}
	for k, v := range o.ErrorClasses {
		r.ErrorClasses[k] += v
	}
}

func (r Report) String() string {
	var classes []string
	for k, v := range r.ErrorClasses {
		classes = append(classes, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(classes)
	return fmt.Sprintf("%d statements: %d ok on both, %d errors on both, %d state checks, %d rows, %d ordered comparisons; errors: %s",
		r.Statements, r.OK, r.Errors, r.Checks, r.Rows, r.Ordered, strings.Join(classes, " "))
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

// Run a case and compare both sides after every statement. The returned error describes the
// first mismatch.
func (r *Runner) Run(ctx context.Context, c Case) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := Report{ErrorClasses: map[string]int{}}
	m, err := r.New()
	if err != nil {
		return rep, err
	}
	defer func() { _ = m.Close() }()
	if _, err := r.PG.ExecContext(ctx, "DROP TABLE IF EXISTS "+tableName); err != nil {
		return rep, fmt.Errorf("postgres: %w", err)
	}
	if _, err := r.PG.ExecContext(ctx, ast.Render(c.Create, ast.Postgres)); err != nil {
		return rep, fmt.Errorf("postgres: %s: %w", ast.Render(c.Create, ast.Postgres), err)
	}
	if _, err := m.ExecContext(ctx, ast.Render(c.Create, ast.Maragudb)); err != nil {
		return rep, fmt.Errorf("maragudb: %s: %w", ast.Render(c.Create, ast.Maragudb), err)
	}
	stateCheck := &ast.Select{Items: []ast.SelectItem{{Star: true}}, From: tableName}
	for _, k := range c.Create.PrimaryKey {
		stateCheck.OrderBy = append(stateCheck.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Name: k}})
	}
	for _, st := range c.Statements {
		rep.Statements++
		if err := r.compare(ctx, m, st, c.Create.PrimaryKey, &rep); err != nil {
			return rep, err
		}
		if _, ok := st.(*ast.Select); !ok {
			var check Report
			if err := r.compare(ctx, m, stateCheck, c.Create.PrimaryKey, &check); err != nil {
				return rep, fmt.Errorf("state after %s: %w", ast.Render(st, ast.Maragudb), err)
			}
			rep.Checks++
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

func (r *Runner) compare(ctx context.Context, m *sql.DB, st ast.Statement, pk []string, rep *Report) error {
	pgSQL := ast.Render(st, ast.Postgres)
	mSQL := ast.Render(st, ast.Maragudb)
	pg := run(ctx, r.PG, pgSQL)
	mg := run(ctx, m, mSQL)
	mismatch := func(detail string) error {
		return &Mismatch{Statement: mSQL, Postgres: pgSQL + describe(pg), Maragudb: mSQL + describe(mg), Detail: detail}
	}
	if (pg.err != nil) != (mg.err != nil) {
		return mismatch("one side failed and the other succeeded")
	}
	if pg.err != nil {
		rep.Errors++
		words := strings.Fields(mg.err.Error())
		if len(words) > 2 {
			words = words[:2]
		}
		rep.ErrorClasses[strings.Join(words, " ")]++
		return nil
	}
	rep.OK++
	sel, isSelect := st.(*ast.Select)
	if !isSelect {
		if pg.affected != mg.affected {
			return mismatch(fmt.Sprintf("rows affected: postgres %d, maragudb %d", pg.affected, mg.affected))
		}
		return nil
	}
	rep.Rows += len(mg.rows)
	if len(pg.cols) != len(mg.cols) {
		return mismatch(fmt.Sprintf("columns: postgres %v, maragudb %v", pg.cols, mg.cols))
	}
	if len(pg.rows) != len(mg.rows) {
		return mismatch(fmt.Sprintf("row count: postgres %d, maragudb %d", len(pg.rows), len(mg.rows)))
	}
	pgRows, mgRows := canonical(pg.rows), canonical(mg.rows)
	if FullyOrdered(sel, pk) {
		rep.Ordered++
	} else {
		sort.Strings(pgRows)
		sort.Strings(mgRows)
	}
	for i := range pgRows {
		if pgRows[i] != mgRows[i] {
			return mismatch(fmt.Sprintf("row %d differs:\n    postgres: %s\n    maragudb: %s", i, pgRows[i], mgRows[i]))
		}
	}
	return nil
}

func describe(r result) string {
	if r.err != nil {
		return "\n    error: " + r.err.Error()
	}
	if r.rows == nil {
		return fmt.Sprintf("\n    %d rows affected", r.affected)
	}
	return fmt.Sprintf("\n    %d rows: %s", len(r.rows), strings.Join(canonical(r.rows), "; "))
}

func run(ctx context.Context, db *sql.DB, sqlText string) result {
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
				parts[j] = fmt.Sprintf("%v", v)
			}
		}
		out[i] = strings.Join(parts, " ")
	}
	return out
}

// IsMismatch reports whether err is a mismatch, as opposed to an infrastructure failure.
func IsMismatch(err error) bool {
	var m *Mismatch
	return errors.As(err, &m)
}
