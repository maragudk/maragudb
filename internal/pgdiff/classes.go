package pgdiff

import (
	"errors"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"

	"maragu.dev/maragudb"
)

// ErrorClass is a maragudb error kind and the Postgres SQLSTATE codes that report the same
// failure.
type ErrorClass struct {
	Name  string
	Err   error
	Codes []string
}

// ErrorClasses maps every exported maragudb statement error to SQLSTATE codes. Where a kind has
// several codes, Postgres splits it more finely: [maragudb.ErrType] covers both a mismatch in a known
// context (42804) and an operator or function with no variant for the argument types (42883),
// [maragudb.ErrCast] covers both a value that does not parse (22P02, 22007, 22008, 22023) and a pair of types that cannot
// be cast (42846), and [maragudb.ErrInvalid] covers every reference that does not resolve.
var ErrorClasses = []ErrorClass{
	{Name: "division by zero", Err: maragudb.ErrDivisionByZero, Codes: []string{"22012"}},
	// 22003 is numeric_value_out_of_range.
	{Name: "out of range", Err: maragudb.ErrOutOfRange, Codes: []string{"22003"}},
	{Name: "not null", Err: maragudb.ErrNotNull, Codes: []string{"23502"}},
	{Name: "duplicate key", Err: maragudb.ErrDuplicateKey, Codes: []string{"23505"}},
	{Name: "syntax", Err: maragudb.ErrSyntax, Codes: []string{"42601"}},
	{Name: "no table", Err: maragudb.ErrNoTable, Codes: []string{"42P01"}},
	{Name: "table exists", Err: maragudb.ErrTableExists, Codes: []string{"42P07"}},
	// 42804 is datatype_mismatch and 42883 is undefined_function, which Postgres reports for an
	// operator without a variant for its operand types.
	{Name: "type", Err: maragudb.ErrType, Codes: []string{"42804", "42883"}},
	// 22P02 is invalid_text_representation, 22007 invalid_datetime_format, 22008
	// datetime_field_overflow, 22023 invalid_parameter_value, which Postgres reports for bytea
	// text in hex format that is not hex, and 42846 cannot_coerce.
	{Name: "cast", Err: maragudb.ErrCast, Codes: []string{"22P02", "22007", "22008", "22023", "42846"}},
	// 42703 is undefined_column, 42883 undefined_function, 42P10 invalid_column_reference, 42701
	// duplicate_column, 42702 ambiguous_column, and 42P16 invalid_table_definition. It also covers
	// arguments that are invalid in themselves, which Postgres classes as data exceptions: 22025
	// invalid_escape_sequence for a LIKE pattern ending in its escape, and 2201W and 2201X for a
	// negative LIMIT or OFFSET.
	{Name: "invalid", Err: maragudb.ErrInvalid, Codes: []string{"42703", "42883", "42P10", "42701", "42702", "42P16", "22025", "2201W", "2201X"}},
	// 25006 is read_only_sql_transaction.
	{Name: "read-only transaction", Err: maragudb.ErrReadOnlyTx, Codes: []string{"25006"}},
	// 25P02 is in_failed_sql_transaction.
	{Name: "transaction aborted", Err: maragudb.ErrTxAborted, Codes: []string{"25P02"}},
}

// ClassOf a maragudb error, or nil when it is none of [ErrorClasses].
func ClassOf(err error) *ErrorClass {
	for i := range ErrorClasses {
		if errors.Is(err, ErrorClasses[i].Err) {
			return &ErrorClasses[i]
		}
	}
	return nil
}

// SQLState of a Postgres error, or "" when it has none.
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// SameClass reports whether a maragudb error and a Postgres error report the same failure.
func SameClass(maragudbErr, postgresErr error) bool {
	c := ClassOf(maragudbErr)
	return c != nil && slices.Contains(c.Codes, SQLState(postgresErr))
}
