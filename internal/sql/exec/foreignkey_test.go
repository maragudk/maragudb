package exec_test

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/verify"
)

// errorKind of a statement error, the sentinel it wraps, for comparing two sessions.
func errorKind(err error) string {
	if err == nil {
		return "ok"
	}
	for _, e := range []error{exec.ErrUnsupported, exec.ErrForeignKey, exec.ErrRestrict, exec.ErrCheck, exec.ErrNotNull, exec.ErrDuplicateKey, exec.ErrDivisionByZero, exec.ErrOutOfRange} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return err.Error()
}

// checkStore runs [exec.CheckConstraints] and [exec.CheckIndexes] on the committed state.
func checkStore(t *testing.T, db *engine.DB) error {
	t.Helper()
	tx, err := db.BeginReadOnly(t.Context())
	is.NotError(t, err)
	defer func() { _ = tx.Rollback() }()
	if err := exec.CheckConstraints(t.Context(), tx); err != nil {
		return err
	}
	return exec.CheckIndexes(t.Context(), tx)
}

func TestSession_ForeignKeysKeepInvariants(t *testing.T) {
	t.Run("random writes on referenced, referencing, and self-referencing tables keep every foreign key and check, with and without an index on the referencing columns", func(t *testing.T) {
		verify.Covers(t, "random inserts, updates, and deletes, in and out of transactions, on a parent, a child with a random ON DELETE action, and a self-referencing table never leave a row whose foreign key has no parent or a row that violates a CHECK, and a session whose child has an index on its referencing columns gives the same outcomes and rows as one without", "INV-FK", "INV-CHECK", "INV-INDEX-CONSISTENT")
		actions := []string{"NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"}
		outcomes := map[string]int{}
		for i, childAction := range actions {
			selfAction := actions[(i+2)%len(actions)]
			hegel.Test(t, func(ht *hegel.T) {
				schema := []string{
					"CREATE TABLE p (id bigint PRIMARY KEY, n bigint NOT NULL DEFAULT 0 CHECK (n >= 0))",
					"CREATE TABLE c (id bigint PRIMARY KEY, pid bigint DEFAULT 0 REFERENCES p ON DELETE " + childAction + ", n bigint CHECK (n < 5))",
					"CREATE TABLE s (id bigint PRIMARY KEY, pid bigint DEFAULT 1 REFERENCES s ON DELETE " + selfAction + ", p bigint REFERENCES p, n bigint DEFAULT 0, CHECK (pid <> id))",
					"INSERT INTO p VALUES (0, 0), (1, 1), (2, 2), (3, 3)",
					"INSERT INTO c VALUES (1, 1, 1), (2, 1, 2), (3, 2, NULL), (4, NULL, 0)",
					"INSERT INTO s VALUES (1, NULL, 1), (2, 1, 2), (3, 2, NULL), (4, 1, 3)",
				}
				type side struct {
					s  *exec.Session
					db *engine.DB
				}
				var sides []side
				for i := range 2 {
					s, db := newWorld(t).session(exec.Options{})
					for _, q := range schema {
						mustExec(t, s, q)
					}
					if i == 1 {
						mustExec(t, s, "CREATE INDEX c_pid ON c (pid DESC, n)")
						mustExec(t, s, "CREATE INDEX s_pid ON s (pid)")
					}
					sides = append(sides, side{s, db})
				}
				small := func() string {
					if hegel.Draw(ht, hegel.Integers(0, 5)) == 0 {
						return "NULL"
					}
					return fmt.Sprint(hegel.Draw(ht, hegel.Integers(0, 4)))
				}
				inTx := false
				for range hegel.Draw(ht, hegel.Integers(1, 15)) {
					var sql string
					table := hegel.Draw(ht, hegel.SampledFrom([]string{"p", "c", "s"}))
					switch hegel.Draw(ht, hegel.Integers(0, 6)) {
					case 0, 1:
						var rows []string
						for range hegel.Draw(ht, hegel.Integers(1, 3)) {
							switch table {
							case "p":
								rows = append(rows, fmt.Sprintf("(%s, %s)", small(), small()))
							default:
								rows = append(rows, fmt.Sprintf("(%s, %s, %s)", small(), small(), small()))
							}
						}
						sql = "INSERT INTO " + table + " VALUES " + strings.Join(rows, ", ")
					case 2:
						col := "pid"
						if table == "p" || hegel.Draw(ht, hegel.Booleans()) {
							col = hegel.Draw(ht, hegel.SampledFrom([]string{"id", "n"}))
						}
						val := small()
						if hegel.Draw(ht, hegel.Integers(0, 3)) == 0 {
							val = "DEFAULT"
						}
						sql = fmt.Sprintf("UPDATE %s SET %s = %s WHERE id = %s", table, col, val, small())
					case 3:
						sql = fmt.Sprintf("UPDATE %s SET n = n + 1 WHERE id IN (%s, %s)", table, small(), small())
					case 4, 5:
						sql = fmt.Sprintf("DELETE FROM %s WHERE id IN (%s, %s)", table, small(), small())
					default:
						switch {
						case inTx && hegel.Draw(ht, hegel.Booleans()):
							sql = "ROLLBACK"
						case inTx:
							sql = "COMMIT"
						default:
							sql = "BEGIN"
						}
						inTx = sql == "BEGIN"
					}
					var kinds []string
					for _, sd := range sides {
						_, _, err := run(t.Context(), sd.s, sql)
						kinds = append(kinds, errorKind(err))
					}
					outcomes[kinds[0]]++
					if kinds[0] != kinds[1] {
						ht.Fatalf("%s: without the index %s, with it %s", sql, kinds[0], kinds[1])
					}
					if inTx {
						continue
					}
					var states []string
					for _, sd := range sides {
						var state []string
						for _, table := range []string{"p", "c", "s"} {
							state = append(state, query(t, sd.s, "SELECT * FROM "+table+" ORDER BY id"))
						}
						states = append(states, strings.Join(state, "\n--\n"))
						if err := checkStore(t, sd.db); err != nil {
							ht.Fatalf("after %s: %v", sql, err)
						}
					}
					if states[0] != states[1] {
						ht.Fatalf("after %s: without the index\n%s\nwith it\n%s", sql, states[0], states[1])
					}
				}
			}, hegel.WithTestCases(200))
		}
		t.Log(outcomes)
		// The cases must reach the violations and the actions, or passing means little.
		for _, kind := range []string{"ok", exec.ErrForeignKey.Error(), exec.ErrCheck.Error(), exec.ErrRestrict.Error()} {
			is.True(t, outcomes[kind] > 20, kind)
		}
	})
}

func TestSession_ForeignKeyCascades(t *testing.T) {
	t.Run("cascades down a chain of rows far deeper than the stack would allow for a recursion per row", func(t *testing.T) {
		verify.Covers(t, "an ON DELETE CASCADE down a self-referencing chain of 10,000 rows deletes the whole chain with the stack limited to 1 MiB, so a cascade never recurses once per row", "INV-FK")
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE n (id bigint PRIMARY KEY, pid bigint REFERENCES n ON DELETE CASCADE)")
		mustExec(t, s, "CREATE INDEX n_pid ON n (pid)")
		var values strings.Builder
		values.WriteString("(1, NULL)")
		for i := 2; i <= 10_000; i++ {
			fmt.Fprintf(&values, ", (%d, %d)", i, i-1)
		}
		mustExec(t, s, "INSERT INTO n VALUES "+values.String())
		// Running out of stack is fatal and cannot be recovered, so the limit makes the test
		// binary fail when the cascade recurses.
		defer debug.SetMaxStack(debug.SetMaxStack(1 << 20))
		res := mustExec(t, s, "DELETE FROM n WHERE id = 2")
		is.Equal(t, int64(1), res.RowsAffected)
		is.Equal(t, "1 NULL", query(t, s, "SELECT * FROM n"))
	})
}
