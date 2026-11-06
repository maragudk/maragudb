package pgdiff_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

// constraintTables are the tables of the constraint tests, which every test drops first.
var constraintTables = []string{"cd", "cd2", "cc", "cc2", "cp", "cf", "cf2", "cs"}

// newConstraintSession compares statements on fresh tables named in [constraintTables].
func newConstraintSession(t *testing.T) *pgdiff.Session {
	t.Helper()
	r := newRunner(t)
	m, err := r.New()
	is.NotError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	pgConn, err := r.PG.Conn(t.Context())
	is.NotError(t, err)
	t.Cleanup(func() { _ = pgConn.Close() })
	mConn, err := m.Conn(t.Context())
	is.NotError(t, err)
	t.Cleanup(func() { _ = mConn.Close() })
	for _, table := range constraintTables {
		// CASCADE drops the foreign keys of other tables, on Postgres only.
		_, err := pgConn.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table+" CASCADE")
		is.NotError(t, err)
	}
	return pgdiff.NewSession(pgConn, mConn)
}

func TestDefaults(t *testing.T) {
	t.Run("fills in defaults as Postgres does, with the same errors", func(t *testing.T) {
		verify.Covers(t, "a column's DEFAULT is used for a column that an INSERT leaves out, for DEFAULT in VALUES and in SET, and for DEFAULT VALUES, converted to the column's type by assignment, and a DEFAULT that reads a column, has a parameter, has the wrong type, or fails to fold fails as in Postgres", "INV-PG-SUBSET", "AC-4.3")
		s := newConstraintSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT a)", rejected: true},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT nope)", rejected: true},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT true)", code: "42804"},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT 'x')", code: "22P02"},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT 1 / 0)", rejected: true},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT 2147483647 + 1)", rejected: true},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT $1)", code: "42P02"},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT 1 DEFAULT 2)", code: "42601"},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b bigint DEFAULT 1 IS NULL)", code: "42601"},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b boolean DEFAULT NOT true)", code: "42601"},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b boolean DEFAULT 'a' LIKE 'b')", code: "42601"},
			outcome{sql: "CREATE TABLE cd (a bigint PRIMARY KEY, b boolean DEFAULT 1 < 2 AND true)", code: "42601"},
			outcome{sql: "CREATE TABLE cd (a bigint DEFAULT 7 PRIMARY KEY, b bigint DEFAULT 1.5, c text DEFAULT 5, d boolean DEFAULT ('a' LIKE 'b'), e double precision DEFAULT -2, f bigint NOT NULL DEFAULT NULL, g bytea DEFAULT '\\x01', h boolean CONSTRAINT x DEFAULT 1 < 2, i text DEFAULT CASE WHEN 1 IS NULL THEN 'n' ELSE 'v' END)"},
			outcome{sql: "INSERT INTO cd (f) VALUES (1)"},
			outcome{sql: "SELECT * FROM cd"},
			outcome{sql: "INSERT INTO cd DEFAULT VALUES", code: "23502"},
			outcome{sql: "INSERT INTO cd (a) DEFAULT VALUES", code: "42601"},
			outcome{sql: "INSERT INTO cd VALUES (DEFAULT + 1)", code: "42601"},
			outcome{sql: "SELECT DEFAULT", code: "42601"},
			outcome{sql: "SELECT a FROM cd WHERE a = DEFAULT", code: "42601"},
			outcome{sql: "INSERT INTO cd VALUES (8, DEFAULT, DEFAULT, DEFAULT, DEFAULT, 3), (9, 4, DEFAULT, true, DEFAULT, 4)"},
			outcome{sql: "INSERT INTO cd (f, a) VALUES (5, DEFAULT)", code: "23505"},
			outcome{sql: "INSERT INTO cd (f, a, b) VALUES (5, 10, DEFAULT), (6, 11, DEFAULT)"},
			outcome{sql: "SELECT * FROM cd ORDER BY a"},
			outcome{sql: "UPDATE cd SET b = DEFAULT, c = DEFAULT, d = DEFAULT WHERE a = 9"},
			outcome{sql: "UPDATE cd SET f = DEFAULT", code: "23502"},
			outcome{sql: "UPDATE cd SET nope = DEFAULT", code: "42703"},
			outcome{sql: "UPDATE cd SET b = DEFAULT + 1", code: "42601"},
			outcome{sql: "UPDATE cd SET a = DEFAULT WHERE a = 11", code: "23505"},
			outcome{sql: "UPDATE cd SET a = DEFAULT WHERE a = 10", code: "23505"},
			outcome{sql: "SELECT * FROM cd ORDER BY a"},
			outcome{sql: "CREATE TABLE cd2 (a bigint PRIMARY KEY, b timestamptz DEFAULT CAST(CAST(now() AS text) AS timestamptz))", rejected: true},
			outcome{sql: "CREATE TABLE cd2 (a bigint PRIMARY KEY, b bigint DEFAULT now())", code: "42804"},
			outcome{sql: "CREATE TABLE cd2 (a bigint PRIMARY KEY, b timestamptz DEFAULT now(), c timestamptz DEFAULT CURRENT_TIMESTAMP, d timestamptz DEFAULT transaction_timestamp(), e text DEFAULT now(), f bigint DEFAULT length(CAST(now() AS text)))"},
			outcome{sql: "BEGIN"},
			outcome{sql: "INSERT INTO cd2 (a) VALUES (1), (2)"},
			outcome{sql: "SELECT a, b = now(), c = now(), d = now(), e = CAST(now() AS text), f > 20 FROM cd2 ORDER BY a"},
			outcome{sql: "SELECT now() = now(), now() = CURRENT_TIMESTAMP, transaction_timestamp() = now(), now() > '2000-01-01 00:00:00+00', now() < '3000-01-01 00:00:00+00'"},
			outcome{sql: "UPDATE cd2 SET b = NULL WHERE a = 1"},
			outcome{sql: "UPDATE cd2 SET b = DEFAULT WHERE a = 1"},
			outcome{sql: "SELECT a, b = now() FROM cd2 ORDER BY a"},
			outcome{sql: "COMMIT"},
			outcome{sql: "SELECT a, b <= now(), b = c, c = d FROM cd2 ORDER BY a"},
			outcome{sql: "SELECT now(1)", code: "42883"},
			outcome{sql: "SELECT CURRENT_TIMESTAMP(3)", rejected: true},
			outcome{sql: "SELECT statement_timestamp()", rejected: true},
			outcome{sql: "SELECT clock_timestamp()", rejected: true},
			outcome{sql: "SELECT LOCALTIMESTAMP", rejected: true},
			outcome{sql: "SELECT CURRENT_DATE", rejected: true},
		)
	})
}

func TestChecks(t *testing.T) {
	t.Run("names CHECK constraints as Postgres does, and fails a CHECK as Postgres does when the table is created", func(t *testing.T) {
		verify.Covers(t, "CHECK constraints get Postgres's names, after the one column they read or after the table, numbered past the names of every constraint of every table, they share the names of a table's constraints with its PRIMARY KEY and UNIQUE constraints and its NOT NULL constraints, and a CHECK that is not boolean, reads a missing column, or repeats a name fails with the class of Postgres's error", "INV-PG-SUBSET", "AC-4.3")
		s := newConstraintSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (a))", code: "42804"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (nope > 0))", code: "42703"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (a > $1))", code: "42P02"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (DEFAULT))", code: "42601"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY, CONSTRAINT x CHECK (a > 0), CONSTRAINT x CHECK (a < 9))", code: "42710"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (a > 0) CHECK (a > 1) CONSTRAINT cc_a_check1 CHECK (a > 2))", code: "42710"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY, CONSTRAINT x CHECK (a > 0), CONSTRAINT x UNIQUE (a))", code: "42710"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY, b bigint, CONSTRAINT cc_a_not_null UNIQUE (b))", code: "42710"},
			outcome{sql: "CREATE TABLE cc (a bigint CONSTRAINT x PRIMARY KEY, CONSTRAINT x CHECK (a > 0))", code: "42710"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (1 / 0 > 0))", rejected: true},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (a > 0) NO INHERIT)", rejected: true},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY, CHECK (a > 0) NOT VALID)", rejected: true},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (a > 0) NOT ENFORCED)", rejected: true},
			outcome{sql: "CREATE TABLE cc2 (a bigint PRIMARY KEY, CONSTRAINT cc_pkey CHECK (a > 0), CONSTRAINT cc_b_check CHECK (a > 0))"},
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY CHECK (a > 0) ENFORCED, b bigint CHECK (a < b) CHECK (b > 0), CHECK (true), CHECK (b > 0), CONSTRAINT foo CHECK (b < 100), CHECK (a + b > 0 AND b <> 7), CONSTRAINT cc_b_check3 CHECK (b <> 8))"},
			outcome{sql: "DROP INDEX cc_pkey", code: "42704"},
			outcome{sql: "DROP INDEX cc_pkey1", code: "2BP01"},
			outcome{sql: "INSERT INTO cc VALUES (1, 8)", code: "23514"},
			outcome{sql: "INSERT INTO cc VALUES (1, 7)", code: "23514"},
			outcome{sql: "INSERT INTO cc VALUES (1, 2), (2, NULL)"},
			outcome{sql: "UPDATE cc SET b = 0", code: "23514"},
			outcome{sql: "UPDATE cc SET b = 50 WHERE a = 1"},
			outcome{sql: "UPDATE cc SET a = -a", code: "23514"},
			outcome{sql: "SELECT * FROM cc ORDER BY a"},
			outcome{sql: "DROP TABLE cc2"},
			outcome{sql: "CREATE TABLE cc2 (a bigint PRIMARY KEY CHECK (a > 0))"},
			outcome{sql: "DROP TABLE cc2"},
			outcome{sql: "DROP TABLE cc"},
			outcome{sql: "CREATE TABLE cc2 (a bigint PRIMARY KEY CHECK (a > 0))"},
		)
	})

	t.Run("checks CHECK constraints in the order of their names, after NOT NULL and before the keys, as Postgres does", func(t *testing.T) {
		verify.Covers(t, "an INSERT or UPDATE evaluates the NOT NULL constraints, then the CHECK constraints in the order of their names, failing at the first that is false or fails itself, and then the primary key and the unique indexes, row by row, so it fails with the same class as Postgres, and NULL passes a CHECK", "INV-CHECK", "INV-PG-SUBSET")
		s := newConstraintSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE cc (a bigint PRIMARY KEY, b bigint NOT NULL, s text UNIQUE, CONSTRAINT z CHECK (10 / a > 0), CONSTRAINT y CHECK (b > 0), CONSTRAINT x CHECK (a > 5), CHECK (CAST(s AS bigint) > 0))"},
			outcome{sql: "INSERT INTO cc VALUES (0, 1, '1')", code: "23514"},
			outcome{sql: "INSERT INTO cc VALUES (0, NULL, '1')", code: "23502"},
			outcome{sql: "INSERT INTO cc VALUES (6, -1, '1')", code: "23514"},
			outcome{sql: "INSERT INTO cc VALUES (6, 1, 'x')", code: "22P02"},
			outcome{sql: "INSERT INTO cc VALUES (20, 1, '1')", code: "23514"},
			outcome{sql: "INSERT INTO cc VALUES (6, 1, '1'), (6, 2, '1')", code: "23505"},
			outcome{sql: "INSERT INTO cc VALUES (6, 1, '1'), (7, -2, '1')", code: "23514"},
			outcome{sql: "INSERT INTO cc VALUES (6, 1, '1'), (7, 2, NULL), (8, 3, '3')"},
			outcome{sql: "UPDATE cc SET s = '1' WHERE a = 7", code: "23505"},
			outcome{sql: "UPDATE cc SET s = '0' WHERE a = 7", code: "23514"},
			outcome{sql: "UPDATE cc SET a = a + 10 WHERE a = 8", code: "23514"},
			outcome{sql: "UPDATE cc SET b = b - 1", code: "23514"},
			outcome{sql: "UPDATE cc SET b = b + 1"},
			outcome{sql: "SELECT * FROM cc ORDER BY a"},
			outcome{sql: "CREATE TABLE cc2 (a bigint PRIMARY KEY, t timestamptz DEFAULT now() CHECK (t <= now()))"},
			outcome{sql: "INSERT INTO cc2 VALUES (1, '2100-01-01 00:00:00+00')", code: "23514"},
			outcome{sql: "INSERT INTO cc2 (a) VALUES (2)"},
		)
	})
}
