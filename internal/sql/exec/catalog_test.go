package exec_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/verify"
)

// catalogSchema has the parts of a table definition that the SQL layer follows: a primary key,
// a unique index, a secondary index, a DEFAULT, a CHECK, and a foreign key with an action.
var catalogSchema = []string{
	"CREATE TABLE p (id bigint PRIMARY KEY, name text UNIQUE, n bigint DEFAULT 7 CHECK (n > 0))",
	"CREATE TABLE c (id bigint PRIMARY KEY, pid bigint REFERENCES p ON DELETE CASCADE, v text)",
	"CREATE INDEX c_pid ON c (pid)",
	"INSERT INTO p VALUES (1, 'a', 2), (2, 'b', 3)",
	"INSERT INTO c VALUES (10, 1, 'x'), (11, 2, 'y')",
}

// catalogStatements read and write through every part of the schema's definitions.
var catalogStatements = []string{
	"SELECT * FROM p",
	"SELECT * FROM p WHERE name = 'a'",
	"SELECT name FROM p ORDER BY name",
	"SELECT * FROM c WHERE pid = 1",
	"SELECT p.name, c.v FROM p JOIN c ON c.pid = p.id ORDER BY c.v",
	"SELECT count(*) FROM c WHERE pid IN (SELECT id FROM p WHERE n > 1)",
	"INSERT INTO p (id, name) VALUES (3, 'c')",
	"INSERT INTO c VALUES (12, 1, 'z')",
	"UPDATE p SET n = n + 1 WHERE id = 1",
	"UPDATE c SET pid = 2 WHERE id = 10",
	"DELETE FROM p WHERE id = 2",
	"DELETE FROM c WHERE v = 'x'",
	"CREATE INDEX ON c (v)",
	"DROP TABLE c",
}

// catalogStore holds the objects of a database with [catalogSchema], made once.
func catalogStore(t testing.TB) map[string][]byte {
	t.Helper()
	clock := s3fake.NewVirtualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	fake := s3fake.New(s3fake.WithClock(clock))
	db, err := engine.Open(t.Context(), engine.Options{Client: fake, Prefix: "db", Now: clock.Now})
	is.NotError(t, err)
	s := exec.NewSession(db, exec.Options{})
	for _, q := range catalogSchema {
		_, _, err := run(t.Context(), s, q)
		is.NotError(t, err, q)
	}
	objects := map[string][]byte{}
	for _, key := range fake.Keys() {
		objects[key], _ = fake.Body(key)
	}
	return objects
}

// withDefinition runs the statements on a copy of the objects in which the catalog entry of the
// relation name is def, each in a session of its own, and then the checks of indexes and
// constraints. It returns the errors.
func withDefinition(t *testing.T, objects map[string][]byte, name string, def []byte) []error {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	clock := s3fake.NewVirtualClock(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	fake := s3fake.New(s3fake.WithClock(clock))
	for key, body := range objects {
		if _, err := fake.Put(ctx, key, body, s3.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := engine.Open(ctx, engine.Options{Client: fake, Prefix: "db", Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Put([]byte("c/"+name), def); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var errs []error
	for _, q := range catalogStatements {
		_, _, err := run(ctx, exec.NewSession(db, exec.Options{}), q)
		errs = append(errs, err)
	}
	ro, err := db.BeginReadOnly(ctx)
	if err != nil {
		t.Fatal(err)
	}
	errs = append(errs, exec.CheckIndexes(ctx, ro), exec.CheckConstraints(ctx, ro))
	return errs
}

func TestCatalog(t *testing.T) {
	objects := catalogStore(t)

	t.Run("a table definition that does not hold up fails every statement that reads it with ErrCorrupt, and never panics", func(t *testing.T) {
		verify.Covers(t, "a table definition that does not decode, names another table, has an index without columns, or a column reference, foreign key, or type out of range fails the statements that read it with ErrCorrupt, and never panics", "INV-DAMAGE-DETECTED")
		for _, test := range []struct {
			name, from, to string
		}{
			{name: "an index without columns", from: `"columns":[{"column":1}]`, to: `"columns":[]`},
			{name: "an index of a column out of range", from: `"columns":[{"column":1}]`, to: `"columns":[{"column":9}]`},
			{name: "a primary key column out of range", from: `"primaryKey":[0]`, to: `"primaryKey":[-1]`},
			{name: "no primary key", from: `"primaryKey":[0]`, to: `"primaryKey":[]`},
			{name: "a column of no type", from: `"type":1`, to: `"type":0`},
			{name: "another table's name", from: `"name":"p"`, to: `"name":"q"`},
			{name: "a foreign key that sets a column outside its key", from: `"refColumns":[0]`, to: `"refColumns":[0],"setColumns":[0]`},
			{name: "a broken encoding", from: `{`, to: `[`},
		} {
			t.Run(test.name, func(t *testing.T) {
				name := "p"
				def, _ := json.Marshal(map[string]any{})
				for _, n := range []string{"p", "c"} {
					b := catalogEntry(t, objects, n)
					if bytes.Contains(b, []byte(test.from)) {
						name, def = n, bytes.Replace(b, []byte(test.from), []byte(test.to), 1)
						break
					}
				}
				is.True(t, !bytes.Equal(def, []byte("{}")), "the schema has "+test.from)
				errs := withDefinition(t, objects, name, def)
				corrupt := 0
				for _, err := range errs {
					if errors.Is(err, engine.ErrCorrupt) {
						corrupt++
					}
				}
				is.True(t, corrupt > 0, "some statement fails with ErrCorrupt")
			})
		}
	})

	t.Run("a foreign key that does not fit the table it references fails with ErrCorrupt when it is followed", func(t *testing.T) {
		for _, test := range []struct {
			name, from, to string
		}{
			{name: "a referenced column out of range", from: `"refColumns":[0]`, to: `"refColumns":[5]`},
			{name: "a referenced column of another type", from: `"refColumns":[0]`, to: `"refColumns":[1]`},
			{name: "an index that the parent does not have", from: `"index":"p_pkey"`, to: `"index":"p_other"`},
			{name: "a parent that is another table", from: `"parent":"p"`, to: `"parent":"c"`},
		} {
			t.Run(test.name, func(t *testing.T) {
				b := catalogEntry(t, objects, "c")
				is.True(t, bytes.Contains(b, []byte(test.from)), string(b))
				errs := withDefinition(t, objects, "c", bytes.Replace(b, []byte(test.from), []byte(test.to), 1))
				corrupt := 0
				for _, err := range errs {
					if errors.Is(err, engine.ErrCorrupt) {
						corrupt++
					}
				}
				is.True(t, corrupt > 0, "some statement fails with ErrCorrupt")
			})
		}
	})
}

// catalogEntry of the relation name among the objects, as a fresh process reads it.
func catalogEntry(t testing.TB, objects map[string][]byte, name string) []byte {
	t.Helper()
	clock := s3fake.NewVirtualClock(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	fake := s3fake.New(s3fake.WithClock(clock))
	for key, body := range objects {
		_, err := fake.Put(t.Context(), key, body, s3.PutOptions{})
		is.NotError(t, err)
	}
	db, err := engine.Open(t.Context(), engine.Options{Client: fake, Prefix: "db", Now: clock.Now})
	is.NotError(t, err)
	tx, err := db.BeginReadOnly(t.Context())
	is.NotError(t, err)
	b, ok, err := tx.Get(t.Context(), []byte("c/"+name))
	is.NotError(t, err)
	is.True(t, ok)
	return b
}

// FuzzCatalog runs statements that read and write through every part of a table definition on a
// database whose definition of a table is arbitrary bytes, which never panics: a definition that
// does not hold up gives an error.
func FuzzCatalog(f *testing.F) {
	objects := catalogStore(f)
	f.Add(false, catalogEntry(f, objects, "p"))
	f.Add(true, catalogEntry(f, objects, "c"))
	f.Add(false, []byte(`{"name":"p","columns":[{"name":"id","type":1,"notNull":true}],"primaryKey":[0],"indexes":[{"name":"x","columns":[]}]}`))
	f.Add(true, []byte(`{"name":"c","columns":[{"name":"id","type":1},{"name":"pid","type":4}],"primaryKey":[0],"foreignKeys":[{"name":"f","columns":[1],"parent":"p","refColumns":[0],"index":"p_pkey","onDelete":2}]}`))
	f.Add(false, []byte(`{"name":"p","indexOf":"c"}`))
	f.Add(true, []byte(`not json`))
	f.Fuzz(func(t *testing.T, child bool, def []byte) {
		name := "p"
		if child {
			name = "c"
		}
		for _, err := range withDefinition(t, objects, name, def) {
			if err != nil && strings.Contains(err.Error(), "panic") {
				t.Fatal(err)
			}
		}
	})
}
