package exec

import (
	"errors"

	"maragu.dev/maragudb/internal/sql/parser"
)

// Errors a statement can fail with. Each is wrapped with a message about the statement.
var (
	// ErrSyntax is a statement that does not parse.
	ErrSyntax = parser.ErrSyntax
	// ErrInvalid is a statement that cannot run: an unknown column or function, wrong arity, a
	// missing parameter, and the like.
	ErrInvalid = errors.New("invalid statement")
	// ErrType is a type mismatch. There is no implicit coercion.
	ErrType = errors.New("type mismatch")
	// ErrNoTable is a reference to a table that does not exist.
	ErrNoTable = errors.New("no such table")
	// ErrTableExists is a CREATE TABLE of a table that exists.
	ErrTableExists = errors.New("table exists")
	// ErrDuplicateKey is an insert or update that would repeat a primary key.
	ErrDuplicateKey = errors.New("duplicate key value violates primary key")
	// ErrNotNull is a NULL in a NOT NULL column.
	ErrNotNull = errors.New("null value violates not-null constraint")
	// ErrDivisionByZero is a division or modulo by zero.
	ErrDivisionByZero = errors.New("division by zero")
	// ErrOutOfRange is an int overflow, a float overflow or underflow, or a cast out of range.
	ErrOutOfRange = errors.New("value out of range")
	// ErrCast is an invalid cast: between types that cannot be cast, or from a string that does
	// not parse.
	ErrCast = errors.New("invalid cast")
	// ErrReadOnlyTx is a write in a read-only transaction.
	ErrReadOnlyTx = errors.New("cannot write in a read-only transaction")
	// ErrNoTx is a COMMIT or ROLLBACK without a transaction.
	ErrNoTx = errors.New("there is no transaction in progress")
	// ErrTxInProgress is a BEGIN inside a transaction.
	ErrTxInProgress = errors.New("there is already a transaction in progress")
)
