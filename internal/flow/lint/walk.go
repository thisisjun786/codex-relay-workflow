package lint

import (
	"slices"
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
	// steps are the bindings of the function whose value, on every path that reaches the point, comes from a
	// crw.tierStep call made before it on the same path, or from a parameter (what the caller passes is for the dry run
	// to judge).
	steps map[*binding]bool
	// pend are the lifecycle calls whose promise a name of the function holds and the path has not read yet: the call has
	// run, and the node reaches its phase only when the name is read (awaited, passed on, returned).
	pend map[*binding]pending
}

// pending is a lifecycle event that has been invoked and not yet shown to complete: the slot of its node and its phase.
type pending struct {
	slot string
	ev   uint8
}

func newState() *state {
	return &state{phases: map[string]uint8{}, labels: map[string]bool{}, steps: map[*binding]bool{}, pend: map[*binding]pending{}}
}

func (s *state) dead() bool { return !s.live }
func (s *state) kill()      { s.live = false }

func (s *state) clone() *state {
	c := &state{live: s.live, author: s.author, phases: make(map[string]uint8, len(s.phases)), labels: make(map[string]bool, len(s.labels)), steps: make(map[*binding]bool, len(s.steps)), pend: make(map[*binding]pending, len(s.pend))}
	for k, v := range s.pend {
		c.pend[k] = v
	}
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
	for k, v := range o.pend {
		if _, ok := s.pend[k]; !ok {
			s.pend[k] = v
		}
	}
	if s.author != o.author {
		s.author = ""
	}
}

func (s *state) equal(o *state) bool {
	if s.live != o.live || s.author != o.author || len(s.phases) != len(o.phases) || len(s.labels) != len(o.labels) || len(s.steps) != len(o.steps) || len(s.pend) != len(o.pend) {
		return false
	}
	for k, v := range s.pend {
		if o.pend[k] != v {
			return false
		}
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

// exit is a loop, a switch or a labeled statement a break (or, for a loop, a continue) can go to. names are its labels;
// a labeled statement that is no loop or switch (labelOnly) is the target of a labeled break only.
type exit struct {
	breaks, continues *state
	isLoop            bool
	labelOnly         bool
	names             []string
}

// tryCtx collects, while a try statement is walked, the states that leave its body or handler abruptly, by where they
// go: throws are the states after each call (any call may throw) and at a throw statement, returns the states at a
// return, and jumps the states at a break or continue whose loop or switch encloses the try statement. exitDepth is the
// number of enclosing exits when the statement starts: a break or continue to one of those leaves the statement.
type tryCtx struct {
	exitDepth       int
	throws, returns *state
	jumps           []*pendingJump
}

// pendingJump is a break (or continue) to the exit at index exit that waits for the finally block.
type pendingJump struct {
	exit int
	cont bool
	st   *state
}

func (c *tryCtx) jump(exit int, cont bool) *state {
	for _, j := range c.jumps {
		if j.exit == exit && j.cont == cont {
			return j.st
		}
	}
	j := &pendingJump{exit: exit, cont: cont, st: newState()}
	c.jumps = append(c.jumps, j)
	return j.st
}

// loopCtx is an enclosing loop whose body is certain to run its calls at least twice: guard is the conditional depth
// at the loop's entry, and a call is certain to repeat only when it is at that depth.
type loopCtx struct {
	certain bool
	guard   int
}

// walker walks the body of one function in evaluation order.
type walker struct {
	s  *scanner
	fn *fnInfo
	// lanes are the functions that run on a child path of the function that creates them: the functions in the
	// arguments of parallel, pipeline and crw.dag.
	lanes map[*fnInfo]bool
	life  bool
	exits []*exit
	tries []*tryCtx
	loops []loopCtx
	// guard is how many conditions (a branch, the right side of a logical operator, a case, a try, a loop that may not
	// run) the point being walked is under, counted from the function's start.
	guard int
	// pending are the labels of the labeled statement being entered, for the loop or switch it labels to take.
	pending []string
	// deferred are the lifecycle calls whose promise the text does not await where it is made.
	deferred map[*ast.CallExpression]*deferral
	// buildKeys are the node keys a fresh executor call of the function starts; initial is the phase set each node
	// slot starts in.
	buildKeys map[string]bool
	initial   map[string]uint8
}

// flow checks lifecycle, labels and reviewer family over the body of every function.
func (s *scanner) flow() {
	lanes := s.laneFunctions()
	for _, fn := range s.a.order {
		w := &walker{s: s, fn: fn, lanes: lanes, deferred: map[*ast.CallExpression]*deferral{}}
		w.life = w.hasLifecycle()
		w.collectNodes()
		start := newState()
		start.live = true
		for k, v := range w.initial {
			start.phases[k] = v
		}
		for _, b := range fn.scope.vars {
			if b.kind == "param" {
				start.steps[b] = true
			}
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

// laneFunctions finds the functions that run on a child path: every function in the arguments of a parallel, pipeline
// or crw.dag call.
func (s *scanner) laneFunctions() map[*fnInfo]bool {
	a := s.a
	lanes := map[*fnInfo]bool{}
	var mark func(n any)
	mark = func(n any) {
		if f := a.fns[n]; f != nil {
			lanes[f] = true
		}
		for _, c := range children(n) {
			mark(c)
		}
	}
	var find func(n any)
	find = func(n any) {
		if c, ok := n.(*ast.CallExpression); ok {
			obj, name := a.callee(c)
			if a.global(c.Callee, "parallel") || a.global(c.Callee, "pipeline") || obj == "crw" && name == "dag" {
				for _, arg := range c.ArgumentList {
					mark(arg)
				}
			}
		}
		for _, c := range children(n) {
			find(c)
		}
	}
	find(a.prog)
	return lanes
}

// crossesLane reports whether a function between the walked one and owner (the walked one included) runs on a child
// path: then a name of owner holds a value of another path.
func (w *walker) crossesLane(owner *fnInfo) bool {
	for f := w.fn; f != nil && f != owner; f = f.parent {
		if w.lanes[f] {
			return true
		}
	}
	return false
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

// deferral says a lifecycle call's promise is not awaited where the call is made: it is dropped (id is nil), or held by
// a name that has to be read before the event is complete.
type deferral struct{ bd *binding }

// lifecycleCall is the crw.commit, crw.verify, crw.review or crw.land call an expression is, if it is one.
func (w *walker) lifecycleCall(e ast.Expression) *ast.CallExpression {
	c, ok := e.(*ast.CallExpression)
	if !ok {
		return nil
	}
	if obj, name := w.s.a.callee(c); obj == "crw" && lifecycleOp(name) != 0 {
		return c
	}
	return nil
}

// complete marks the event a name's promise stands for as done: the node is in its phase.
func (w *walker) complete(bd *binding, st *state) {
	if p, ok := st.pend[bd]; ok {
		delete(st.pend, bd)
		st.phases[p.slot] = p.ev
	}
}

// completeIn completes every pending event whose name appears in n (a function literal may read it when it runs).
func (w *walker) completeIn(n any, st *state) {
	if len(st.pend) == 0 {
		return
	}
	var visit func(n any)
	visit = func(n any) {
		if id, ok := n.(*ast.Identifier); ok {
			if bd := w.s.a.refs[id]; bd != nil {
				w.complete(bd, st)
			}
		}
		for _, c := range children(n) {
			visit(c)
		}
	}
	visit(n)
}

// event applies one lifecycle event of one node. It is reported only when it is illegal in every phase the node may be
// in, so a path the text cannot rule out is not a violation; the node then goes on in the event's phase. The call's
// promise has to settle for the phase to be reached: a call whose promise is dropped never gets the node there, and a
// call held by a name gets it there where the name is read (anywhere the text can read it counts: the lint does not
// follow the promise further).
func (w *walker) event(ev uint8, node string, line int, st *state, def *deferral) {
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
	switch {
	case def == nil:
		st.phases[slot] = ev
	case def.bd != nil:
		st.pend[def.bd] = pending{slot: slot, ev: ev}
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

func (w *walker) pushExit(loop bool, names []string) *exit {
	e := &exit{breaks: newState(), continues: newState(), isLoop: loop, names: names}
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

// noteThrow records a state an exception may leave from (after a call, at a throw statement) for the handler or the
// finally block of the innermost try.
func (w *walker) noteThrow(st *state) {
	if len(w.tries) > 0 {
		w.tries[len(w.tries)-1].throws.join(st)
	}
}

// noteReturn records the state at a return for the finally block of the innermost try.
func (w *walker) noteReturn(st *state) {
	if len(w.tries) > 0 {
		w.tries[len(w.tries)-1].returns.join(st)
	}
}

// jumpTo delivers the state at a break or continue to the exit at index i: through the finally block of the innermost
// try when the jump leaves that try, else to the exit itself.
func (w *walker) jumpTo(i int, cont bool, st *state) {
	if n := len(w.tries); n > 0 && w.tries[n-1].exitDepth > i {
		w.tries[n-1].jump(i, cont).join(st)
		return
	}
	if cont {
		w.exits[i].continues.join(st)
	} else {
		w.exits[i].breaks.join(st)
	}
}

// binding walks one declarator: the initializer, then the names it gives a value to.
func (w *walker) binding(b *ast.Binding, st *state) {
	if id, ok := b.Target.(*ast.Identifier); ok {
		w.deferTo(b.Initializer, id)
	}
	w.expr(b.Initializer, st)
	derived := b.Initializer != nil && w.derives(b.Initializer, st)
	patternNames(b.Target, func(id *ast.Identifier) { w.setStep(id, derived, st) })
}

// deferTo notes that a lifecycle call held by a name is complete where the name is read.
func (w *walker) deferTo(e ast.Expression, id *ast.Identifier) {
	if c := w.lifecycleCall(e); c != nil {
		w.deferred[c] = &deferral{bd: w.s.a.refs[id]}
	}
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

// derives reports whether an expression is, or reads a name that currently holds, the result of crw.tierStep. A name of
// an enclosing function holds what that function gave it, which a plain call leaves on the same path: the text does
// not show what it holds when the call runs, so it counts, and the dry run judges the id. Across a parallel, pipeline
// or crw.dag function it is a value of another path and does not count.
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
			if b := a.refs[x]; b != nil && (st.steps[b] || b.owner != w.fn && b.owner != nil && !w.crossesLane(b.owner)) {
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
	labels := w.pending
	w.pending = nil
	switch s := n.(type) {
	case *ast.ExpressionStatement:
		// A lifecycle call used as a statement (or voided) drops its promise.
		e := s.Expression
		if u, ok := e.(*ast.UnaryExpression); ok && u.Operator == token.VOID {
			e = u.Operand
		}
		if c := w.lifecycleCall(e); c != nil {
			w.deferred[c] = &deferral{}
		}
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
		w.noteReturn(st)
		st.kill()
	case *ast.ThrowStatement:
		w.expr(s.Argument, st)
		w.noteThrow(st)
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
		names := append(append([]string(nil), labels...), string(s.Label.Name))
		switch s.Statement.(type) {
		case *ast.LabelledStatement, *ast.ForStatement, *ast.WhileStatement, *ast.DoWhileStatement, *ast.ForInStatement, *ast.ForOfStatement, *ast.SwitchStatement:
			// The loop or switch takes the labels: a break or continue to one of them is its own.
			w.pending = names
			w.stmt(s.Statement, st)
		default:
			// Any other labeled statement is left by a break to its label.
			e := w.pushExit(false, names)
			e.labelOnly = true
			w.stmt(s.Statement, st)
			w.popExit()
			st.join(e.breaks)
		}
	case *ast.BranchStatement:
		cont := s.Token == token.CONTINUE
		for i := len(w.exits) - 1; i >= 0; i-- {
			e := w.exits[i]
			if s.Label != nil {
				if !slices.Contains(e.names, string(s.Label.Name)) {
					continue
				}
			} else if e.labelOnly {
				continue
			}
			if cont && !e.isLoop {
				continue
			}
			w.jumpTo(i, cont, st)
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
		w.loop(s.Test, s.Update, s.Body, s.Test != nil, forRounds(s), labels, st)
	case *ast.WhileStatement:
		w.loop(s.Test, nil, s.Body, !truthy(s.Test), 0, labels, st)
	case *ast.DoWhileStatement:
		w.doWhile(s, labels, st)
	case *ast.ForInStatement:
		w.expr(s.Source, st)
		w.loop(nil, nil, s.Body, true, 0, labels, st)
	case *ast.ForOfStatement:
		w.expr(s.Source, st)
		w.loop(nil, nil, s.Body, true, arrayRounds(s.Source), labels, st)
	case *ast.TryStatement:
		w.try(s, st)
	case *ast.SwitchStatement:
		w.expr(s.Discriminant, st)
		pre := st.clone()
		e := w.pushExit(false, labels)
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

// try walks a try statement. A handler may start from the state before the try or from any state an exception leaves
// the body with; it takes those exceptions, and only what it throws itself goes on. The finally block runs once for
// the paths that end the body or the handler normally (and go on after the statement) and once for each kind of abrupt
// completion that is still pending (a throw, a return, a break or continue to each exit), from the state that
// completion had; when the block ends normally the completion goes on where it was going with the block's state, and
// a return, throw, break or continue of the block itself replaces it.
func (w *walker) try(s *ast.TryStatement, st *state) {
	pre := st.clone()
	ctx := &tryCtx{exitDepth: len(w.exits), throws: newState(), returns: newState()}
	w.tries = append(w.tries, ctx)
	w.guarded(func() {
		w.stmts(s.Body.List, st)
		if s.Catch != nil {
			c := pre.clone()
			c.join(ctx.throws)
			ctx.throws = newState()
			if s.Catch.Body != nil {
				w.stmts(s.Catch.Body.List, c)
			}
			st.join(c)
		}
	})
	w.tries = w.tries[:len(w.tries)-1]
	resume := func(pending *state, goOn func(*state)) {
		if pending.dead() {
			return
		}
		fin := pending.clone()
		if s.Finally != nil {
			w.stmts(s.Finally.List, fin)
		}
		if !fin.dead() {
			goOn(fin)
		}
	}
	resume(ctx.throws, w.noteThrow)
	resume(ctx.returns, w.noteReturn)
	for _, j := range ctx.jumps {
		resume(j.st, func(fin *state) { w.jumpTo(j.exit, j.cont, fin) })
	}
	if s.Finally != nil {
		w.stmts(s.Finally.List, st)
	}
}

// doWhile walks a do-while loop to a fixed point. The body runs once before the test, so the state after the loop is
// one the body has run on; whether it runs again is the test's, so a label the first round used is not repeated by the
// text unless the loop is endless and has no way out of a round early.
func (w *walker) doWhile(s *ast.DoWhileStatement, labels []string, st *state) {
	certain := truthy(s.Test) && !leavesEarly(s.Body)
	w.loops = append(w.loops, loopCtx{certain: certain, guard: w.guard})
	entry := st.clone()
	head := entry.clone()
	breaks := newState()
	var out *state
	for i := 0; i < 4; i++ {
		cur := head.clone()
		e := w.pushExit(true, labels)
		w.stmt(s.Body, cur)
		w.popExit()
		cur.join(e.continues)
		w.expr(s.Test, cur)
		breaks.join(e.breaks)
		out = cur
		next := entry.clone()
		next.join(cur)
		if next.equal(head) {
			break
		}
		head = next
	}
	w.loops = w.loops[:len(w.loops)-1]
	out.join(breaks)
	*st = *out
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
func (w *walker) loop(test, update ast.Expression, body ast.Statement, hasTest bool, rounds int, labels []string, st *state) {
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
		e := w.pushExit(true, labels)
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
		w.completeIn(e, st)
		return
	case *ast.Identifier:
		if bd := w.s.a.refs[e]; bd != nil {
			w.complete(bd, st)
		}
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
		if id, ok := e.Left.(*ast.Identifier); ok && e.Operator == token.ASSIGN {
			w.deferTo(e.Right, id)
		}
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
		w.event(lifecycleOp(name), a.eventNode(c), line, st, w.deferred[c])
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
			w.s.add(RuleTierStepDecision, call.line, call.label, "the stepDecision of an executor-t1 call must be the id crw.tierStep returned on the same path before it and not overwritten since; this one does not come from a crw.tierStep call that reaches it (a decision of the path that starts a parallel, pipeline or crw.dag function is not the path that function runs on)")
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
		w.event(pBuilt, a.buildNode(call), call.line, st, nil)
	}
}
