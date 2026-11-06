package pgdiff_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

// constraintTables are the tables of the constraint tests, which every test drops first.
var constraintTables = []string{"cd", "cd2", "cc", "cc2", "cp", "cf", "cf2", "cg", "ch", "cq", "cr", "cs", "cs2"}

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

func TestForeignKeys(t *testing.T) {
	t.Run("creates and drops foreign keys as Postgres does, with the same names and errors", func(t *testing.T) {
		verify.Covers(t, "FOREIGN KEY and REFERENCES get Postgres's names, reference a primary key or the first unique index on the referenced columns, fail with the class of Postgres's error for a missing table or column, columns without a unique index, a different number of columns, and incompatible types, and DROP TABLE and DROP INDEX of what a foreign key depends on fail with 2BP01", "INV-PG-SUBSET", "AC-4.3")
		s := newConstraintSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE cp (id bigint PRIMARY KEY, u bigint UNIQUE, v bigint, w text, x double precision UNIQUE)"},
			outcome{sql: "CREATE UNIQUE INDEX cp_vw ON cp (v, w)"},
			outcome{sql: "CREATE UNIQUE INDEX cp_u2 ON cp (u)"},
			outcome{sql: "CREATE INDEX cp_v ON cp (v)"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES nope)", code: "42P01"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp_vw)", code: "42809"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, FOREIGN KEY (nope) REFERENCES cp)", code: "42703"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp (nope))", code: "42703"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp (v))", code: "42830"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint, c bigint, FOREIGN KEY (b, c) REFERENCES cp (u, u))", code: "42830"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint, c bigint, FOREIGN KEY (b, c) REFERENCES cp (id))", code: "42830"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b text REFERENCES cp)", code: "42804"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b double precision REFERENCES cp)", code: "42804"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp (x))", rejected: true},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b double precision REFERENCES cp (x))", rejected: true},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp ON DELETE SET NULL (a))", code: "42P10"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp ON DELETE SET NULL (nope))", code: "42703"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp ON DELETE CASCADE ON DELETE CASCADE)", code: "42601"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp ON UPDATE SET NULL (b))", rejected: true},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp MATCH PARTIAL)", rejected: true},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint, CONSTRAINT foo CHECK (a > 0), CONSTRAINT foo FOREIGN KEY (nope) REFERENCES nope)", code: "42710"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint CONSTRAINT cf_a_not_null REFERENCES cp)", code: "42710"},
			outcome{sql: "CREATE TABLE cf (a bigint PRIMARY KEY, b bigint REFERENCES cp, c bigint REFERENCES cp (u) ON DELETE CASCADE, w text, FOREIGN KEY (w, b) REFERENCES cp (w, v) ON DELETE SET NULL (b), FOREIGN KEY (b) REFERENCES cp ON DELETE RESTRICT ON UPDATE RESTRICT, CONSTRAINT cf_b_fkey2 CHECK (b <> 7))"},
			outcome{sql: "DROP TABLE cp", code: "2BP01"},
			outcome{sql: "DROP TABLE IF EXISTS cp", code: "2BP01"},
			outcome{sql: "DROP INDEX cp_vw", code: "2BP01"},
			outcome{sql: "DROP INDEX cp_u2"},
			outcome{sql: "DROP INDEX cp_u_key", code: "2BP01"},
			outcome{sql: "DROP INDEX cp_v"},
			outcome{sql: "CREATE TABLE cf2 (a bigint PRIMARY KEY, b bigint, c bigint, FOREIGN KEY (b) REFERENCES cf2, FOREIGN KEY (c) REFERENCES cf ON DELETE SET DEFAULT, CONSTRAINT cf_b_fkey FOREIGN KEY (b) REFERENCES cp)"},
			outcome{sql: "DROP TABLE cf", code: "2BP01"},
			outcome{sql: "DROP TABLE cf2"},
			outcome{sql: "CREATE TABLE cf2 (a bigint PRIMARY KEY, b bigint REFERENCES cf2 ON DELETE CASCADE, CHECK (a > 0), c bigint, d bigint REFERENCES cf2 (c))", code: "42830"},
			outcome{sql: "CREATE TABLE cf2 (a bigint PRIMARY KEY, b bigint REFERENCES cf2 ON DELETE CASCADE, c bigint UNIQUE, d bigint REFERENCES cf2 (c))"},
			outcome{sql: "DROP TABLE cf2"},
			outcome{sql: "DROP TABLE cf"},
			outcome{sql: "DROP TABLE cp"},
			outcome{sql: "CREATE TABLE cp (id bigint PRIMARY KEY)"},
		)
	})

	t.Run("checks foreign keys and fires their actions as Postgres does", func(t *testing.T) {
		verify.Covers(t, "an insert or update of a row whose foreign key has no NULL and no parent row fails with 23503 at the end of the statement, a delete or key update of a referenced row fails with 23503 for NO ACTION and 23001 for RESTRICT, or deletes, sets to NULL, or sets to the defaults the referencing rows, with the rows the same on both afterwards", "INV-FK", "INV-PG-SUBSET")
		s := newConstraintSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE cp (id bigint PRIMARY KEY, u bigint UNIQUE, w text, UNIQUE (w, u))"},
			outcome{sql: "INSERT INTO cp VALUES (1, 10, 'a'), (2, 20, 'b'), (3, 30, 'c'), (4, 40, NULL)"},
			outcome{sql: "CREATE TABLE cf (id bigint PRIMARY KEY, a bigint REFERENCES cp ON DELETE CASCADE, w text, CHECK (w <> 'x'))"},
			outcome{sql: "CREATE TABLE cr (id bigint PRIMARY KEY, d bigint REFERENCES cp ON DELETE RESTRICT)"},
			outcome{sql: "CREATE TABLE cg (id bigint PRIMARY KEY, b bigint REFERENCES cp (u) ON DELETE SET NULL ON UPDATE RESTRICT, w text)"},
			outcome{sql: "CREATE TABLE ch (id bigint PRIMARY KEY, c bigint NOT NULL DEFAULT 1 REFERENCES cp ON DELETE SET DEFAULT)"},
			outcome{sql: "CREATE TABLE cq (id bigint PRIMARY KEY, w text, u bigint, FOREIGN KEY (w, u) REFERENCES cp (w, u) ON DELETE SET NULL (w))"},
			outcome{sql: "CREATE INDEX ch_c ON ch (c)"},
			outcome{sql: "INSERT INTO cf VALUES (1, 1, 'a'), (2, 9, NULL)", code: "23503"},
			outcome{sql: "INSERT INTO cf VALUES (1, 1, 'a'), (3, 2, 'b'), (4, 2, NULL), (2, NULL, NULL)"},
			outcome{sql: "INSERT INTO cr VALUES (3, 3), (4, 4)"},
			outcome{sql: "INSERT INTO cg VALUES (1, 99, NULL)", code: "23503"},
			outcome{sql: "INSERT INTO cg VALUES (1, 10, 'a'), (2, NULL, 'zz'), (3, 20, NULL), (4, 40, NULL)"},
			outcome{sql: "INSERT INTO ch VALUES (1, 2), (2, 3), (3, 1)"},
			outcome{sql: "INSERT INTO cq VALUES (1, 'b', 20), (2, 'c', 30), (3, 'a', 20)", code: "23503"},
			outcome{sql: "INSERT INTO cq VALUES (1, 'b', 20), (2, 'c', 30), (3, NULL, 20), (4, 'zz', NULL)"},
			outcome{sql: "INSERT INTO cf VALUES (5, 1, NULL), (5, 9, NULL)", code: "23505"},
			outcome{sql: "UPDATE cf SET a = 9 WHERE id = 5"},
			outcome{sql: "UPDATE cf SET a = 9 WHERE id = 1", code: "23503"},
			outcome{sql: "UPDATE cr SET d = 9 WHERE id = 3", code: "23503"},
			outcome{sql: "UPDATE cf SET a = 3 WHERE id = 1"},
			outcome{sql: "UPDATE cf SET w = 'x' WHERE id = 1", code: "23514"},
			outcome{sql: "SELECT * FROM cf ORDER BY id"},
			outcome{sql: "DELETE FROM cp WHERE id = 3", code: "23001"},
			outcome{sql: "UPDATE cp SET u = 11 WHERE id = 1", code: "23001"},
			outcome{sql: "UPDATE cp SET w = 'z' WHERE id = 2", code: "23503"},
			outcome{sql: "UPDATE cp SET id = 12 WHERE id = 2", code: "23503"},
			outcome{sql: "UPDATE cp SET id = 13 WHERE id = 4", code: "23503"},
			outcome{sql: "UPDATE cp SET u = 41 WHERE id = 4", code: "23001"},
			outcome{sql: "UPDATE cp SET w = 'q' WHERE id = 4"},
			outcome{sql: "BEGIN"},
			outcome{sql: "UPDATE cr SET d = NULL"},
			outcome{sql: "DELETE FROM cp WHERE id = 2"},
			outcome{sql: "SELECT * FROM cf ORDER BY id"},
			outcome{sql: "SELECT * FROM cg ORDER BY id"},
			outcome{sql: "SELECT * FROM ch ORDER BY id"},
			outcome{sql: "SELECT * FROM cq ORDER BY id"},
			outcome{sql: "DELETE FROM cp WHERE id = 1", code: "23503"},
			outcome{sql: "ROLLBACK"},
			outcome{sql: "BEGIN"},
			outcome{sql: "DELETE FROM cr WHERE id = 3"},
			outcome{sql: "DELETE FROM cp WHERE id = 3"},
			outcome{sql: "SELECT * FROM ch ORDER BY id"},
			outcome{sql: "SELECT * FROM cq ORDER BY id"},
			outcome{sql: "INSERT INTO cf VALUES (6, 3, NULL)", code: "23503"},
			outcome{sql: "ROLLBACK"},
			outcome{sql: "BEGIN"},
			outcome{sql: "INSERT INTO cp VALUES (5, 50, 'e')"},
			outcome{sql: "INSERT INTO cf VALUES (6, 5, 'e')"},
			outcome{sql: "INSERT INTO cr VALUES (6, 5)"},
			outcome{sql: "COMMIT"},
			outcome{sql: "DELETE FROM cp WHERE id = 5", code: "23001"},
			outcome{sql: "DELETE FROM cr WHERE id = 6"},
			outcome{sql: "DELETE FROM cp WHERE id = 5"},
			outcome{sql: "SELECT * FROM cf ORDER BY id"},
			outcome{sql: "SELECT * FROM cp ORDER BY id"},
		)
	})

	t.Run("checks a self-referencing table at the end of the statement, as Postgres does", func(t *testing.T) {
		verify.Covers(t, "a self-referencing table is checked at the end of each statement, so an UPDATE that moves parents and children together passes, one that moves only the parents fails, a cycle of references can be inserted in one statement, and deleting a whole tree passes where deleting part of it fails", "INV-FK", "INV-PG-SUBSET")
		s := newConstraintSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE cs (id bigint PRIMARY KEY, pid bigint REFERENCES cs, n bigint)"},
			outcome{sql: "INSERT INTO cs VALUES (1, NULL, 0), (2, 1, 0), (3, 2, 0)"},
			outcome{sql: "UPDATE cs SET id = id + 10, pid = pid + 10"},
			outcome{sql: "SELECT * FROM cs ORDER BY id"},
			outcome{sql: "UPDATE cs SET id = id + 10", code: "23503"},
			outcome{sql: "UPDATE cs SET pid = 13 WHERE id = 11"},
			outcome{sql: "UPDATE cs SET pid = NULL WHERE id = 11"},
			outcome{sql: "INSERT INTO cs VALUES (5, 6, 0), (6, 5, 0)"},
			outcome{sql: "DELETE FROM cs WHERE id = 11", code: "23503"},
			outcome{sql: "DELETE FROM cs WHERE id IN (11, 12)", code: "23503"},
			outcome{sql: "DELETE FROM cs WHERE id IN (11, 12, 13)"},
			outcome{sql: "UPDATE cs SET id = CASE WHEN id = 5 THEN 7 ELSE id END, pid = CASE WHEN id = 6 THEN 7 ELSE pid END"},
			outcome{sql: "SELECT * FROM cs ORDER BY id"},
			outcome{sql: "UPDATE cs SET id = CASE WHEN id = 7 THEN 8 ELSE id END, n = 1 WHERE id IN (6, 7)", code: "23503"},
			outcome{sql: "DELETE FROM cs"},
			outcome{sql: "CREATE TABLE cs2 (id bigint PRIMARY KEY, pid bigint REFERENCES cs2 ON UPDATE RESTRICT)"},
			outcome{sql: "INSERT INTO cs2 VALUES (1, NULL), (2, 1)"},
			outcome{sql: "UPDATE cs2 SET id = 10 WHERE id = 1", code: "23001"},
			outcome{sql: "BEGIN"},
			outcome{sql: "INSERT INTO cs2 VALUES (3, NULL), (4, 3)"},
			outcome{sql: "UPDATE cs2 SET id = id + 10 WHERE id IN (3, 4)", code: "23001"},
			outcome{sql: "ROLLBACK"},
			outcome{sql: "BEGIN"},
			outcome{sql: "INSERT INTO cs2 VALUES (4, NULL), (3, 4)"},
			outcome{sql: "UPDATE cs2 SET id = id + 10 WHERE id IN (3, 4)", code: "23001"},
			outcome{sql: "ROLLBACK"},
			outcome{sql: "DROP TABLE cs2"},
		)
	})

	t.Run("cascades through chains of tables and trees, and rejects actions whose outcome depends on their order", func(t *testing.T) {
		verify.Covers(t, "ON DELETE CASCADE, SET NULL, and SET DEFAULT go through chains of tables and down self-referencing trees as in Postgres, the rows they change are checked like any other, and a DELETE whose actions could give another outcome in another order is rejected", "INV-FK", "INV-PG-SUBSET", "UNS-FOREIGN-KEY-ORDER")
		s := newConstraintSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE cp (id bigint PRIMARY KEY)"},
			outcome{sql: "CREATE TABLE cf (id bigint PRIMARY KEY, pid bigint NOT NULL REFERENCES cp ON DELETE CASCADE)"},
			outcome{sql: "CREATE TABLE cf2 (id bigint PRIMARY KEY, fid bigint REFERENCES cf ON DELETE CASCADE, n bigint)"},
			outcome{sql: "CREATE TABLE ch (id bigint PRIMARY KEY, pid bigint DEFAULT 1 REFERENCES cp ON DELETE SET DEFAULT, CHECK (pid > 0))"},
			outcome{sql: "CREATE TABLE cs (id bigint PRIMARY KEY, pid bigint REFERENCES cs ON DELETE CASCADE, p bigint REFERENCES cp ON DELETE SET NULL, CHECK (p IS NOT NULL OR id > 100))"},
			outcome{sql: "INSERT INTO cp VALUES (1), (2), (3)"},
			outcome{sql: "INSERT INTO cf VALUES (1, 1), (2, 2), (3, 2)"},
			outcome{sql: "INSERT INTO cf2 VALUES (1, 1, 0), (2, 3, 0), (3, NULL, 0), (4, 2, 0)"},
			outcome{sql: "INSERT INTO ch VALUES (1, 2), (2, 3), (3, 3), (4, 1)"},
			outcome{sql: "INSERT INTO cs VALUES (1, NULL, 1), (2, 1, 1), (3, 2, 1), (4, 1, 1), (101, NULL, 2), (5, 101, 3)"},
			outcome{sql: "BEGIN"},
			outcome{sql: "DELETE FROM cp WHERE id = 2"},
			outcome{sql: "SELECT * FROM cf ORDER BY id"},
			outcome{sql: "SELECT * FROM cf2 ORDER BY id"},
			outcome{sql: "SELECT * FROM ch ORDER BY id"},
			outcome{sql: "SELECT * FROM cs ORDER BY id"},
			outcome{sql: "ROLLBACK"},
			outcome{sql: "DELETE FROM cp WHERE id = 1", code: "23503"},
			outcome{sql: "DELETE FROM cp WHERE id = 3", code: "23514"},
			outcome{sql: "DELETE FROM cs WHERE id = 1"},
			outcome{sql: "SELECT * FROM cs ORDER BY id"},
			outcome{sql: "CREATE TABLE cd (id bigint PRIMARY KEY, x bigint REFERENCES cp, y bigint REFERENCES cp ON DELETE CASCADE)"},
			outcome{sql: "DELETE FROM cp WHERE id = 3", rejected: true},
			outcome{sql: "DROP TABLE cd"},
			outcome{sql: "CREATE TABLE cd (id bigint PRIMARY KEY, x bigint REFERENCES cf ON DELETE SET NULL, y bigint REFERENCES cp ON DELETE CASCADE)"},
			outcome{sql: "DELETE FROM cp WHERE id = 3", rejected: true},
			outcome{sql: "DELETE FROM cf WHERE id = 3"},
		)
	})
}
