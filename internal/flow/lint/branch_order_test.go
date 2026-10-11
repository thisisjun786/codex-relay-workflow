package lint

import (
	"slices"
	"strings"
	"testing"

	"github.com/dop251/goja/ast"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// staticFindings runs the static rules over a script body and lists what they find, without lines: rule, label and
// message, sorted. A permutation of the text moves lines, never what is found.
func staticFindings(body string, imports []string) ([]string, error) {
	a, err := parse(body, imports)
	if err != nil {
		return nil, err
	}
	var found []Diagnostic
	s := &scanner{a: a, policy: engine.FakePolicy(), out: &found}
	s.scan(a.prog, nil)
	s.flow()
	var out []string
	for _, d := range dedupe(found) {
		out = append(out, d.Rule+"|"+d.Label+"|"+d.Message)
	}
	slices.Sort(out)
	return out, nil
}

// branchSwaps are the texts of a body in which one if statement or conditional expression has its two alternatives
// swapped and its test negated: if (t) A else B becomes if (!(t)) {B} else {A}, if (t) A becomes if (!(t)) {} else {A},
// and t ? a : b becomes !(t) ? b : a. Each text runs the same paths as the body, in the other order.
func branchSwaps(body string, imports []string) ([]string, error) {
	a, err := parse(body, imports)
	if err != nil {
		return nil, err
	}
	base := len(a.src) - len(body) - len("\n})")
	off := func(n ast.Node) (int, int) {
		i, k := int(n.Idx0())-1-base, int(n.Idx1())-1-base
		if x, ok := n.(*ast.IfStatement); ok {
			// The parser leaves the position of the keyword unset: it is the last "if" before the test.
			t := int(x.Test.Idx0()) - 1 - base
			i = -1
			if t >= 0 {
				i = strings.LastIndex(body[:t], "if")
			}
		}
		return i, k
	}
	text := func(n ast.Node) string { i, k := off(n); return body[i:k] }
	var out []string
	var visit func(n any)
	visit = func(n any) {
		switch x := n.(type) {
		case *ast.IfStatement:
			if i, _ := off(x); i >= 0 {
				ti, tk := off(x.Test)
				ci, ck := off(x.Consequent)
				if x.Alternate == nil {
					out = append(out, body[:ti]+"!("+text(x.Test)+")"+body[tk:ci]+"{} else {"+text(x.Consequent)+"\n}"+body[ck:])
				} else {
					ai, ak := off(x.Alternate)
					out = append(out, body[:ti]+"!("+text(x.Test)+")"+body[tk:ci]+"{"+text(x.Alternate)+"\n}"+body[ck:ai]+"{"+text(x.Consequent)+"\n}"+body[ak:])
				}
			}
		case *ast.ConditionalExpression:
			if i, _ := off(x); i >= 0 {
				ti, tk := off(x.Test)
				ci, ck := off(x.Consequent)
				ai, ak := off(x.Alternate)
				out = append(out, body[:ti]+"!("+text(x.Test)+")"+body[tk:ci]+text(x.Alternate)+body[ck:ai]+text(x.Consequent)+body[ak:])
			}
		}
		for _, c := range children(n) {
			visit(c)
		}
	}
	visit(a.prog)
	return out, nil
}

// sameAcrossBranchOrders requires the static findings of a script to be the same whichever order each of its if
// statements and conditional expressions lists its alternatives in: a finding comes from the paths, not from the
// order the walk meets them in. Every script the lint tests check goes through it.
func sameAcrossBranchOrders(t *testing.T, name string, text []byte) {
	t.Helper()
	prog, err := script.Load(script.Source{Name: name, Text: text})
	if err != nil {
		return
	}
	want, err := staticFindings(prog.Body, prog.Imports)
	if err != nil {
		return
	}
	swaps, err := branchSwaps(prog.Body, prog.Imports)
	if err != nil {
		t.Fatal(err)
	}
	for i, body := range swaps {
		got, err := staticFindings(body, prog.Imports)
		if err != nil {
			t.Fatalf("%s: swap %d does not parse: %v\n%s", name, i, err, body)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the findings depend on the branch order (swap %d)\nin order: %v\nswapped:  %v\n%s", name, i, want, got, body)
		}
	}
}

// The swaps themselves: one per if statement and conditional, each a script that parses and differs from the body.
func TestBranchSwapsCoverEveryAlternative(t *testing.T) {
	body := "if (a) { x() } else { y() }\nif (b) z()\nconst v = c ? 1 : 2\n"
	swaps, err := branchSwaps(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"if (!(a)) {{ y() }\n} else {{ x() }\n}\nif (b) z()\nconst v = c ? 1 : 2\n",
		"if (a) { x() } else { y() }\nif (!(b)) {} else {z()\n}\nconst v = c ? 1 : 2\n",
		"if (a) { x() } else { y() }\nif (b) z()\nconst v = !(c) ? 2 : 1\n",
	}
	if !slices.Equal(swaps, want) {
		t.Errorf("swaps:\n%q\nwant\n%q", swaps, want)
	}
}
