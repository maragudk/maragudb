package exec_test

import (
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/verify"
)

func TestSession_Defaults(t *testing.T) {
	t.Run("now() is the start of the transaction, the same for every use in it, from the engine's clock at microsecond precision", func(t *testing.T) {
		verify.Covers(t, "now(), CURRENT_TIMESTAMP, and transaction_timestamp() are the start of the transaction on the engine's clock, truncated to microseconds, the same in every statement of a transaction and in a DEFAULT, and later in a later transaction", "INV-PG-SUBSET")
		w := newWorld(t)
		s, _ := w.session(exec.Options{})
		w.clock.Sleep(t.Context(), 1500*time.Nanosecond)
		mustExec(t, s, "CREATE TABLE d (id bigint PRIMARY KEY, ts timestamptz DEFAULT now(), s text DEFAULT CURRENT_TIMESTAMP)")
		mustExec(t, s, "BEGIN")
		start := query(t, s, "SELECT now()")
		is.Equal(t, "2026-01-01 00:00:00.000001+00", start, "the clock of the engine, truncated to microseconds")
		w.clock.Sleep(t.Context(), time.Second)
		is.Equal(t, start, query(t, s, "SELECT CURRENT_TIMESTAMP"))
		is.Equal(t, start, query(t, s, "SELECT transaction_timestamp()"))
		mustExec(t, s, "INSERT INTO d (id) VALUES (1), (2)")
		w.clock.Sleep(t.Context(), time.Second)
		mustExec(t, s, "INSERT INTO d VALUES (3, DEFAULT, DEFAULT)")
		is.Equal(t, "1 true \"2026-01-01 00:00:00.000001+00\"\n2 true \"2026-01-01 00:00:00.000001+00\"\n3 true \"2026-01-01 00:00:00.000001+00\"", query(t, s, "SELECT id, ts = now(), s FROM d ORDER BY id"))
		mustExec(t, s, "COMMIT")

		is.Equal(t, "2026-01-01 00:00:02.000001+00", query(t, s, "SELECT now()"), "a later transaction starts later")
		mustExec(t, s, "INSERT INTO d (id) VALUES (4)")
		mustExec(t, s, "UPDATE d SET ts = DEFAULT WHERE id = 1")
		is.Equal(t, "1 2026-01-01 00:00:02.000001+00\n2 2026-01-01 00:00:00.000001+00\n3 2026-01-01 00:00:00.000001+00\n4 2026-01-01 00:00:02.000001+00", query(t, s, "SELECT id, ts FROM d ORDER BY id"))
	})

	t.Run("an INSERT without a column list fills the first columns and gives the others their defaults", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE d (id bigint PRIMARY KEY, a bigint DEFAULT 5, b text DEFAULT 'x' || 'y', c boolean)")
		mustExec(t, s, "INSERT INTO d VALUES (1), (2)")
		mustExec(t, s, "INSERT INTO d VALUES (3, DEFAULT, 'z')")
		is.Equal(t, `1 5 "xy" NULL
2 5 "xy" NULL
3 5 "z" NULL`, query(t, s, "SELECT * FROM d ORDER BY id"))
		is.Error(t, exec.ErrNotNull, queryErr(t, s, "INSERT INTO d DEFAULT VALUES"), "the default of the primary key is NULL")
		mustExec(t, s, "CREATE TABLE d2 (id bigint PRIMARY KEY DEFAULT 7, a bigint DEFAULT 5)")
		mustExec(t, s, "INSERT INTO d2 DEFAULT VALUES")
		is.Equal(t, "7 5", query(t, s, "SELECT * FROM d2"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "INSERT INTO d VALUES (4, 5, 'a', TRUE, 6)"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "INSERT INTO d (id, a) VALUES (4)"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "INSERT INTO d VALUES (4), (5, 6)"))
	})
}
