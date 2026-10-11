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

// state is what holds on every path that reaches a point of a function body: the set of lifecycle phases each executor
// node may be in, the labels already used, the model family of the last author call, and the names that hold the result
// of crw.tierStep. A state with live unset is a point no path reaches.
type state struct {
	live   bool
	phases map[string]uint8
	labels map[string]bool
	author string
	// steps are the bindings whose value, on every path that reaches the point, comes from a crw.tierStep call made
	// before it on the same function's path.
	steps map[*binding]bool
}

func newState() *state {
	return &state{phases: map[string]uint8{}, labels: map[string]bool{}, steps: map[*binding]bool{}}
}

func (s *state) dead() bool { return !s.live }
func (s *state) kill()      { s.live = false }

func (s *state) clone() *state {
	c := &state{live: s.live, author: s.author, phases: make(map[string]uint8, len(s.phases)), labels: make(map[string]bool, len(s.labels)), steps: make(map[*binding]bool, len(s.steps))}
	for k, v := range s.phases {
		c.phases[k] = v
	}
	for k := range s.labels {
		c.labels[k] = true
	}
	for k := range s.steps {
		c.steps[k] = true
	}
	return c
}

// join merges the state of another path into s: the phases of a node may be any of both, a label is used or a name holds
// a step only when it does on both, and the author is known only when both agree.
func (s *state) join(o *state) {
	switch {
	case o.dead():
		return
	case s.dead():
		*s = *o.clone()
		return
	}
	for k, v := range o.phases {
		s.phases[k] |= v
	}
	for k := range s.labels {
		if !o.labels[k] {
			delete(s.labels, k)
		}
	}
	for k := range s.steps {
		if !o.steps[k] {
			delete(s.steps, k)
		}
	}
	if s.author != o.author {
		s.author = ""
	}
}

func (s *state) equal(o *state) bool {
	if s.live != o.live || s.author != o.author || len(s.phases) != len(o.phases) || len(s.labels) != len(o.labels) || len(s.steps) != len(o.steps) {
		return false
	}
	for k, v := range s.phases {
		if o.phases[k] != v {
			return false
		}
	}
	for k := range s.labels {
		if !o.labels[k] {
			return false
		}
	}
	for k := range s.steps {
		if !o.steps[k] {
			return false
		}
	}
	return true
}

type exit struct {
	breaks, continues *state
	isLoop            bool
}

// tryCtx collects, while the body of a try statement is walked, the states a handler or the finally block can start
// from: thrown are the states after each call (any call may throw), abrupt the states at a return, throw, break or
// continue.
type tryCtx struct {
	thrown, abrupt *state
}

// loopCtx is an enclosing loop whose body is certain to run its calls at least twice: guard is the conditional depth
// at the loop's entry, and a call is certain to repeat only when it is at that depth.
type loopCtx struct {
	certain bool
	guard   int
}

// walker walks the body of one function in evaluation order.
type walker struct {
	s     *scanner
	fn    *fnInfo
	life  bool
	exits []*exit
	tries []*tryCtx
	loops []loopCtx
	// guard is how many conditions (a branch, the right side of a logical operator, a case, a try, a loop that may not
	// run) the point being walked is under, counted from the function's start.
	guard int
	// buildKeys are the node keys a fresh executor call of the function starts; initial is the phase set each node
	// slot starts in.
	buildKeys map[string]bool
	initial   map[string]uint8
}

// flow checks lifecycle, labels and reviewer family over the body of every function.
func (s *scanner) flow() {
	for _, fn := range s.a.order {
		w := &walker{s: s, fn: fn}
		w.life = w.hasLifecycle()
		w.collectNodes()
		start := newState()
		start.live = true
		for k, v := range w.initial {
			start.phases[k] = v
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

// eventNode is the node a crw.commit, crw.verify, crw.review or crw.land call names: its first argument.
func (a *analysis) eventNode(c *ast.CallExpression) string {
	if len(c.ArgumentList) == 0 {
		return ""
	}
	return a.nodeKey(c.ArgumentList[0])
}

// buildNode is the node a fresh executor call starts: its node option ("" when the call states none).
func (a *analysis) buildNode(call *agentCall) string {
	return a.nodeKey(call.opts["node"])
}

// collectNodes finds the executor nodes the function builds and the nodes its lifecycle calls name. Each node has its
// own phase: a node the function builds starts before its build, a node it only names was built elsewhere. A lifecycle
// call whose node is not the node of any build, when some build states no node, belongs to that build.
func (w *walker) collectNodes() {
	w.buildKeys = map[string]bool{}
	w.initial = map[string]uint8{}
	a := w.s.a
	var events []string
	w.walkOwn(func(n any) {
		c, ok := n.(*ast.CallExpression)
		if !ok {
			return
		}
		if obj, name := a.callee(c); obj == "crw" && lifecycleOp(name) != 0 {
			events = append(events, a.eventNode(c))
		} else if call, ok := a.agentFacts(c, w.fn, w.s.policy); ok && call.family == "executor" && !call.resume {
			w.buildKeys[a.buildNode(call)] = true
		}
	})
	for k := range w.buildKeys {
		w.initial[k] = pInit
	}
	for _, k := range events {
		slot := w.slot(k)
		if _, ok := w.initial[slot]; !ok {
			w.initial[slot] = pBuilt
		}
	}
}

// slot is the key a node's phase is kept under.
func (w *walker) slot(key string) string {
	if w.buildKeys[key] || !w.buildKeys[""] {
		return key
	}
	return ""
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

// event applies one lifecycle event of one node. It is reported only when it is illegal in every phase the node may be
// in, so a path the text cannot rule out is not a violation; the node then goes on in the event's phase.
func (w *walker) event(ev uint8, node string, line int, st *state) {
	if st.dead() || !w.life {
		return
	}
	slot := w.slot(node)
	cur, ok := st.phases[slot]
	if !ok {
		cur = pBuilt
	}
	if cur&legalFrom[ev] == 0 {
		switch {
		case ev == pLanded && cur == pLanded:
			w.s.add(RuleLandTwice, line, "", "crw.land runs a second time for the node: land happens at most once")
		case ev == pLanded:
			w.s.add(RuleLandWithoutReview, line, "", "crw.land runs while the node is %s: a land needs a completed review of the committed head (commit, verify, review)", phaseList(cur))
		default:
			w.s.add(RuleLifecycleOrder, line, "", "%s runs while the node is %s: the order is build, then rounds of commit, verify, review, then land", eventNames[ev], phaseList(cur))
		}
	}
	st.phases[slot] = ev
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
	e := &exit{breaks: newState(), continues: newState(), isLoop: loop}
	w.exits = append(w.exits, e)
	return e
}

func (w *walker) popExit() { w.exits = w.exits[:len(w.exits)-1] }

// guarded walks code that may not run.
func (w *walker) guarded(f func()) {
	w.guard++
	f()
	w.guard--
}

// abrupt records the state at a return, throw, break or continue for the finally block of the enclosing try.
func (w *walker) abrupt(st *state) {
	if len(w.tries) > 0 {
		w.tries[len(w.tries)-1].abrupt.join(st)
	}
}

// noteThrow records the state after a call as one a handler may start from.
func (w *walker) noteThrow(st *state) {
	if len(w.tries) > 0 {
		w.tries[len(w.tries)-1].thrown.join(st)
	}
}

// binding walks one declarator: the initializer, then the names it gives a value to.
func (w *walker) binding(b *ast.Binding, st *state) {
	w.expr(b.Initializer, st)
	derived := b.Initializer != nil && w.derives(b.Initializer, st)
	patternNames(b.Target, func(id *ast.Identifier) { w.setStep(id, derived, st) })
}

// setStep records that a name now holds (or no longer holds) a value that comes from crw.tierStep.
func (w *walker) setStep(id *ast.Identifier, holds bool, st *state) {
	bd := w.s.a.refs[id]
	if bd == nil {
		return
	}
	if holds {
		st.steps[bd] = true
	} else {
		delete(st.steps, bd)
	}
}

// derives reports whether an expression is, or reads a name that currently holds, the result of crw.tierStep.
func (w *walker) derives(e ast.Expression, st *state) bool {
	a := w.s.a
	found := false
	var visit func(n any)
	visit = func(n any) {
		if found || n == nil {
			return
		}
		switch x := n.(type) {
		case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
			return
		case *ast.Identifier:
			if b := a.refs[x]; b != nil && st.steps[b] {
				found = true
			}
		case *ast.CallExpression:
			if obj, name := a.callee(x); obj == "crw" && name == "tierStep" {
				found = true
			}
		}
		for _, c := range children(n) {
			visit(c)
		}
	}
	visit(e)
	return found
}

func (w *walker) stmt(n ast.Statement, st *state) {
	switch s := n.(type) {
	case *ast.ExpressionStatement:
		w.expr(s.Expression, st)
	case *ast.VariableStatement:
		for _, b := range s.List {
			w.binding(b, st)
		}
	case *ast.LexicalDeclaration:
		for _, b := range s.List {
			w.binding(b, st)
		}
	case *ast.BlockStatement:
		w.stmts(s.List, st)
	case *ast.ReturnStatement:
		w.expr(s.Argument, st)
		w.abrupt(st)
		st.kill()
	case *ast.ThrowStatement:
		w.expr(s.Argument, st)
		w.abrupt(st)
		st.kill()
	case *ast.IfStatement:
		w.expr(s.Test, st)
		other := st.clone()
		w.guarded(func() {
			w.stmt(s.Consequent, st)
			if s.Alternate != nil {
				w.stmt(s.Alternate, other)
			}
		})
		st.join(other)
	case *ast.LabelledStatement:
		w.stmt(s.Statement, st)
	case *ast.BranchStatement:
		w.abrupt(st)
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
				w.binding(b, st)
			}
		case *ast.ForLoopInitializerLexicalDecl:
			for _, b := range init.LexicalDeclaration.List {
				w.binding(b, st)
			}
		}
		w.loop(s.Test, s.Update, s.Body, s.Test != nil, forRounds(s), st)
	case *ast.WhileStatement:
		w.loop(s.Test, nil, s.Body, !truthy(s.Test), 0, st)
	case *ast.DoWhileStatement:
		// The body runs once before the test.
		once := w.pushExit(true)
		w.stmt(s.Body, st)
		w.popExit()
		st.join(once.continues)
		w.expr(s.Test, st)
		first := st.clone()
		w.loop(s.Test, nil, s.Body, true, 0, first)
		st.join(once.breaks)
		st.join(first)
	case *ast.ForInStatement:
		w.expr(s.Source, st)
		w.loop(nil, nil, s.Body, true, 0, st)
	case *ast.ForOfStatement:
		w.expr(s.Source, st)
		w.loop(nil, nil, s.Body, true, arrayRounds(s.Source), st)
	case *ast.TryStatement:
		w.try(s, st)
	case *ast.SwitchStatement:
		w.expr(s.Discriminant, st)
		pre := st.clone()
		e := w.pushExit(false)
		after := newState()
		var prev *state
		hasDefault := false
		w.guarded(func() {
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
		})
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

// try walks a try statement. A handler may start from the state before the try or after any call in its body. The
// finally block runs once for the paths that leave the body normally (and go on after the statement) and once for the
// paths that leave it abruptly with the state they had at the return, throw, break or continue.
func (w *walker) try(s *ast.TryStatement, st *state) {
	pre := st.clone()
	ctx := &tryCtx{thrown: newState(), abrupt: newState()}
	w.tries = append(w.tries, ctx)
	w.guarded(func() {
		w.stmts(s.Body.List, st)
		if s.Catch != nil {
			c := pre.clone()
			c.join(ctx.thrown)
			if s.Catch.Body != nil {
				w.stmts(s.Catch.Body.List, c)
			}
			st.join(c)
		}
	})
	w.tries = w.tries[:len(w.tries)-1]
	if len(w.tries) > 0 {
		w.tries[len(w.tries)-1].thrown.join(ctx.thrown)
	}
	if s.Finally == nil {
		w.abrupt(ctx.abrupt)
		return
	}
	if !ctx.abrupt.dead() {
		fin := ctx.abrupt.clone()
		w.stmts(s.Finally.List, fin)
		// A return or throw in the finally block overrides the one that reached it; either way the state that
		// leaves the finally block goes on to the enclosing try's own finally block.
		if len(w.tries) > 0 {
			w.tries[len(w.tries)-1].abrupt.join(fin)
		}
	}
	w.stmts(s.Finally.List, st)
}

// leavesEarly reports whether a loop body has a break, a continue, a return or a throw outside the functions nested in
// it: then a round may stop or skip a call, and whether a call in the body repeats is for the dry run to say.
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
			if x.Token == token.BREAK || x.Token == token.CONTINUE {
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

// writesName reports whether a loop body assigns or updates the name.
func writesName(body ast.Statement, name string) bool {
	found := false
	var rec func(n any)
	rec = func(n any) {
		if found {
			return
		}
		switch x := n.(type) {
		case *ast.AssignExpression:
			if id, ok := x.Left.(*ast.Identifier); ok && string(id.Name) == name {
				found = true
				return
			}
			if _, ok := x.Left.(*ast.Identifier); !ok {
				if _, ok := x.Left.(*ast.DotExpression); !ok {
					found = true // a destructuring target may write it
					return
				}
			}
		case *ast.UnaryExpression:
			if id, ok := x.Operand.(*ast.Identifier); ok && string(id.Name) == name && (x.Operator == token.INCREMENT || x.Operator == token.DECREMENT) {
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

func intLiteral(e ast.Expression) (int, bool) {
	n, ok := e.(*ast.NumberLiteral)
	if !ok {
		return 0, false
	}
	switch v := n.Value.(type) {
	case int64:
		return int(v), true
	case float64:
		if v == float64(int(v)) && v > -1e9 && v < 1e9 {
			return int(v), true
		}
	}
	return 0, false
}

// forRounds is how many rounds a counting loop `for (let i = a; i < b; i++)` makes when it is shown by the text: its
// bounds are integer literals, it counts up by one, and its body does not write the counter. 0 is "not shown".
func forRounds(s *ast.ForStatement) int {
	var name string
	var start int
	switch init := s.Initializer.(type) {
	case *ast.ForLoopInitializerLexicalDecl:
		if len(init.LexicalDeclaration.List) != 1 {
			return 0
		}
		b := init.LexicalDeclaration.List[0]
		id, ok := b.Target.(*ast.Identifier)
		v, isInt := intLiteral(b.Initializer)
		if !ok || !isInt {
			return 0
		}
		name, start = string(id.Name), v
	case *ast.ForLoopInitializerVarDeclList:
		if len(init.List) != 1 {
			return 0
		}
		id, ok := init.List[0].Target.(*ast.Identifier)
		v, isInt := intLiteral(init.List[0].Initializer)
		if !ok || !isInt {
			return 0
		}
		name, start = string(id.Name), v
	default:
		return 0
	}
	test, ok := s.Test.(*ast.BinaryExpression)
	if !ok || (test.Operator != token.LESS && test.Operator != token.LESS_OR_EQUAL) {
		return 0
	}
	if id, ok := test.Left.(*ast.Identifier); !ok || string(id.Name) != name {
		return 0
	}
	limit, ok := intLiteral(test.Right)
	if !ok {
		return 0
	}
	counts := false
	switch u := s.Update.(type) {
	case *ast.UnaryExpression:
		id, ok := u.Operand.(*ast.Identifier)
		counts = ok && string(id.Name) == name && u.Operator == token.INCREMENT
	case *ast.AssignExpression:
		id, ok := u.Left.(*ast.Identifier)
		step, isInt := intLiteral(u.Right)
		counts = ok && string(id.Name) == name && u.Operator == token.ADD_ASSIGN && isInt && step == 1
	}
	if !counts || writesName(s.Body, name) {
		return 0
	}
	rounds := limit - start
	if test.Operator == token.LESS_OR_EQUAL {
		rounds++
	}
	return max(rounds, 0)
}

// arrayRounds is the length of an array literal a for-of loop walks, when the literal is plain (no hole, no spread).
func arrayRounds(e ast.Expression) int {
	lit, ok := e.(*ast.ArrayLiteral)
	if !ok {
		return 0
	}
	for _, el := range lit.Value {
		if el == nil {
			return 0
		}
		if _, spread := el.(*ast.SpreadElement); spread {
			return 0
		}
	}
	return len(lit.Value)
}

// truthy reports whether a loop test is the literal true (a loop that exits only through break or return).
func truthy(e ast.Expression) bool {
	b, ok := e.(*ast.BooleanLiteral)
	return ok && b.Value
}

// loop walks a loop body to a fixed point: the state at the head is the entry state joined with every state that comes
// round again. endless is set for a loop with no test or a constant true one, which is left only through break or return.
// rounds is the number of rounds the text shows the loop makes (0 when it does not); a loop with at least two rounds and
// no way out of a round early runs every call of its body at least twice.
func (w *walker) loop(test, update ast.Expression, body ast.Statement, hasTest bool, rounds int, st *state) {
	head := st.clone()
	breaks := newState()
	certain := rounds >= 2 && !leavesEarly(body)
	w.loops = append(w.loops, loopCtx{certain: certain, guard: w.guard})
	if !certain {
		w.guard++
	}
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
	if !certain {
		w.guard--
	}
	w.loops = w.loops[:len(w.loops)-1]
	exitState := newState()
	if hasTest {
		exitState = head.clone()
		w.expr(test, exitState)
	}
	exitState.join(breaks)
	*st = *exitState
}

// repeats reports whether the point being walked is, by the text, run at least twice with the same label: it is in a
// loop of at least two rounds, under no condition inside it.
func (w *walker) repeats() bool {
	for _, l := range w.loops {
		if l.certain && l.guard == w.guard {
			return true
		}
	}
	return false
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
		w.guarded(func() {
			w.expr(e.Consequent, st)
			w.expr(e.Alternate, other)
		})
		st.join(other)
		return
	case *ast.BinaryExpression:
		if e.Operator == token.LOGICAL_AND || e.Operator == token.LOGICAL_OR || e.Operator == token.COALESCE {
			w.expr(e.Left, st)
			other := st.clone()
			w.guarded(func() { w.expr(e.Right, other) })
			st.join(other)
			return
		}
	case *ast.AssignExpression:
		w.expr(e.Right, st)
		switch left := e.Left.(type) {
		case *ast.Identifier:
			if e.Operator == token.ASSIGN {
				w.setStep(left, w.derives(e.Right, st), st)
			}
		case *ast.ArrayPattern, *ast.ObjectPattern:
			if e.Operator == token.ASSIGN {
				holds := w.derives(e.Right, st)
				patternNames(left, func(id *ast.Identifier) { w.setStep(id, holds, st) })
			}
		default:
			w.expr(e.Left, st)
		}
		return
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
		w.event(lifecycleOp(name), a.eventNode(c), line, st)
	case obj == "agent":
		call, _ := a.agentFacts(c, w.fn, w.s.policy)
		w.agent(call, st)
	}
	w.noteThrow(st)
}

func (w *walker) agent(call *agentCall, st *state) {
	a := w.s.a
	if call.hasLabel && call.labelStatic {
		switch {
		case st.labels[call.label]:
			w.s.add(RuleLabelDuplicate, call.line, call.label, "label %q is used by an earlier call on this path; labels are unique within a path", call.label)
		case w.repeats():
			w.s.add(RuleLabelDuplicate, call.line, call.label, "label %q is on a call that a loop of at least two rounds runs every round, so every round repeats it; labels are unique within a path (put the round in the label)", call.label)
		}
		st.labels[call.label] = true
	}
	if call.role == "executor-t1" && !call.resume {
		if sd, ok := call.opts["stepDecision"]; ok && !w.derives(sd, st) {
			w.s.add(RuleTierStepDecision, call.line, call.label, "the stepDecision of an executor-t1 call must be the id crw.tierStep returned on the same path, earlier in this function and not overwritten since; this one does not come from a crw.tierStep call that reaches it")
		}
	}
	if a.reviewCall(call) {
		family := call.modelFamily
		if call.resume {
			family = a.resumeFamily(call.opts["resume"], w.s.policy, 0)
		}
		if st.author != "" && family != "" && family == st.author {
			w.s.add(RuleReviewerFamily, call.line, call.label, "a review call runs on the %s model family, the family of the call that wrote the change; a review comes from another model family than the author's", family)
		}
		return
	}
	if call.role == "" || call.resume {
		return
	}
	if call.family == "executor" {
		st.author = call.modelFamily
		w.event(pBuilt, a.buildNode(call), call.line, st)
	}
}
