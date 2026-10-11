package lint

import (
	"fmt"
	"strings"

	"github.com/dop251/goja/ast"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// Rule names. A diagnostic carries exactly one.
const (
	RuleLoad              = "load"
	RuleParse             = "parse"
	RuleLandTwice         = "lifecycle_land_twice"
	RuleLandWithoutReview = "lifecycle_land_without_review"
	RuleLifecycleOrder    = "lifecycle_order"
	RuleReviewerFamily    = "reviewer_family_same"
	RuleLabelDuplicate    = "label_duplicate"
	RuleTierStepDecision  = "executor_t1_without_step_decision"
	RuleClassifyArg       = "classify_arg"
	RuleResumeArg         = "resume_arg"
	RuleDeterminism       = "determinism_stub"
	RuleLaneMutableLet    = "lane_reads_mutable_let"
	RuleAliasAmbiguous    = "alias_ambiguous"
	RuleCallRefused       = "call_refused"
	RulePathConcurrency   = "path_concurrency"
)

// Sources of a diagnostic.
const (
	sourceStatic = "static"
	sourceTrace  = "trace"
)

// Diagnostic is one finding. Line is 0 for a finding the dry-run trace made, which has no line.
type Diagnostic struct {
	Rule    string `json:"rule"`
	Line    int    `json:"line,omitempty"`
	Label   string `json:"label,omitempty"`
	Message string `json:"message"`
	Source  string `json:"source"`
}

func (d Diagnostic) String() string {
	where := ""
	if d.Line > 0 {
		where = fmt.Sprintf("line %d: ", d.Line)
	}
	return fmt.Sprintf("%s%s: %s (%s)", where, d.Rule, d.Message, d.Source)
}

// scanner holds the static checks that need no control flow: they read single calls.
type scanner struct {
	a      *analysis
	policy *engine.Policy
	out    *[]Diagnostic
}

func (s *scanner) add(rule string, line int, label, format string, args ...any) {
	*s.out = append(*s.out, Diagnostic{Rule: rule, Line: line, Label: label, Message: fmt.Sprintf(format, args...), Source: sourceStatic})
}

// scan visits every node of the script once, with the function it belongs to.
func (s *scanner) scan(n any, fn *fnInfo) {
	switch x := n.(type) {
	case nil:
		return
	case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
		if f := s.a.fns[x]; f != nil {
			fn = f
		}
	case *ast.CallExpression:
		s.call(x, fn)
	case *ast.NewExpression:
		if s.a.global(x.Callee, "Date") && len(x.ArgumentList) == 0 {
			s.add(RuleDeterminism, s.a.lineOf(x), "", "new Date() reads the wall clock, which a CRW script cannot: read time from args or crw.clock()")
		}
	}
	for _, c := range children(n) {
		s.scan(c, fn)
	}
}

func (s *scanner) call(c *ast.CallExpression, fn *fnInfo) {
	a := s.a
	line := a.lineOf(c)
	if a.global(c.Callee, "Date") {
		s.add(RuleDeterminism, line, "", "Date() reads the wall clock, which a CRW script cannot: read time from args or crw.clock()")
	}
	obj, name := a.callee(c)
	switch {
	case obj == "Date" && name == "now":
		s.add(RuleDeterminism, line, "", "Date.now() reads the wall clock, which a CRW script cannot: read time from args or crw.clock()")
	case obj == "Math" && name == "random":
		s.add(RuleDeterminism, line, "", "Math.random() is not repeatable, which a CRW script must be: derive the value from args or the journal")
	case obj == "crw" && name == "classify":
		if len(c.ArgumentList) > 0 {
			s.checkResult(c.ArgumentList[0], RuleClassifyArg, "crw.classify", line)
		}
	case obj == "crw" && name == "dag":
		s.dag(c)
	case obj == "agent":
		call, _ := a.agentFacts(c, fn, s.policy)
		s.agent(call)
	}
}

// checkResult judges a value that must be a CallResult agent.call returned or its handle.
func (s *scanner) checkResult(e ast.Expression, rule, what string, line int) {
	switch s.a.exprKind(e) {
	case kOutput:
		s.add(rule, line, "", "%s takes the CallResult agent.call returned or its handle, and this is an output: agent() resolves with the output, so call agent.call and pass its result or its .handle", what)
	case kBare:
		s.add(rule, line, "", "%s takes the CallResult agent.call returned or its handle, and this is a bare value (an object, a number, null, or a string that is no handle)", what)
	}
}

func (s *scanner) agent(call *agentCall) {
	if r, ok := call.opts["resume"]; ok {
		s.checkResult(r, RuleResumeArg, "agent.call resume", call.line)
	}
	if call.refusal != nil && call.refusal.Cause == "alias_ambiguous" {
		s.add(RuleAliasAmbiguous, call.line, call.label, "%s", strings.TrimPrefix(call.refusal.Detail, "alias_ambiguous: "))
	}
	// An executor-t1 call carries the stepDecision crw.tierStep returned on its own path. A continuation inherits it.
	// That the stepDecision it states is that id, still, is judged along the function's paths (walker.agent).
	if call.role == "executor-t1" && !call.resume {
		if _, ok := call.opts["stepDecision"]; !ok {
			s.add(RuleTierStepDecision, call.line, call.label, "an executor-t1 call needs stepDecision: the id crw.tierStep returned on its path (a fresh executor-t1 thread starts only after a recorded step decision)")
		}
	}
}

// dag checks the lane function and the onSignal function of crw.dag: they read no outer let that is assigned again.
func (s *scanner) dag(c *ast.CallExpression) {
	if len(c.ArgumentList) > 1 {
		s.laneFunction(c.ArgumentList[1], "lane")
	}
	for _, arg := range c.ArgumentList {
		lit, ok := arg.(*ast.ObjectLiteral)
		if !ok {
			continue
		}
		for _, p := range lit.Value {
			switch pp := p.(type) {
			case *ast.PropertyKeyed:
				if k, ok := pp.Key.(*ast.Identifier); ok && !pp.Computed && k.Name == "onSignal" {
					s.laneFunction(pp.Value, "onSignal")
				}
				if k, ok := pp.Key.(*ast.StringLiteral); ok && !pp.Computed && k.Value == "onSignal" {
					s.laneFunction(pp.Value, "onSignal")
				}
			case *ast.PropertyShort:
				if pp.Name.Name == "onSignal" {
					s.laneFunction(&pp.Name, "onSignal")
				}
			}
		}
	}
}

// laneFunction finds the function an expression names or is and reports the outer lets it reads.
func (s *scanner) laneFunction(e ast.Expression, what string) {
	a := s.a
	var target any
	switch x := e.(type) {
	case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
		target = x
	case *ast.Identifier:
		if b := a.refs[x]; b != nil {
			target = b.decl
		}
	}
	f := a.fns[target]
	if f == nil {
		return
	}
	reported := map[string]bool{}
	for _, r := range f.freeReads {
		if r.b.kind != "let" || !r.b.reassigned || reported[r.b.name] {
			continue
		}
		reported[r.b.name] = true
		s.add(RuleLaneMutableLet, r.line, "", "the %s function of crw.dag reads `%s`, an outer let binding that is assigned again (line %d); a lane reads its facts from the journal, not from a variable another path may have changed", what, r.b.name, r.b.line)
	}
}

// reviewCall reports whether an agent call reviews work: its prompt is a crw.reviewPacket(...) or its label starts with
// "review".
func (a *analysis) reviewCall(call *agentCall) bool {
	if p, ok := call.prompt.(*ast.CallExpression); ok {
		if obj, name := a.callee(p); obj == "crw" && name == "reviewPacket" {
			return true
		}
	}
	return call.hasLabel && call.labelStatic && strings.HasPrefix(strings.ToLower(call.label), "review")
}
