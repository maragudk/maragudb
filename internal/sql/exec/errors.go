package exec

import (
	"errors"

	"maragu.dev/maragudb/internal/sql/parser"
)

// Errors a statement can fail with. Each is wrapped with a message about the statement.
var (
	// ErrSyntax is a statement that does not parse, and that Postgres rejects too.
	ErrSyntax = parser.ErrSyntax
	// ErrInvalid is a statement that cannot run: an unknown column or function, wrong arity, a
	// missing parameter, and the like.
	ErrInvalid = parser.ErrInvalid
	// ErrUnsupported is a statement that Postgres accepts and maragudb rejects, with a message
	// that says what is unsupported and, where possible, what to write instead. It is the only
	// way maragudb is stricter than Postgres.
	ErrUnsupported = parser.ErrUnsupported
	// ErrType is a type mismatch: an operator or function that does not exist for the argument
	// types, or a value of the wrong type where a type is required.
	ErrType = errors.New("type mismatch")
	// ErrNoTable is a reference to a table that does not exist.
	ErrNoTable = errors.New("no such table")
	// ErrTableExists is a CREATE TABLE or CREATE INDEX of a name that a table or index has.
	ErrTableExists = errors.New("relation exists")
	// ErrNoIndex is a DROP INDEX of an index that does not exist.
	ErrNoIndex = errors.New("no such index")
	// ErrWrongObjectType is a statement on a relation of the wrong kind, such as a SELECT from an
	// index or a DROP INDEX of a table.
	ErrWrongObjectType = errors.New("wrong object type")
	// ErrDependentObjects is a DROP INDEX of an index that a constraint of its table needs.
	ErrDependentObjects = errors.New("dependent objects still exist")
	// ErrProgramLimit is a statement beyond a limit of Postgres, such as an index of more than
	// 32 columns.
	ErrProgramLimit = errors.New("program limit exceeded")
	// ErrDuplicateKey is an insert or update that would repeat a primary key or the values of a
	// unique index, or a CREATE UNIQUE INDEX on rows that do.
	ErrDuplicateKey = errors.New("duplicate key value violates unique constraint")
	// ErrCheck is an insert or update of a row for which a CHECK constraint of its table is false.
	ErrCheck = errors.New("new row violates check constraint")
	// ErrDuplicateObject is a constraint whose name another constraint of the table has.
	ErrDuplicateObject = errors.New("duplicate object")
	// ErrForeignKey is an insert or update of a row whose foreign key has no row in the
	// referenced table, or an update or delete of a row that a foreign key with NO ACTION still
	// references.
	ErrForeignKey = errors.New("violates foreign key constraint")
	// ErrRestrict is an update or delete of a row that a foreign key with RESTRICT references.
	ErrRestrict = errors.New("violates RESTRICT setting of foreign key constraint")
	// ErrInvalidForeignKey is a foreign key whose referenced columns are not those of a primary
	// key or a unique index, or whose columns do not match them in number.
	ErrInvalidForeignKey = errors.New("invalid foreign key")
	// ErrNotNull is a NULL in a NOT NULL column.
	ErrNotNull = errors.New("null value violates not-null constraint")
	// ErrGrouping is a grouped SELECT that reads a column outside of its GROUP BY expressions and
	// its aggregates, or an aggregate where none is allowed, such as in WHERE or in the arguments
	// of another aggregate.
	ErrGrouping = errors.New("grouping error")
	// ErrCardinality is a scalar subquery that returns more than one row.
	ErrCardinality = errors.New("more than one row returned by a subquery used as an expression")
	// ErrDivisionByZero is a division or modulo by zero.
	ErrDivisionByZero = errors.New("division by zero")
	// ErrOutOfRange is an integer or bigint overflow, a float overflow or underflow, a cast out
	// of range, or a timestamp out of range.
	ErrOutOfRange = errors.New("value out of range")
	// ErrCast is an invalid cast: between types that cannot be cast, or from text that does not
	// parse as the type.
	ErrCast = errors.New("invalid cast")
	// ErrInvalidUTF8 is a string parameter that is not valid UTF-8 or that holds a NUL byte, which
	// no text in Postgres can hold, whatever type the parameter has.
	ErrInvalidUTF8 = errors.New(`invalid byte sequence for encoding "UTF8"`)
	// ErrReadOnlyTx is a write in a read-only transaction.
	ErrReadOnlyTx = errors.New("cannot write in a read-only transaction")
	// ErrTxAborted is a statement in a transaction in which an earlier statement failed, which
	// Postgres ignores until the transaction is rolled back.
	ErrTxAborted = errors.New("current transaction is aborted, commands ignored until end of transaction block")
	// ErrNoTx is a commit or rollback through the driver without a transaction.
	ErrNoTx = errors.New("there is no transaction in progress")
	// ErrTxInProgress is a BEGIN through the driver inside a transaction.
	ErrTxInProgress = errors.New("there is already a transaction in progress")
)
