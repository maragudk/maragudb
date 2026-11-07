// Package history records transaction histories and checks them for anomalies.
//
// The list-append checker follows Elle: every key holds a list, writes append a unique value,
// and reads return the whole list. From the reads it infers the version order of each key, builds
// a dependency graph with write-write, write-read, and read-write edges plus real-time order, and
// reports any cycle, which is a violation of strict serializability.
package history

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Outcome of a transaction as seen by its client.
type Outcome int

// Outcomes.
const (
	// OK means the commit was acknowledged.
	OK Outcome = iota
	// Failed means the transaction was definitely not applied.
	Failed
	// Unknown means the client does not know whether the transaction was applied.
	Unknown
)

func (o Outcome) String() string {
	switch o {
	case OK:
		return "ok"
	case Failed:
		return "failed"
	case Unknown:
		return "unknown"
	}
	return fmt.Sprint("outcome(", int(o), ")")
}

// OpKind of a list-append operation.
type OpKind int

// Operation kinds.
const (
	// Read of a key's whole list.
	Read OpKind = iota
	// Append of a value to a key's list.
	Append
)

// Op is one list-append operation inside a transaction.
type Op struct {
	Kind  OpKind
	Key   string
	Value int64
	// List read, for reads that completed.
	List []int64
}

func (o Op) String() string {
	if o.Kind == Append {
		return fmt.Sprintf("append(%s, %d)", o.Key, o.Value)
	}
	return fmt.Sprintf("read(%s)=%v", o.Key, o.List)
}

// Tx is one transaction in a history.
type Tx struct {
	ID     int
	Client int
	// Invoke and Complete are when the client started the transaction and when it learned the
	// outcome, as monotonic timestamps in any unit.
	Invoke   time.Duration
	Complete time.Duration
	Ops      []Op
	Outcome  Outcome
	Err      string
}

func (t Tx) String() string {
	var ops []string
	for _, op := range t.Ops {
		ops = append(ops, op.String())
	}
	return fmt.Sprintf("tx %d (client %d, %v-%v, %v): %s", t.ID, t.Client, t.Invoke, t.Complete, t.Outcome, strings.Join(ops, ", "))
}

// Anomaly found by a checker.
type Anomaly struct {
	Kind    string
	Message string
	Txs     []Tx
}

func (a *Anomaly) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", a.Kind, a.Message)
	for _, tx := range a.Txs {
		b.WriteString("\n  ")
		b.WriteString(tx.String())
	}
	return b.String()
}

// BankAudit is one read of all account balances.
type BankAudit struct {
	TxID     int
	Balances map[string]int64
}

// CheckBank verifies that every audit sums to total and that no balance is negative.
func CheckBank(audits []BankAudit, total int64) error {
	for _, a := range audits {
		var sum int64
		for account, balance := range a.Balances {
			if balance < 0 {
				return &Anomaly{Kind: "negative balance", Message: fmt.Sprintf("tx %d read %s = %d", a.TxID, account, balance)}
			}
			sum += balance
		}
		if sum != total {
			return &Anomaly{Kind: "money not conserved", Message: fmt.Sprintf("tx %d read a total of %d, want %d: %v", a.TxID, sum, total, a.Balances)}
		}
	}
	return nil
}

// ErrUndecided is returned by [CheckBankDurability] for a history whose transfers with an unknown
// outcome have too many combinations to try.
var ErrUndecided = errors.New("too many combinations of transfers with an unknown outcome to decide")

// CheckBankDurability verifies the final balances of the bank workload against its transfers,
// each a transaction whose appends are the amounts that it moved into accounts, negative for the
// account it moved them from. Every balance must be initial plus the amounts of every acknowledged
// transfer and of one set of the transfers whose outcome is unknown, the same set for every
// account, so that a lost acknowledged transfer shows even where it conserves money. Transfers
// with an unknown outcome that move the same amounts count as one with a multiplicity, and it
// returns [ErrUndecided], and no [*Anomaly], when their combinations are more than
// [maxCombinations] for each half of them.
func CheckBankDurability(txs []Tx, initial int64, final map[string]int64) error {
	index := map[string]int{}
	var accounts []string
	account := func(name string) int {
		i, ok := index[name]
		if !ok {
			i = len(accounts)
			index[name] = i
			accounts = append(accounts, name)
		}
		return i
	}
	names := make([]string, 0, len(final))
	for name := range final {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		account(name)
	}
	type delta map[int]int64
	var acked, unknown []delta
	for _, tx := range txs {
		if tx.Outcome == Failed {
			continue
		}
		d := delta{}
		for _, op := range tx.Ops {
			if op.Kind == Append {
				d[account(op.Key)] += op.Value
			}
		}
		if tx.Outcome == OK {
			acked = append(acked, d)
		} else {
			unknown = append(unknown, d)
		}
	}
	missing := make([]int64, len(accounts))
	for name, i := range index {
		missing[i] = final[name] - initial
	}
	for _, d := range acked {
		for i, v := range d {
			missing[i] -= v
		}
	}
	// Transfers that move the same amounts between the same accounts are one group, of which any
	// number up to its size may have landed, and a transfer that moves nothing does not matter.
	groupOf := map[string]int{}
	var groups []group
	for _, d := range unknown {
		v := make([]int64, len(accounts))
		zero := true
		for i, x := range d {
			v[i] = x
			zero = zero && x == 0
		}
		if zero {
			continue
		}
		k := vectorKey(v)
		if g, ok := groupOf[k]; ok {
			groups[g].size++
			continue
		}
		groupOf[k] = len(groups)
		groups = append(groups, group{vector: v, size: 1})
	}
	found, ok := sumsTo(groups, missing)
	if !ok {
		return ErrUndecided
	}
	if found {
		return nil
	}
	diff := map[string]int64{}
	for i, name := range accounts {
		if missing[i] != 0 {
			diff[name] = missing[i]
		}
	}
	return &Anomaly{Kind: "lost acknowledged write", Message: fmt.Sprintf("the final balances differ from those of the acknowledged transfers by %v, which no set of the %d transfers whose outcome is unknown makes up", diff, len(unknown))}
}

// maxCombinations of the groups of each half that [sumsTo] tries.
const maxCombinations = 1 << 20

// group of equal vectors, of which a sum may take any number up to size.
type group struct {
	vector []int64
	size   int
}

func vectorKey(v []int64) string {
	b := make([]byte, 0, 8*len(v))
	for _, x := range v {
		b = binary.LittleEndian.AppendUint64(b, uint64(x))
	}
	return string(b)
}

// sumsTo reports whether some choice of up to the size of each group of vectors sums to target,
// by meeting in the middle: it keeps the sums of every choice in the first half of the groups,
// and looks up what each choice in the second half leaves of the target. It reports false for ok
// when a half has more than [maxCombinations] choices.
func sumsTo(groups []group, target []int64) (found, ok bool) {
	// The halves split the combinations rather than the groups.
	half, left := 0, 1
	for half < len(groups) && left*(groups[half].size+1) <= maxCombinations {
		left *= groups[half].size + 1
		half++
	}
	right := 1
	for _, g := range groups[half:] {
		right *= g.size + 1
		if right > maxCombinations {
			return false, false
		}
	}
	// each calls f with the sum of every choice from gs, until f returns true.
	each := func(gs []group, f func(sum []int64) bool) bool {
		sum := make([]int64, len(target))
		var visit func(i int) bool
		visit = func(i int) bool {
			if i == len(gs) {
				return f(sum)
			}
			found := false
			taken := 0
			for ; taken <= gs[i].size; taken++ {
				if visit(i + 1) {
					found = true
					break
				}
				if taken < gs[i].size {
					for j, x := range gs[i].vector {
						sum[j] += x
					}
				}
			}
			for j, x := range gs[i].vector {
				sum[j] -= x * int64(min(taken, gs[i].size))
			}
			return found
		}
		return visit(0)
	}
	sums := map[string]bool{}
	each(groups[:half], func(sum []int64) bool {
		sums[vectorKey(sum)] = true
		return false
	})
	rest := make([]int64, len(target))
	return each(groups[half:], func(sum []int64) bool {
		for j := range rest {
			rest[j] = target[j] - sum[j]
		}
		return sums[vectorKey(rest)]
	}), true
}

// CheckDurability verifies the acknowledged commits against the final state read after all
// activity: every acknowledged append is present, no failed append is present anywhere, and
// an unknown transaction's appends are either all present or all absent.
func CheckDurability(txs []Tx, final map[string][]int64) error {
	if err := validate(txs); err != nil {
		return err
	}
	present := map[int64]string{}
	writers := map[int64]Tx{}
	for key, list := range final {
		for _, v := range list {
			if other, ok := present[v]; ok {
				return &Anomaly{Kind: "duplicate value", Message: fmt.Sprintf("value %d is in both %s and %s", v, other, key)}
			}
			present[v] = key
		}
	}
	for _, tx := range txs {
		for _, op := range tx.Ops {
			if op.Kind == Append {
				writers[op.Value] = tx
			}
		}
	}
	for v, key := range present {
		if _, ok := writers[v]; !ok {
			return &Anomaly{Kind: "unknown value", Message: fmt.Sprintf("final state of %s has value %d that no transaction appended", key, v)}
		}
	}
	for _, tx := range txs {
		var appended, found int
		for _, op := range tx.Ops {
			if op.Kind != Append {
				continue
			}
			appended++
			if key, ok := present[op.Value]; ok {
				if key != op.Key {
					return &Anomaly{Kind: "misplaced value", Message: fmt.Sprintf("value %d was appended to %s but is in %s", op.Value, op.Key, key), Txs: []Tx{tx}}
				}
				found++
			}
		}
		switch tx.Outcome {
		case OK:
			if found != appended {
				return &Anomaly{Kind: "lost acknowledged write", Message: fmt.Sprintf("%d of %d appends missing from the final state", appended-found, appended), Txs: []Tx{tx}}
			}
		case Failed:
			if found != 0 {
				return &Anomaly{Kind: "failed write applied", Message: fmt.Sprintf("%d appends of a failed transaction are in the final state", found), Txs: []Tx{tx}}
			}
		case Unknown:
			if found != 0 && found != appended {
				return &Anomaly{Kind: "partial write", Message: fmt.Sprintf("%d of %d appends of an unknown transaction are in the final state", found, appended), Txs: []Tx{tx}}
			}
		}
	}
	for _, tx := range txs {
		if tx.Outcome != Failed {
			continue
		}
		own := map[int64]bool{}
		for _, op := range tx.Ops {
			if op.Kind == Append {
				own[op.Value] = true
			}
		}
		for _, other := range txs {
			if other.ID == tx.ID {
				continue
			}
			for _, op := range other.Ops {
				if op.Kind != Read {
					continue
				}
				for _, v := range op.List {
					if own[v] {
						return &Anomaly{Kind: "read of failed write", Message: fmt.Sprintf("tx %d read value %d of a failed transaction", other.ID, v), Txs: []Tx{tx, other}}
					}
				}
			}
		}
	}
	return nil
}

// validate the preconditions of a history: unique transaction IDs, unique appended values, and
// invocation before completion.
func validate(txs []Tx) error {
	byID := map[int]bool{}
	writer := map[int64]int{}
	for _, tx := range txs {
		if byID[tx.ID] {
			return &Anomaly{Kind: "invalid history", Message: fmt.Sprintf("transaction ID %d is used twice", tx.ID), Txs: []Tx{tx}}
		}
		byID[tx.ID] = true
		if tx.Complete < tx.Invoke {
			return &Anomaly{Kind: "invalid history", Message: "completed before invoked", Txs: []Tx{tx}}
		}
		for _, op := range tx.Ops {
			if op.Kind != Append {
				continue
			}
			if other, ok := writer[op.Value]; ok {
				return &Anomaly{Kind: "invalid history", Message: fmt.Sprintf("value %d appended by both tx %d and tx %d", op.Value, other, tx.ID), Txs: []Tx{tx}}
			}
			writer[op.Value] = tx.ID
		}
	}
	return nil
}

// CheckListAppend verifies strict serializability of a list-append history. final is the
// state read after all activity, by key; when non-nil it must be the longest read of every key.
func CheckListAppend(txs []Tx, final map[string][]int64) error {
	if err := validate(txs); err != nil {
		return err
	}
	writer := map[int64]int{}
	byID := map[int]Tx{}
	for _, tx := range txs {
		byID[tx.ID] = tx
		for _, op := range tx.Ops {
			if op.Kind == Append {
				writer[op.Value] = tx.ID
			}
		}
	}

	// Transactions whose effects can be in the graph: acknowledged ones, and unknown ones whose
	// writes were observed. Failed ones must not be observed at all.
	observed := map[int64]bool{}
	for _, tx := range txs {
		for _, op := range tx.Ops {
			if op.Kind == Read {
				for _, v := range op.List {
					observed[v] = true
				}
			}
		}
	}
	for _, list := range final {
		for _, v := range list {
			observed[v] = true
		}
	}
	committed := map[int]bool{}
	for _, tx := range txs {
		switch tx.Outcome {
		case OK:
			committed[tx.ID] = true
		case Unknown:
			for _, op := range tx.Ops {
				if op.Kind == Append && observed[op.Value] {
					committed[tx.ID] = true
				}
			}
		}
	}

	// The version order of each key is the longest observed list; every other read must be a prefix of it.
	order := map[string][]int64{}
	for key, list := range final {
		order[key] = list
	}
	for _, tx := range txs {
		own := map[string][]int64{}
		for _, op := range tx.Ops {
			switch op.Kind {
			case Append:
				own[op.Key] = append(own[op.Key], op.Value)
			case Read:
				external, err := stripOwn(tx, op, own[op.Key])
				if err != nil {
					return err
				}
				for _, v := range external {
					w, ok := writer[v]
					if !ok {
						return &Anomaly{Kind: "unknown value", Message: fmt.Sprintf("read value %d that no transaction appended", v), Txs: []Tx{tx}}
					}
					if byID[w].Outcome == Failed {
						return &Anomaly{Kind: "read of failed write", Message: fmt.Sprintf("read value %d of failed tx %d", v, w), Txs: []Tx{byID[w], tx}}
					}
					if w == tx.ID {
						return &Anomaly{Kind: "internal inconsistency", Message: fmt.Sprintf("read own value %d out of place", v), Txs: []Tx{tx}}
					}
				}
				cur := order[op.Key]
				switch {
				case len(external) > len(cur):
					if !isPrefix(cur, external) {
						return &Anomaly{Kind: "incompatible orders", Message: fmt.Sprintf("key %s was read as %v and as %v", op.Key, cur, external), Txs: []Tx{tx}}
					}
					order[op.Key] = external
				default:
					if !isPrefix(external, cur) {
						return &Anomaly{Kind: "incompatible orders", Message: fmt.Sprintf("key %s was read as %v but its order is %v", op.Key, external, cur), Txs: []Tx{tx}}
					}
				}
			}
		}
	}
	for key, list := range order {
		seen := map[int64]bool{}
		for _, v := range list {
			if seen[v] {
				return &Anomaly{Kind: "duplicate value", Message: fmt.Sprintf("key %s holds value %d twice: %v", key, v, list)}
			}
			seen[v] = true
		}
		if final != nil && len(list) > len(final[key]) {
			return &Anomaly{Kind: "observed value missing from final state", Message: fmt.Sprintf("key %s was read as %v but ends as %v", key, list, final[key]), Txs: []Tx{byID[writer[list[len(list)-1]]]}}
		}
	}

	// A failed transaction's reads are still real observations of a snapshot, so it joins the
	// graph as a reader: it has no write edges and no completion, but it was invoked.
	participates := map[int]bool{}
	for _, tx := range txs {
		if committed[tx.ID] {
			participates[tx.ID] = true
			continue
		}
		if tx.Outcome != Failed {
			continue
		}
		for _, op := range tx.Ops {
			if op.Kind == Read {
				participates[tx.ID] = true
			}
		}
	}

	// Nodes and edges are added in a fixed order so that the reported cycle is the same on every run.
	g := newGraph()
	txNode := map[string]int{}
	for _, tx := range txs {
		if participates[tx.ID] {
			g.node(fmt.Sprint("tx", tx.ID))
			txNode[fmt.Sprint("tx", tx.ID)] = tx.ID
		}
	}
	keys := make([]string, 0, len(order))
	for key := range order {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Write-write edges along each key's version order.
	position := map[string]map[int64]int{}
	for _, key := range keys {
		list := order[key]
		position[key] = map[int64]int{}
		for i, v := range list {
			position[key][v] = i
			if i > 0 {
				from, to := writer[list[i-1]], writer[v]
				if from != to && committed[from] && committed[to] {
					g.edge(fmt.Sprint("tx", from), fmt.Sprint("tx", to), fmt.Sprintf("ww on %s: %d before %d", key, list[i-1], v))
				}
			}
		}
	}
	// Write-read and read-write edges from each read.
	for _, tx := range txs {
		if !participates[tx.ID] {
			continue
		}
		own := map[string][]int64{}
		for _, op := range tx.Ops {
			switch op.Kind {
			case Append:
				own[op.Key] = append(own[op.Key], op.Value)
			case Read:
				external, _ := stripOwn(tx, op, own[op.Key])
				list := order[op.Key]
				next := 0
				if len(external) > 0 {
					last := external[len(external)-1]
					if w := writer[last]; w != tx.ID && committed[w] {
						g.edge(fmt.Sprint("tx", w), fmt.Sprint("tx", tx.ID), fmt.Sprintf("wr on %s: read %d", op.Key, last))
					}
					next = position[op.Key][last] + 1
				}
				if next < len(list) {
					if w := writer[list[next]]; w != tx.ID && committed[w] {
						g.edge(fmt.Sprint("tx", tx.ID), fmt.Sprint("tx", w), fmt.Sprintf("rw on %s: read before %d", op.Key, list[next]))
					}
				}
			}
		}
	}
	// Real-time edges: a transaction that completed before another was invoked precedes it.
	// Barrier nodes keep the edge count linear. An unknown transaction has no completion.
	var invokes []time.Duration
	seenInvoke := map[time.Duration]bool{}
	for _, tx := range txs {
		if participates[tx.ID] && !seenInvoke[tx.Invoke] {
			seenInvoke[tx.Invoke] = true
			invokes = append(invokes, tx.Invoke)
		}
	}
	sort.Slice(invokes, func(i, j int) bool { return invokes[i] < invokes[j] })
	barrier := func(t time.Duration) string { return fmt.Sprint("barrier", t) }
	for i, t := range invokes {
		g.node(barrier(t))
		if i > 0 {
			g.edge(barrier(invokes[i-1]), barrier(t), "")
		}
	}
	for _, tx := range txs {
		if !participates[tx.ID] {
			continue
		}
		g.edge(barrier(tx.Invoke), fmt.Sprint("tx", tx.ID), fmt.Sprintf("invoked at %v", tx.Invoke))
		if tx.Outcome != OK {
			continue
		}
		i := sort.Search(len(invokes), func(i int) bool { return invokes[i] > tx.Complete })
		if i < len(invokes) {
			g.edge(fmt.Sprint("tx", tx.ID), barrier(invokes[i]), fmt.Sprintf("completed at %v", tx.Complete))
		}
	}

	if cycle := g.findCycle(); cycle != nil {
		var cycleTxs []Tx
		var steps []string
		for _, e := range cycle {
			if id, ok := txNode[e.from]; ok {
				cycleTxs = append(cycleTxs, byID[id])
			}
			if e.label != "" {
				steps = append(steps, fmt.Sprintf("%s -> %s (%s)", e.from, e.to, e.label))
			}
		}
		return &Anomaly{Kind: "cycle", Message: strings.Join(steps, "; "), Txs: cycleTxs}
	}
	return nil
}

// stripOwn removes the transaction's own appends from the tail of a read list and checks that
// they are there in order.
func stripOwn(tx Tx, op Op, own []int64) ([]int64, error) {
	list := op.List
	if len(own) == 0 {
		return list, nil
	}
	if len(list) < len(own) || !isPrefix(own, list[len(list)-len(own):]) {
		return nil, &Anomaly{Kind: "internal inconsistency", Message: fmt.Sprintf("read %v of %s does not end with own appends %v", list, op.Key, own), Txs: []Tx{tx}}
	}
	return list[:len(list)-len(own)], nil
}

func isPrefix(prefix, list []int64) bool {
	if len(prefix) > len(list) {
		return false
	}
	for i, v := range prefix {
		if list[i] != v {
			return false
		}
	}
	return true
}

type edge struct {
	from, to, label string
}

type graph struct {
	nodes []string
	index map[string]int
	out   map[string][]edge
}

func newGraph() *graph {
	return &graph{index: map[string]int{}, out: map[string][]edge{}}
}

func (g *graph) node(n string) {
	if _, ok := g.index[n]; !ok {
		g.index[n] = len(g.nodes)
		g.nodes = append(g.nodes, n)
	}
}

func (g *graph) edge(from, to, label string) {
	g.node(from)
	g.node(to)
	g.out[from] = append(g.out[from], edge{from: from, to: to, label: label})
}

// findCycle returns the edges of some cycle, or nil. Barrier-only cycles cannot exist because
// barriers form a chain.
func (g *graph) findCycle() []edge {
	const (
		white = iota
		gray
		black
	)
	color := map[string]int{}
	var stack []edge
	var found []edge
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = gray
		for _, e := range g.out[n] {
			switch color[e.to] {
			case white:
				stack = append(stack, e)
				if visit(e.to) {
					return true
				}
				stack = stack[:len(stack)-1]
			case gray:
				start := 0
				for i, s := range stack {
					if s.from == e.to {
						start = i
						break
					}
				}
				found = append(append([]edge(nil), stack[start:]...), e)
				return true
			}
		}
		color[n] = black
		return false
	}
	for _, n := range g.nodes {
		if color[n] == white && visit(n) {
			return found
		}
	}
	return nil
}
