package lint

import (
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
)

// The lifecycle of one executor node, as an automaton over the states a node can be in:
//
//	init -build-> built -commit-> committed -verify-> verified -review-> reviewed -land-> landed
//
// with a commit allowed again from built, verified or reviewed (a fix round), a build allowed again until the node has
// landed (a rebuild, the second tier), and a land allowed once, from reviewed.
const (
	pInit uint8 = 1 << iota
	pBuilt
	pCommitted
	pVerified
	pReviewed
	pLanded
)

var phaseNames = map[uint8]string{pInit: "before the build", pBuilt: "built", pCommitted: "committed", pVerified: "verified", pReviewed: "reviewed", pLanded: "landed"}

func phaseList(mask uint8) string {
	var names []string
	for bit := pInit; bit != 0 && bit <= pLanded; bit <<= 1 {
		if mask&bit != 0 {
			names = append(names, phaseNames[bit])
		}
	}
	return strings.Join(names, " or ")
}

// state is what holds on every path that reaches a point of a function body: the set of lifecycle phases the node may be
// in, the labels already used, and the role family of the last author call.
type state struct {
	phases uint8
	labels map[string]bool
	author string
}

func (s *state) dead() bool { return s.phases == 0 }
func (s *state) kill()      { s.phases = 0 }

func (s *state) clone() *state {
	c := &state{phases: s.phases, author: s.author, labels: make(map[string]bool, len(s.labels))}
	for k := range s.labels {
		c.labels[k] = true
	}
	return c
}

// join merges the state of another path into s: the phases may be any of both, a label is used only when it is on both,
// and the author is known only when both agree.
func (s *state) join(o *state) {
	switch {
	case o.dead():
		return
	case s.dead():
		*s = *o.clone()
		return
	}
	s.phases |= o.phases
	for k := range s.labels {
		if !o.labels[k] {
			delete(s.labels, k)
		}
	}
	if s.author != o.author {
		s.author = ""
	}
}

func (s *state) equal(o *state) bool {
	if s.phases != o.phases || s.author != o.author || len(s.labels) != len(o.labels) {
		return false
	}
	for k := range s.labels {
		if !o.labels[k] {
			return false
		}
	}
	return true
}

type exit struct {
	breaks, continues *state
	isLoop            bool
}

// walker walks the body of one function in evaluation order.
type walker struct {
	s     *scanner
	fn    *fnInfo
	life  bool
	exits []*exit
	// loopFree has one entry per enclosing loop: whether its body has no break, return or throw, so every round reaches
	// every call in it.
	loopFree []bool
}

// flow checks lifecycle, labels and reviewer family over the body of every function.
func (s *scanner) flow() {
	for _, fn := range s.a.order {
		w := &walker{s: s, fn: fn}
		w.life = w.hasLifecycle()
		start := &state{phases: pInit, labels: map[string]bool{}}
		if w.life && !w.hasBuild() {
			start.phases = pBuilt
		}
		switch f := fn.node.(type) {
		case *ast.FunctionLiteral:
			if f.Body != nil {
				w.stmts(f.Body.List, start)
			}
		case *ast.ArrowFunctionLiteral:
			switch body := f.Body.(type) {
			case *ast.BlockStatement:
				w.stmts(body.List, start)
			case *ast.ExpressionBody:
				w.expr(body.Expression, start)
			}
		}
	}
}

// walkOwn visits the nodes of the function that are not in a function nested in it.
func (w *walker) walkOwn(visit func(n any)) {
	var rec func(n any)
	rec = func(n any) {
		switch n.(type) {
		case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
			if w.s.a.fns[n] != w.fn {
				return
			}
		}
		visit(n)
		for _, c := range children(n) {
			rec(c)
		}
	}
	switch f := w.fn.node.(type) {
	case *ast.FunctionLiteral:
		if f.Body != nil {
			for _, st := range f.Body.List {
				rec(st)
			}
		}
	case *ast.ArrowFunctionLiteral:
		switch body := f.Body.(type) {
		case *ast.BlockStatement:
			for _, st := range body.List {
				rec(st)
			}
		case *ast.ExpressionBody:
			rec(body.Expression)
		}
	}
}

func (w *walker) hasLifecycle() bool {
	found := false
	w.walkOwn(func(n any) {
		if c, ok := n.(*ast.CallExpression); ok {
			if obj, name := w.s.a.callee(c); obj == "crw" && lifecycleOp(name) != 0 {
				found = true
			}
		}
	})
	w.fn.life = found
	return found
}

// hasBuild reports whether the function itself starts an executor thread.
func (w *walker) hasBuild() bool {
	found := false
	w.walkOwn(func(n any) {
		if c, ok := n.(*ast.CallExpression); ok {
			if call, ok := w.s.a.agentFacts(c, w.fn, w.s.policy); ok && call.family == "executor" && !call.resume {
				found = true
			}
		}
	})
	return found
}

func lifecycleOp(name string) uint8 {
	switch name {
	case "commit":
		return pCommitted
	case "verify":
		return pVerified
	case "review":
		return pReviewed
	case "land":
		return pLanded
	}
	return 0
}

// legalFrom are the phases an event may happen in.
var legalFrom = map[uint8]uint8{
	pBuilt:     pInit | pBuilt | pCommitted | pVerified | pReviewed,
	pCommitted: pBuilt | pVerified | pReviewed,
	pVerified:  pCommitted,
	pReviewed:  pVerified,
	pLanded:    pReviewed,
}

var eventNames = map[uint8]string{pBuilt: "build", pCommitted: "crw.commit", pVerified: "crw.verify", pReviewed: "crw.review", pLanded: "crw.land"}

// event applies one lifecycle event. It is reported only when it is illegal in every phase the node may be in, so a
// path the text cannot rule out is not a violation; the node then goes on in the event's phase.
func (w *walker) event(ev uint8, line int, st *state) {
	if st.dead() {
		return
	}
	if w.life && st.phases&legalFrom[ev] == 0 {
		switch {
		case ev == pLanded && st.phases == pLanded:
			w.s.add(RuleLandTwice, line, "", "crw.land runs a second time for the node: land happens at most once")
		case ev == pLanded:
			w.s.add(RuleLandWithoutReview, line, "", "crw.land runs while the node is %s: a land needs a completed review of the committed head (commit, verify, review)", phaseList(st.phases))
		default:
			w.s.add(RuleLifecycleOrder, line, "", "%s runs while the node is %s: the order is build, then rounds of commit, verify, review, then land", eventNames[ev], phaseList(st.phases))
		}
	}
	if w.life {
		st.phases = ev
	}
}

func (w *walker) stmts(list []ast.Statement, st *state) {
	for _, s := range list {
		if st.dead() {
			return
		}
		w.stmt(s, st)
	}
}

func (w *walker) pushExit(loop bool) *exit {
	e := &exit{breaks: &state{labels: map[string]bool{}}, continues: &state{labels: map[string]bool{}}, isLoop: loop}
	w.exits = append(w.exits, e)
	return e
}

func (w *walker) popExit() { w.exits = w.exits[:len(w.exits)-1] }

func (w *walker) stmt(n ast.Statement, st *state) {
	switch s := n.(type) {
	case *ast.ExpressionStatement:
		w.expr(s.Expression, st)
	case *ast.VariableStatement:
		for _, b := range s.List {
			w.expr(b.Initializer, st)
		}
	case *ast.LexicalDeclaration:
		for _, b := range s.List {
			w.expr(b.Initializer, st)
		}
	case *ast.BlockStatement:
		w.stmts(s.List, st)
	case *ast.ReturnStatement:
		w.expr(s.Argument, st)
		st.kill()
	case *ast.ThrowStatement:
		w.expr(s.Argument, st)
		st.kill()
	case *ast.IfStatement:
		w.expr(s.Test, st)
		other := st.clone()
		w.stmt(s.Consequent, st)
		if s.Alternate != nil {
			w.stmt(s.Alternate, other)
		}
		st.join(other)
	case *ast.LabelledStatement:
		w.stmt(s.Statement, st)
	case *ast.BranchStatement:
		for i := len(w.exits) - 1; i >= 0; i-- {
			e := w.exits[i]
			if s.Token == token.CONTINUE && !e.isLoop {
				continue
			}
			if s.Token == token.CONTINUE {
				e.continues.join(st)
			} else {
				e.breaks.join(st)
			}
			break
		}
		st.kill()
	case *ast.ForStatement:
		switch init := s.Initializer.(type) {
		case *ast.ForLoopInitializerExpression:
			w.expr(init.Expression, st)
		case *ast.ForLoopInitializerVarDeclList:
			for _, b := range init.List {
				w.expr(b.Initializer, st)
			}
		case *ast.ForLoopInitializerLexicalDecl:
			for _, b := range init.LexicalDeclaration.List {
				w.expr(b.Initializer, st)
			}
		}
		w.loop(s.Test, s.Update, s.Body, s.Test != nil, st)
	case *ast.WhileStatement:
		w.loop(s.Test, nil, s.Body, !truthy(s.Test), st)
	case *ast.DoWhileStatement:
		// The body runs once before the test.
		once := w.pushExit(true)
		w.stmt(s.Body, st)
		w.popExit()
		st.join(once.continues)
		w.expr(s.Test, st)
		first := st.clone()
		w.loop(s.Test, nil, s.Body, true, first)
		st.join(once.breaks)
		st.join(first)
	case *ast.ForInStatement:
		w.expr(s.Source, st)
		w.loop(nil, nil, s.Body, true, st)
	case *ast.ForOfStatement:
		w.expr(s.Source, st)
		w.loop(nil, nil, s.Body, true, st)
	case *ast.TryStatement:
		pre := st.clone()
		w.stmts(s.Body.List, st)
		if s.Catch != nil {
			c := pre.clone()
			c.join(st)
			if s.Catch.Body != nil {
				w.stmts(s.Catch.Body.List, c)
			}
			st.join(c)
		}
		if s.Finally != nil {
			if st.dead() {
				// The finally block still runs on a path that returned; check it from the state before the try.
				fin := pre.clone()
				w.stmts(s.Finally.List, fin)
			} else {
				w.stmts(s.Finally.List, st)
			}
		}
	case *ast.SwitchStatement:
		w.expr(s.Discriminant, st)
		pre := st.clone()
		e := w.pushExit(false)
		after := &state{labels: map[string]bool{}}
		var prev *state
		hasDefault := false
		for i, c := range s.Body {
			if i == s.Default {
				hasDefault = true
			}
			start := pre.clone()
			if prev != nil {
				start.join(prev)
			}
			w.expr(c.Test, start)
			w.stmts(c.Consequent, start)
			prev = start
		}
		w.popExit()
		if prev != nil {
			after.join(prev)
		}
		after.join(e.breaks)
		if !hasDefault {
			after.join(pre)
		}
		*st = *after
	}
}

// leavesEarly reports whether a loop body has a break, a return or a throw outside the functions nested in it: then a
// round may be the last, and whether a call in the body repeats is for the dry run to say.
func leavesEarly(body ast.Statement) bool {
	found := false
	var rec func(n any)
	rec = func(n any) {
		if found {
			return
		}
		switch x := n.(type) {
		case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
			return
		case *ast.ReturnStatement, *ast.ThrowStatement:
			found = true
			return
		case *ast.BranchStatement:
			if x.Token == token.BREAK {
				found = true
				return
			}
		}
		for _, c := range children(n) {
			rec(c)
		}
	}
	rec(body)
	return found
}

// truthy reports whether a loop test is the literal true (a loop that exits only through break or return).
func truthy(e ast.Expression) bool {
	b, ok := e.(*ast.BooleanLiteral)
	return ok && b.Value
}

// loop walks a loop body to a fixed point: the state at the head is the entry state joined with every state that comes
// round again. endless is set for a loop with no test or a constant true one, which is left only through break or return.
func (w *walker) loop(test, update ast.Expression, body ast.Statement, hasTest bool, st *state) {
	head := st.clone()
	breaks := &state{labels: map[string]bool{}}
	w.loopFree = append(w.loopFree, !leavesEarly(body))
	for i := 0; i < 4; i++ {
		cur := head.clone()
		w.expr(test, cur)
		e := w.pushExit(true)
		w.stmt(body, cur)
		w.popExit()
		cur.join(e.continues)
		w.expr(update, cur)
		breaks.join(e.breaks)
		next := head.clone()
		next.join(cur)
		if next.equal(head) {
			break
		}
		head = next
	}
	w.loopFree = w.loopFree[:len(w.loopFree)-1]
	exitState := &state{labels: map[string]bool{}}
	if hasTest {
		exitState = head.clone()
		w.expr(test, exitState)
	}
	exitState.join(breaks)
	*st = *exitState
}

// expr evaluates an expression: its parts in order, then, for a call, the call itself. A function literal is not
// evaluated here; it has its own walk.
func (w *walker) expr(n any, st *state) {
	switch e := n.(type) {
	case nil:
		return
	case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral, *ast.ClassLiteral:
		return
	case *ast.ConditionalExpression:
		w.expr(e.Test, st)
		other := st.clone()
		w.expr(e.Consequent, st)
		w.expr(e.Alternate, other)
		st.join(other)
		return
	case *ast.BinaryExpression:
		if e.Operator == token.LOGICAL_AND || e.Operator == token.LOGICAL_OR || e.Operator == token.COALESCE {
			w.expr(e.Left, st)
			other := st.clone()
			w.expr(e.Right, other)
			st.join(other)
			return
		}
	}
	for _, c := range children(n) {
		w.expr(c, st)
	}
	if c, ok := n.(*ast.CallExpression); ok {
		w.call(c, st)
	}
}

func (w *walker) call(c *ast.CallExpression, st *state) {
	a := w.s.a
	line := a.lineOf(c)
	obj, name := a.callee(c)
	switch {
	case obj == "crw" && lifecycleOp(name) != 0:
		w.event(lifecycleOp(name), line, st)
	case obj == "agent":
		call, _ := a.agentFacts(c, w.fn, w.s.policy)
		w.agent(call, st)
	}
}

func (w *walker) agent(call *agentCall, st *state) {
	a := w.s.a
	if call.hasLabel && call.labelStatic {
		switch {
		case st.labels[call.label]:
			w.s.add(RuleLabelDuplicate, call.line, call.label, "label %q is used by an earlier call on this path; labels are unique within a path", call.label)
		case len(w.loopFree) > 0 && w.loopFree[len(w.loopFree)-1]:
			w.s.add(RuleLabelDuplicate, call.line, call.label, "label %q is on a call inside a loop that leaves only when its rounds are done, so every round repeats it; labels are unique within a path (put the round in the label)", call.label)
		}
		st.labels[call.label] = true
	}
	if call.role == "" || call.resume {
		return
	}
	if a.reviewCall(call) {
		if st.author != "" && call.family == st.author {
			w.s.add(RuleReviewerFamily, call.line, call.label, "a review call runs as %s, the same role family (%s) as the call that wrote the change; a review comes from another family than the author's", call.role, call.family)
		}
		return
	}
	if call.family == "executor" {
		st.author = call.family
		w.event(pBuilt, call.line, st)
	}
}
