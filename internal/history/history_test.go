package history_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func app(key string, v int64) history.Op {
	return history.Op{Kind: history.Append, Key: key, Value: v}
}

func read(key string, list ...int64) history.Op {
	if list == nil {
		list = []int64{}
	}
	return history.Op{Kind: history.Read, Key: key, List: list}
}

func tx(id int, invoke, complete int, outcome history.Outcome, ops ...history.Op) history.Tx {
	return history.Tx{ID: id, Client: id, Invoke: time.Duration(invoke), Complete: time.Duration(complete), Ops: ops, Outcome: outcome}
}

func anomalyKind(t *testing.T, err error) string {
	t.Helper()
	var a *history.Anomaly
	if !errors.As(err, &a) {
		return ""
	}
	return a.Kind
}

func TestCheckListAppend(t *testing.T) {
	verify.Covers(t, "the list-append checker accepts serializable histories and rejects stale reads, write skew, lost updates, reads of failed writes, duplicate values, and cycles through real-time edges", "AC-1.8", "INV-STRICT-SERIALIZABLE")
	tests := []struct {
		name  string
		txs   []history.Tx
		final map[string][]int64
		want  string
	}{
		{
			name: "accepts a serial history",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(2, 20, 30, history.OK, read("k", 1), app("k", 2)),
				tx(3, 40, 50, history.OK, read("k", 1, 2)),
			},
			final: map[string][]int64{"k": {1, 2}},
		},
		{
			name: "accepts concurrent transactions in either order",
			txs: []history.Tx{
				tx(1, 0, 30, history.OK, app("k", 1)),
				tx(2, 10, 20, history.OK, read("k"), app("j", 2)),
			},
			final: map[string][]int64{"k": {1}, "j": {2}},
		},
		{
			name: "rejects a stale read after a completed write",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(2, 20, 30, history.OK, read("k")),
			},
			final: map[string][]int64{"k": {1}},
			want:  "cycle",
		},
		{
			name: "rejects a read that misses an earlier element of a key read by a later transaction",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(2, 11, 20, history.OK, app("k", 2)),
				tx(3, 30, 40, history.OK, read("k", 2)),
			},
			final: map[string][]int64{"k": {1, 2}},
			want:  "incompatible orders",
		},
		{
			name: "rejects write skew",
			txs: []history.Tx{
				tx(1, 0, 20, history.OK, read("a"), app("b", 1)),
				tx(2, 0, 20, history.OK, read("b"), app("a", 2)),
			},
			final: map[string][]int64{"a": {2}, "b": {1}},
			want:  "cycle",
		},
		{
			name: "rejects a lost update seen as incompatible orders",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(2, 0, 10, history.OK, app("k", 2)),
				tx(3, 20, 30, history.OK, read("k", 1)),
				tx(4, 20, 30, history.OK, read("k", 2)),
			},
			final: map[string][]int64{"k": {2}},
			want:  "incompatible orders",
		},
		{
			name: "rejects a read of a failed transaction's write",
			txs: []history.Tx{
				tx(1, 0, 10, history.Failed, app("k", 1)),
				tx(2, 20, 30, history.OK, read("k", 1)),
			},
			final: map[string][]int64{"k": {1}},
			want:  "read of failed write",
		},
		{
			name: "rejects a value nobody appended",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, read("k", 99)),
			},
			final: map[string][]int64{"k": {99}},
			want:  "unknown value",
		},
		{
			name: "treats an observed unknown transaction as committed",
			txs: []history.Tx{
				tx(1, 0, 10, history.Unknown, app("k", 1)),
				tx(2, 20, 30, history.OK, read("k", 1)),
			},
			final: map[string][]int64{"k": {1}},
		},
		{
			name: "an unknown transaction has no completion so it may be ordered late",
			txs: []history.Tx{
				tx(1, 0, 10, history.Unknown, app("k", 1)),
				tx(2, 20, 30, history.OK, read("k")),
			},
			final: map[string][]int64{"k": {1}},
		},
		{
			name: "ignores an unobserved unknown transaction",
			txs: []history.Tx{
				tx(1, 0, 10, history.Unknown, app("k", 1)),
				tx(2, 20, 30, history.OK, read("k")),
			},
			final: map[string][]int64{},
		},
		{
			name: "reads see own appends",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1), read("k", 1), app("k", 2), read("k", 1, 2)),
				tx(2, 20, 30, history.OK, read("k", 1, 2)),
			},
			final: map[string][]int64{"k": {1, 2}},
		},
		{
			name: "rejects a read that does not show own appends",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1), read("k")),
			},
			final: map[string][]int64{"k": {1}},
			want:  "internal inconsistency",
		},
		{
			name: "rejects a duplicated value",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
			},
			final: map[string][]int64{"k": {1, 1}},
			want:  "duplicate value",
		},
		{
			name: "rejects an observed value that is missing from the final state",
			txs: []history.Tx{
				tx(1, 0, 10, history.Unknown, app("k", 1)),
				tx(2, 20, 30, history.OK, read("k", 1)),
			},
			final: map[string][]int64{"k": {}},
			want:  "observed value missing from final state",
		},
		{
			name: "rejects a transaction that completed before it was invoked",
			txs: []history.Tx{
				tx(1, 10, 5, history.OK, app("k", 1)),
			},
			final: map[string][]int64{"k": {1}},
			want:  "invalid history",
		},
		{
			name: "rejects a value appended by two transactions",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(2, 20, 30, history.Failed, app("k", 1)),
			},
			final: map[string][]int64{"k": {1}},
			want:  "invalid history",
		},
		{
			name: "rejects a reused transaction ID",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(1, 20, 30, history.OK, app("k", 2)),
			},
			final: map[string][]int64{"k": {1, 2}},
			want:  "invalid history",
		},
		{
			name: "rejects a stale read by a transaction that later failed",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(2, 20, 30, history.Failed, read("k"), app("j", 2)),
			},
			final: map[string][]int64{"k": {1}},
			want:  "cycle",
		},
		{
			name: "accepts a failed transaction whose read was current",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, app("k", 1)),
				tx(2, 20, 30, history.Failed, read("k", 1), app("k", 2)),
			},
			final: map[string][]int64{"k": {1}},
		},
		{
			name: "rejects a three-transaction cycle through write-read and real-time edges",
			txs: []history.Tx{
				tx(1, 0, 10, history.OK, read("k"), app("j", 1)),
				tx(2, 20, 30, history.OK, read("j", 1), app("k", 2)),
				tx(3, 40, 50, history.OK, read("k", 2), read("j")),
			},
			final: map[string][]int64{"k": {2}, "j": {1}},
			want:  "cycle",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := history.CheckListAppend(test.txs, test.final)
			if test.want == "" {
				is.NotError(t, err)
				return
			}
			is.Equal(t, test.want, anomalyKind(t, err), fmt.Sprint(err))
		})
	}
}

func TestCheckDurability(t *testing.T) {
	verify.Covers(t, "the durability checker rejects a missing acknowledged write, an applied failed write, a partially applied unknown transaction, and values in the wrong key or never written", "AC-3.1", "AC-3.2")
	tests := []struct {
		name  string
		txs   []history.Tx
		final map[string][]int64
		want  string
	}{
		{
			name:  "accepts acknowledged writes in the final state",
			txs:   []history.Tx{tx(1, 0, 10, history.OK, app("k", 1), app("j", 2))},
			final: map[string][]int64{"k": {1}, "j": {2}},
		},
		{
			name:  "rejects a missing acknowledged write",
			txs:   []history.Tx{tx(1, 0, 10, history.OK, app("k", 1), app("j", 2))},
			final: map[string][]int64{"k": {1}},
			want:  "lost acknowledged write",
		},
		{
			name:  "rejects a failed write that was applied",
			txs:   []history.Tx{tx(1, 0, 10, history.Failed, app("k", 1))},
			final: map[string][]int64{"k": {1}},
			want:  "failed write applied",
		},
		{
			name:  "rejects a partially applied unknown transaction",
			txs:   []history.Tx{tx(1, 0, 10, history.Unknown, app("k", 1), app("j", 2))},
			final: map[string][]int64{"k": {1}},
			want:  "partial write",
		},
		{
			name:  "accepts an unknown transaction that was fully applied or not at all",
			txs:   []history.Tx{tx(1, 0, 10, history.Unknown, app("k", 1), app("j", 2)), tx(2, 0, 10, history.Unknown, app("k", 3))},
			final: map[string][]int64{"k": {1}, "j": {2}},
		},
		{
			name:  "rejects a value in the wrong key",
			txs:   []history.Tx{tx(1, 0, 10, history.OK, app("k", 1))},
			final: map[string][]int64{"j": {1}},
			want:  "misplaced value",
		},
		{
			name:  "rejects a value nobody wrote",
			txs:   []history.Tx{tx(1, 0, 10, history.OK, app("k", 1))},
			final: map[string][]int64{"k": {1, 7}},
			want:  "unknown value",
		},
		{
			name:  "rejects a read of a failed write even when it is gone from the final state",
			txs:   []history.Tx{tx(1, 0, 10, history.Failed, app("k", 1)), tx(2, 0, 10, history.OK, read("k", 1))},
			final: map[string][]int64{},
			want:  "read of failed write",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := history.CheckDurability(test.txs, test.final)
			if test.want == "" {
				is.NotError(t, err)
				return
			}
			is.Equal(t, test.want, anomalyKind(t, err), fmt.Sprint(err))
		})
	}
}

func TestCheckBank(t *testing.T) {
	verify.Covers(t, "the bank checker accepts audits that sum to the total and rejects a wrong total and a negative balance", "AC-1.7")
	t.Run("accepts audits that sum to the total", func(t *testing.T) {
		is.NotError(t, history.CheckBank([]history.BankAudit{{TxID: 1, Balances: map[string]int64{"a": 60, "b": 40}}}, 100))
	})

	t.Run("rejects a wrong total", func(t *testing.T) {
		err := history.CheckBank([]history.BankAudit{{TxID: 1, Balances: map[string]int64{"a": 60, "b": 50}}}, 100)
		is.Equal(t, "money not conserved", anomalyKind(t, err))
	})

	t.Run("rejects a negative balance", func(t *testing.T) {
		err := history.CheckBank([]history.BankAudit{{TxID: 1, Balances: map[string]int64{"a": 110, "b": -10}}}, 100)
		is.Equal(t, "negative balance", anomalyKind(t, err))
	})
}

func transfer(id int, outcome history.Outcome, from, to string, amount int64) history.Tx {
	return tx(id, id*10, id*10+5, outcome, app(from, -amount), app(to, amount))
}

func TestCheckBankDurability(t *testing.T) {
	verify.Covers(t, "the bank durability checker accepts final balances that the acknowledged transfers and a set of the unknown ones explain, and rejects a lost acknowledged transfer and an applied failed one", "AC-3.1", "AC-3.2")
	tests := []struct {
		name  string
		txs   []history.Tx
		final map[string]int64
		want  string
	}{
		{
			name:  "accepts balances that the acknowledged transfers explain",
			txs:   []history.Tx{transfer(1, history.OK, "a", "b", 10), transfer(2, history.OK, "b", "c", 30)},
			final: map[string]int64{"a": 90, "b": 80, "c": 130},
		},
		{
			name:  "accepts balances without the transfers that failed or have an unknown outcome",
			txs:   []history.Tx{transfer(1, history.Failed, "a", "b", 10), transfer(2, history.Unknown, "b", "c", 30)},
			final: map[string]int64{"a": 100, "b": 100, "c": 100},
		},
		{
			name: "accepts balances with some of the transfers that have an unknown outcome",
			txs: []history.Tx{
				transfer(1, history.Unknown, "a", "b", 10), transfer(2, history.Unknown, "b", "c", 30),
				transfer(3, history.OK, "c", "a", 5), transfer(4, history.Unknown, "a", "c", 7),
			},
			final: map[string]int64{"a": 98, "b": 70, "c": 132},
		},
		{
			name:  "accepts a transfer that read nothing, such as an audit",
			txs:   []history.Tx{tx(1, 0, 5, history.OK), transfer(2, history.OK, "a", "b", 10)},
			final: map[string]int64{"a": 90, "b": 110},
		},
		{
			name:  "rejects balances without an acknowledged transfer",
			txs:   []history.Tx{transfer(1, history.OK, "a", "b", 10), transfer(2, history.OK, "b", "c", 30)},
			final: map[string]int64{"a": 90, "b": 110, "c": 100},
			want:  "lost acknowledged write",
		},
		{
			name:  "rejects balances with half of an acknowledged transfer, which conserve no money",
			txs:   []history.Tx{transfer(1, history.OK, "a", "b", 10)},
			final: map[string]int64{"a": 90, "b": 100},
			want:  "lost acknowledged write",
		},
		{
			name:  "rejects balances with a failed transfer",
			txs:   []history.Tx{transfer(1, history.Failed, "a", "b", 10)},
			final: map[string]int64{"a": 90, "b": 110},
			want:  "lost acknowledged write",
		},
		{
			name:  "rejects balances that only a transfer with an unknown outcome applied twice explains",
			txs:   []history.Tx{transfer(1, history.Unknown, "a", "b", 10)},
			final: map[string]int64{"a": 80, "b": 120},
			want:  "lost acknowledged write",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := history.CheckBankDurability(test.txs, 100, test.final)
			if test.want == "" {
				is.NotError(t, err)
				return
			}
			is.Equal(t, test.want, anomalyKind(t, err), fmt.Sprint(err))
		})
	}

	t.Run("decides many transfers with an unknown outcome that move the same amounts", func(t *testing.T) {
		var txs []history.Tx
		for id := 1; id <= 60; id++ {
			txs = append(txs, transfer(id, history.Unknown, "a", "b", 1))
		}
		txs = append(txs, transfer(61, history.OK, "b", "c", 2))
		is.NotError(t, history.CheckBankDurability(txs, 100, map[string]int64{"a": 63, "b": 135, "c": 102}))
		err := history.CheckBankDurability(txs, 100, map[string]int64{"a": 63, "b": 137, "c": 100})
		is.Equal(t, "lost acknowledged write", anomalyKind(t, err), fmt.Sprint(err))
	})

	t.Run("reports that it cannot decide too many combinations of transfers with an unknown outcome", func(t *testing.T) {
		var txs []history.Tx
		for id := 1; id <= 44; id++ {
			txs = append(txs, transfer(id, history.Unknown, "a", "b", int64(id)))
		}
		err := history.CheckBankDurability(txs, 100, map[string]int64{"a": 100, "b": 100})
		is.True(t, errors.Is(err, history.ErrUndecided), fmt.Sprint(err))
		is.Equal(t, "", anomalyKind(t, err))
	})

	t.Run("accepts every serial history of transfers, and rejects it without one of its acknowledged transfers", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			accounts := []string{"a", "b", "c", "d"}
			balances := map[string]int64{}
			for _, a := range accounts {
				balances[a] = 100
			}
			// Without unknown outcomes, no other set of transfers can make up for a lost one.
			outcomes := []history.Outcome{history.OK, history.OK, history.Failed}
			withUnknown := hegel.Draw(ht, hegel.Booleans())
			if withUnknown {
				outcomes = append(outcomes, history.Unknown)
			}
			var txs, acked []history.Tx
			n := hegel.Draw(ht, hegel.Integers(1, 30))
			for id := 1; id <= n; id++ {
				outcome := hegel.Draw(ht, hegel.SampledFrom(outcomes))
				from := hegel.Draw(ht, hegel.SampledFrom(accounts))
				to := hegel.Draw(ht, hegel.SampledFrom(accounts))
				amount := int64(hegel.Draw(ht, hegel.Integers(1, 50)))
				if outcome == history.OK || outcome == history.Unknown && hegel.Draw(ht, hegel.Booleans()) {
					balances[from] -= amount
					balances[to] += amount
				}
				txs = append(txs, transfer(id, outcome, from, to, amount))
				if outcome == history.OK && from != to {
					acked = append(acked, txs[len(txs)-1])
				}
			}
			if err := history.CheckBankDurability(txs, 100, balances); err != nil {
				ht.Fatalf("rejected a serial history: %v", err)
			}
			if withUnknown || len(acked) == 0 {
				return
			}
			lost := hegel.Draw(ht, hegel.SampledFrom(acked))
			for _, op := range lost.Ops {
				balances[op.Key] -= op.Value
			}
			if err := history.CheckBankDurability(txs, 100, balances); err == nil {
				ht.Fatalf("accepted balances without %v", lost)
			}
		}, hegel.WithTestCases(500))
	})
}

func TestCheckBankAudits(t *testing.T) {
	verify.Covers(t, "the bank audit checker accepts audits that the transfers acknowledged before them and a set of the others that began before they ended explain, and rejects an audit that misses a transfer acknowledged before it began", "INV-STRICT-SERIALIZABLE")
	// audit is a read-only transaction from invoke to complete that saw balances.
	audit := func(id, invoke, complete int, balances map[string]int64) (history.Tx, history.BankAudit) {
		return tx(id, invoke, complete, history.OK), history.BankAudit{TxID: id, Balances: balances}
	}
	tests := []struct {
		name     string
		txs      []history.Tx
		balances map[string]int64
		want     string
	}{
		{
			name:     "accepts an audit that saw every transfer acknowledged before it began",
			txs:      []history.Tx{transfer(1, history.OK, "a", "b", 10), transfer(2, history.OK, "b", "c", 30)},
			balances: map[string]int64{"a": 90, "b": 80, "c": 130},
		},
		{
			name:     "accepts an audit that missed a transfer that was acknowledged only after it began",
			txs:      []history.Tx{transfer(1, history.OK, "a", "b", 10), tx(2, 31, 35, history.OK, app("b", -30), app("c", 30))},
			balances: map[string]int64{"a": 90, "b": 110, "c": 100},
		},
		{
			name:     "accepts an audit that saw a transfer that was acknowledged only after it began",
			txs:      []history.Tx{transfer(1, history.OK, "a", "b", 10), tx(2, 31, 35, history.OK, app("b", -30), app("c", 30))},
			balances: map[string]int64{"a": 90, "b": 80, "c": 130},
		},
		{
			name:     "accepts an audit that saw some of the transfers with an unknown outcome, but not those that failed",
			txs:      []history.Tx{transfer(1, history.Unknown, "a", "b", 10), transfer(2, history.Unknown, "b", "c", 30), transfer(3, history.Failed, "c", "a", 5)},
			balances: map[string]int64{"a": 100, "b": 70, "c": 130},
		},
		{
			name:     "rejects an audit that missed a transfer acknowledged before it began",
			txs:      []history.Tx{transfer(1, history.OK, "a", "b", 10), transfer(2, history.OK, "b", "c", 30)},
			balances: map[string]int64{"a": 90, "b": 110, "c": 100},
			want:     "stale read",
		},
		{
			name:     "rejects an audit that saw a transfer that began after it ended",
			txs:      []history.Tx{transfer(1, history.OK, "a", "b", 10), tx(2, 50, 55, history.OK, app("b", -30), app("c", 30))},
			balances: map[string]int64{"a": 90, "b": 80, "c": 130},
			want:     "stale read",
		},
		{
			name:     "rejects an audit that saw a failed transfer",
			txs:      []history.Tx{transfer(1, history.Failed, "a", "b", 10)},
			balances: map[string]int64{"a": 90, "b": 110},
			want:     "stale read",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			at, a := audit(100, 30, 40, test.balances)
			err := history.CheckBankAudits(append(test.txs, at), []history.BankAudit{a}, 100)
			if test.want == "" {
				is.NotError(t, err)
				return
			}
			is.Equal(t, test.want, anomalyKind(t, err), fmt.Sprint(err))
		})
	}

	t.Run("ignores an audit without a transaction, such as the final state", func(t *testing.T) {
		txs := []history.Tx{transfer(1, history.OK, "a", "b", 10)}
		is.NotError(t, history.CheckBankAudits(txs, []history.BankAudit{{TxID: -1, Balances: map[string]int64{"a": 100, "b": 100}}}, 100))
	})

	t.Run("reports that it cannot decide too many combinations of concurrent transfers", func(t *testing.T) {
		var txs []history.Tx
		for id := 1; id <= 44; id++ {
			txs = append(txs, transfer(id, history.Unknown, "a", "b", int64(id)))
		}
		at, a := audit(100, 1000, 1010, map[string]int64{"a": 100, "b": 100})
		err := history.CheckBankAudits(append(txs, at), []history.BankAudit{a}, 100)
		is.True(t, errors.Is(err, history.ErrUndecided), fmt.Sprint(err))
		is.Equal(t, "", anomalyKind(t, err))
	})

	t.Run("accepts every audit of a serial history, and rejects one that misses a transfer acknowledged before it began", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			accounts := []string{"a", "b", "c", "d"}
			balances := map[string]int64{}
			for _, a := range accounts {
				balances[a] = 100
			}
			withUnknown := hegel.Draw(ht, hegel.Booleans())
			outcomes := []history.Outcome{history.OK, history.OK, history.Failed}
			if withUnknown {
				outcomes = append(outcomes, history.Unknown)
			}
			var txs []history.Tx
			var audits []history.BankAudit
			// last is the last transfer acknowledged before the next audit begins.
			var last *history.Tx
			n := hegel.Draw(ht, hegel.Integers(1, 30))
			for id := 1; id <= n; id++ {
				if hegel.Draw(ht, hegel.Integers(0, 2)) == 0 {
					snapshot := map[string]int64{}
					for k, v := range balances {
						snapshot[k] = v
					}
					txs = append(txs, tx(id, id*10, id*10+5, history.OK))
					audits = append(audits, history.BankAudit{TxID: id, Balances: snapshot})
					continue
				}
				outcome := hegel.Draw(ht, hegel.SampledFrom(outcomes))
				from := hegel.Draw(ht, hegel.SampledFrom(accounts))
				to := hegel.Draw(ht, hegel.SampledFrom(accounts[1:]))
				if from == to {
					from = accounts[0]
				}
				amount := int64(hegel.Draw(ht, hegel.Integers(1, 50)))
				if outcome == history.OK || outcome == history.Unknown && hegel.Draw(ht, hegel.Booleans()) {
					balances[from] -= amount
					balances[to] += amount
				}
				txs = append(txs, transfer(id, outcome, from, to, amount))
				if outcome == history.OK {
					last = &txs[len(txs)-1]
				}
			}
			if err := history.CheckBankAudits(txs, audits, 100); err != nil {
				ht.Fatalf("rejected a serial history: %v", err)
			}
			if withUnknown || last == nil {
				return
			}
			// An audit after the last acknowledged transfer that does not see it.
			stale := map[string]int64{}
			for k, v := range balances {
				stale[k] = v
			}
			for _, op := range last.Ops {
				stale[op.Key] -= op.Value
			}
			id := n + 1
			txs = append(txs, tx(id, id*10, id*10+5, history.OK))
			if err := history.CheckBankAudits(txs, append(audits, history.BankAudit{TxID: id, Balances: stale}), 100); err == nil {
				ht.Fatalf("accepted an audit without %v", *last)
			}
		}, hegel.WithTestCases(500))
	})
}

// serialHistory generates transactions that ran one after another over a model, so the
// history is strictly serializable by construction.
func serialHistory(ht *hegel.T) ([]history.Tx, map[string][]int64) {
	keys := []string{"a", "b", "c"}
	state := map[string][]int64{}
	var txs []history.Tx
	now := 0
	next := int64(1)
	n := hegel.Draw(ht, hegel.Integers(1, 12))
	for id := 1; id <= n; id++ {
		outcome := hegel.Draw(ht, hegel.SampledFrom([]history.Outcome{history.OK, history.OK, history.OK, history.Failed, history.Unknown}))
		pending := map[string][]int64{}
		for k, v := range state {
			pending[k] = append([]int64(nil), v...)
		}
		var ops []history.Op
		for range hegel.Draw(ht, hegel.Integers(1, 4)) {
			key := hegel.Draw(ht, hegel.SampledFrom(keys))
			if hegel.Draw(ht, hegel.Booleans()) {
				ops = append(ops, app(key, next))
				pending[key] = append(pending[key], next)
				next++
			} else {
				ops = append(ops, read(key, pending[key]...))
			}
		}
		invoke := now
		now += hegel.Draw(ht, hegel.Integers(1, 5))
		txs = append(txs, tx(id, invoke, now, outcome, ops...))
		now++
		applied := outcome == history.OK || (outcome == history.Unknown && hegel.Draw(ht, hegel.Booleans()))
		if applied {
			state = pending
		}
	}
	return txs, state
}

func TestChecks_Properties(t *testing.T) {
	t.Run("accept every serial history", func(t *testing.T) {
		verify.Covers(t, "the checkers accept every generated serial history, so they raise no false alarms", "AC-1.8")
		hegel.Test(t, func(ht *hegel.T) {
			txs, final := serialHistory(ht)
			if err := history.CheckListAppend(txs, final); err != nil {
				ht.Fatalf("list-append: %v", err)
			}
			if err := history.CheckDurability(txs, final); err != nil {
				ht.Fatalf("durability: %v", err)
			}
		}, hegel.WithTestCases(1000))
	})

	t.Run("reject a serial history whose acknowledged read was truncated by one value", func(t *testing.T) {
		verify.Covers(t, "the checkers reject every generated serial history with one acknowledged read truncated", "AC-2", "AC-1.8")
		hegel.Test(t, func(ht *hegel.T) {
			txs, final := serialHistory(ht)
			// Dropping a value written by an unknown transaction is consistent, because that
			// transaction has no completion and may be ordered after the read.
			acked := map[int64]bool{}
			for _, tx := range txs {
				if tx.Outcome != history.OK {
					continue
				}
				for _, op := range tx.Ops {
					if op.Kind == history.Append {
						acked[op.Value] = true
					}
				}
			}
			var candidates []int
			for i, tx := range txs {
				if tx.Outcome != history.OK {
					continue
				}
				for j, op := range tx.Ops {
					if op.Kind == history.Read && len(op.List) > 0 && acked[op.List[len(op.List)-1]] {
						candidates = append(candidates, i*100+j)
					}
				}
			}
			ht.Assume(len(candidates) > 0)
			c := hegel.Draw(ht, hegel.SampledFrom(candidates))
			op := &txs[c/100].Ops[c%100]
			op.List = op.List[:len(op.List)-1]
			err := history.CheckListAppend(txs, final)
			if err == nil {
				ht.Fatalf("accepted truncated read in %v", txs[c/100])
			}
		}, hegel.WithTestCases(1000))
	})

	t.Run("reject a serial history where an acknowledged append was dropped from the final state", func(t *testing.T) {
		verify.Covers(t, "the checkers reject every generated serial history whose final state lost one acknowledged append", "AC-2", "AC-3.1")
		hegel.Test(t, func(ht *hegel.T) {
			txs, final := serialHistory(ht)
			var candidates []int64
			for _, tx := range txs {
				if tx.Outcome != history.OK {
					continue
				}
				for _, op := range tx.Ops {
					if op.Kind == history.Append {
						candidates = append(candidates, op.Value)
					}
				}
			}
			ht.Assume(len(candidates) > 0)
			victim := hegel.Draw(ht, hegel.SampledFrom(candidates))
			for key, list := range final {
				var kept []int64
				for _, v := range list {
					if v != victim {
						kept = append(kept, v)
					}
				}
				final[key] = kept
			}
			if err := history.CheckDurability(txs, final); err == nil {
				ht.Fatalf("accepted dropped value %d", victim)
			}
		}, hegel.WithTestCases(1000))
	})
}

func TestAnomaly(t *testing.T) {
	t.Run("prints the kind, message, and transactions", func(t *testing.T) {
		a := &history.Anomaly{Kind: "cycle", Message: "x", Txs: []history.Tx{tx(1, 0, 1, history.OK, app("k", 1))}}
		s := a.Error()
		is.True(t, strings.HasPrefix(s, "cycle: x"))
		is.True(t, strings.Contains(s, "append(k, 1)"))
	})
}
