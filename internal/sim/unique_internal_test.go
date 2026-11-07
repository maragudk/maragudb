package sim

import (
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/history"
)

func TestUserCheck(t *testing.T) {
	insert := func(invoke, complete int, outcome history.Outcome, rows ...user) userWrite {
		return userWrite{invoke: dur(invoke), complete: dur(complete), outcome: outcome, inserts: rows}
	}
	change := func(invoke, complete int, outcome history.Outcome, email string) userWrite {
		return userWrite{invoke: dur(invoke), complete: dur(complete), outcome: outcome, changes: email}
	}
	alice, nobody := newUser(10, "e01"), newUser(11, nil)
	moved := alice
	moved.email.String, moved.n = "e02", 1

	tests := []struct {
		name   string
		writes []userWrite
		rows   []user
		invoke int
		ok     bool
	}{
		{name: "accepts the rows of acknowledged inserts", writes: []userWrite{insert(0, 10, history.OK, alice, nobody)}, rows: []user{alice, nobody}, invoke: -1, ok: true},
		{name: "rejects a missing row with a NULL email", writes: []userWrite{insert(0, 10, history.OK, alice, nobody)}, rows: []user{alice}, invoke: -1},
		{name: "rejects a missing row whose email no write names", writes: []userWrite{insert(0, 10, history.OK, alice), change(20, 30, history.OK, "e02")}, invoke: -1},
		{name: "rejects a changed row whose email no write names", writes: []userWrite{insert(0, 10, history.OK, alice)}, rows: []user{moved}, invoke: -1},
		{name: "accepts a missing or changed row whose email a later write names", writes: []userWrite{insert(0, 10, history.OK, alice), change(20, 30, history.OK, "e01")}, rows: []user{moved}, invoke: -1, ok: true},
		{name: "accepts a missing row whose email a concurrent write with an unknown outcome names", writes: []userWrite{change(5, 8, history.Unknown, "e01"), insert(0, 10, history.OK, alice)}, invoke: -1, ok: true},
		{name: "rejects a missing row whose email only a failed write names", writes: []userWrite{insert(0, 10, history.OK, alice), change(20, 30, history.Failed, "e01")}, invoke: -1},
		{name: "rejects a missing row whose email only a write acknowledged before the insert began names", writes: []userWrite{change(0, 5, history.OK, "e01"), insert(10, 20, history.OK, alice)}, invoke: -1},
		{name: "accepts a missing row that a transaction moved by its id", writes: []userWrite{insert(0, 10, history.OK, alice), {invoke: dur(20), complete: dur(30), outcome: history.OK, moves: []int64{10}}}, invoke: -1, ok: true},
		{name: "rejects a row of a failed insert", writes: []userWrite{insert(0, 10, history.Failed, alice)}, rows: []user{alice}, invoke: -1},
		{name: "accepts the rows of an insert with an unknown outcome or not", writes: []userWrite{insert(0, 10, history.Unknown, alice, nobody)}, rows: []user{nobody}, invoke: -1, ok: true},
		{name: "rejects a reader that misses an insert acknowledged before it began", writes: []userWrite{insert(0, 10, history.OK, nobody)}, invoke: 20},
		{name: "accepts a reader that misses an insert acknowledged after it began", writes: []userWrite{insert(0, 30, history.OK, nobody)}, invoke: 20, ok: true},
		{name: "rejects a reader that sees a row of a failed insert", writes: []userWrite{insert(0, 30, history.Failed, nobody)}, rows: []user{nobody}, invoke: 20},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newUserCheck(test.writes).check(test.rows, dur(test.invoke))
			if test.ok {
				is.NotError(t, err)
				return
			}
			is.True(t, err != nil, "an error")
		})
	}
}

// dur is n units of a stamp of the history.
func dur(n int) time.Duration {
	return time.Duration(n)
}
