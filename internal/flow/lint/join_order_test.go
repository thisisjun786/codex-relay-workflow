package lint

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/file"
)

// The join of two states does not depend on which one the walk met first, whatever the pending events of the names
// are: the alternatives of a name, their generation on each path, a name pending on one path only.
func TestStateJoinIsOrderFree(t *testing.T) {
	calls := []*ast.CallExpression{{LeftParenthesis: 10}, {LeftParenthesis: 20}, {LeftParenthesis: 30}}
	for i, c := range calls {
		c.Callee = &ast.Identifier{Idx: file.Idx(i*10 + 1)}
		c.RightParenthesis = c.LeftParenthesis + 1
	}
	bds := []*binding{{name: "p"}, {name: "q"}}
	evs := []uint8{pCommitted, pReviewed, pLanded}
	gen := func(seed int) *state {
		st := newState()
		st.live = true
		r := seed
		next := func(n int) int { r = (r*1103515245 + 12345) & 0x7fffffff; return r % n }
		for _, slot := range []string{"n", "m"} {
			if next(3) > 0 {
				st.phases[slot] = uint8(1) << next(6)
			}
		}
		for _, bd := range bds {
			var set pendSet
			for i, c := range calls {
				// A call names one node and one event; what differs between paths is its generation.
				if next(2) == 0 {
					set.add(pending{slot: []string{"n", "m"}[i%2], ev: evs[i], call: c, paths: uint8(next(3) + 1)})
				}
			}
			if len(set.alts) > 0 {
				set.partial = next(2) == 0
				st.pend[bd] = set
			}
		}
		return st
	}
	for i := 0; i < 2000; i++ {
		a, b := gen(2*i+1), gen(2*i+2)
		ab, ba := a.clone(), b.clone()
		ab.join(b)
		ba.join(a)
		if !ab.equal(ba) {
			t.Fatalf("seed %d: the join depends on the order:\n%s\n%s", i, dumpPend(ab), dumpPend(ba))
		}
		again := ab.clone()
		again.join(ab)
		if !again.equal(ab) {
			t.Fatalf("seed %d: a state joined with itself changes", i)
		}
	}
}

func dumpPend(st *state) string {
	var b strings.Builder
	fmt.Fprintf(&b, "phases %v;", st.phases)
	for bd, set := range st.pend {
		fmt.Fprintf(&b, " %s partial=%v", bd.name, set.partial)
		for _, p := range set.alts {
			fmt.Fprintf(&b, " [%s %d @%d paths %d]", p.slot, p.ev, p.call.Idx0(), p.paths)
		}
	}
	return b.String()
}
