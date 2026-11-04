package exec

import (
	"fmt"
	"math"

	"maragu.dev/maragudb/internal/sql/value"
)

// cast binds CAST(x AS t), with the casts Postgres has among the supported types: between the
// numeric types, where a float becomes an integer by rounding half to even and a numeric literal
// by rounding half away from zero; from every type to text through its output function; and from
// text to every type through its input function, which is also how a quoted literal takes the
// type.
func (b *binder) cast(x *cexpr, t value.Type) (*cexpr, error) {
	from := x.typ
	switch from {
	case value.Unknown:
		return resolve(x, t)
	case value.Numeric:
		return b.numericTo(x, t)
	}
	var f func(v value.Value) (value.Value, error)
	fallible := false
	// The conversions between timestamptz and text are stable in Postgres, because they depend
	// on the session's time zone and date style, so Postgres folds them only for a NULL.
	stable := from == value.Time && t == value.String || from == value.String && t == value.Time
	switch {
	case from == t:
		return x, nil
	case from == value.Int4 && t == value.Int, (from == value.Int4 || from == value.Int) && t == value.Float:
		return widen(x, t), nil
	case from == value.Float && t == value.Int:
		fallible = true
		f = func(v value.Value) (value.Value, error) {
			r := math.RoundToEven(v.F)
			if math.IsNaN(r) || r < -9223372036854775808.0 || r >= 9223372036854775808.0 {
				return value.Value{}, fmt.Errorf("%w: bigint out of range", ErrOutOfRange)
			}
			return value.NewInt(int64(r)), nil
		}
	case from == value.Int4 && t == value.Bool:
		f = func(v value.Value) (value.Value, error) { return value.NewBool(v.I != 0), nil }
	case (from == value.Int4 || from == value.Int) && t == value.Bytes, from == value.Bytes && t == value.Int:
		return nil, fmt.Errorf("%w: casts between bytea and integers are not supported", ErrUnsupported)
	case t == value.String:
		f = func(v value.Value) (value.Value, error) { return value.NewString(value.Format(v)), nil }
	case from == value.String:
		fallible = true
		f = func(v value.Value) (value.Value, error) {
			out, err := value.Parse(v.S, t)
			if err != nil {
				return value.Value{}, castError(err)
			}
			return out, nil
		}
	default:
		return nil, fmt.Errorf("%w: cannot cast type %v to %v", ErrCast, from, t)
	}
	if x.folded && x.val.Null {
		// The conversion is strict.
		return constant(value.NewNull(t)), nil
	}
	return b.fold(&cexpr{typ: t, fallible: fallible, stable: stable, eval: func(row []value.Value) (value.Value, error) {
		v, err := x.value(row)
		if err != nil || v.Null {
			return value.NewNull(t), err
		}
		return f(v)
	}}, x)
}

// assignable checks that an expression can be stored in a column, applying Postgres's
// assignment casts: a quoted literal is read as the column's type, a numeric literal rounds half
// away from zero into a bigint, a float rounds half to even into one, integers widen, and every
// type is written as text into a text column.
func (b *binder) assignable(e *cexpr, t *table, col int) (*cexpr, error) {
	c := t.Columns[col]
	switch {
	case e.typ == c.Type:
		return e, nil
	case e.typ == value.Unknown:
		return resolve(e, c.Type)
	case e.typ == value.Numeric:
		if c.Type == value.Int || c.Type == value.Float || c.Type == value.String {
			return b.numericTo(e, c.Type)
		}
	case (e.typ == value.Int4 || e.typ == value.Int) && (c.Type == value.Int || c.Type == value.Float):
		return widen(e, c.Type), nil
	case e.typ == value.Float && c.Type == value.Int, c.Type == value.String:
		return b.cast(e, c.Type)
	}
	return nil, fmt.Errorf("%w: column %q is of type %v but expression is of type %v", ErrType, c.Name, c.Type, e.typ)
}
