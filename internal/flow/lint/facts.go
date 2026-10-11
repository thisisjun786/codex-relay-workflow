package lint

import (
	"fmt"
	"strings"

	"github.com/dop251/goja/ast"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// kind is what a value is known to be, as far as the script's text shows.
type kind int

const (
	kUnknown    kind = iota
	kOutput          // what agent() resolves with, or a CallResult's output
	kCallResult      // what agent.call resolves with
	kHandle          // a CallResult's handle
	kBare            // an object or array literal, a number, null, or a string that is no handle
	kTierStep        // what crw.tierStep resolves with
)

// global reports whether the identifier names a global (it is declared nowhere in the script) called name.
func (a *analysis) global(e ast.Expression, name string) bool {
	id, ok := e.(*ast.Identifier)
	return ok && string(id.Name) == name && a.refs[id] == nil
}

// callee classifies a call expression: agent(...), agent.call(...), crw.<op>(...) and the like.
func (a *analysis) callee(c *ast.CallExpression) (obj, name string) {
	if a.global(c.Callee, "agent") {
		return "agent", ""
	}
	if d, ok := c.Callee.(*ast.DotExpression); ok {
		if a.global(d.Left, "agent") && string(d.Identifier.Name) == "call" {
			return "agent", "call"
		}
		for _, o := range [...]string{"crw", "Date", "Math"} {
			if a.global(d.Left, o) {
				return o, string(d.Identifier.Name)
			}
		}
	}
	return "", ""
}

// exprKind is what the text of an expression says it is.
func (a *analysis) exprKind(e ast.Expression) kind {
	return a.exprKindDepth(e, 0)
}

func (a *analysis) exprKindDepth(e ast.Expression, depth int) kind {
	if depth > 8 {
		return kUnknown
	}
	switch x := e.(type) {
	case *ast.AwaitExpression:
		return a.exprKindDepth(x.Argument, depth+1)
	case *ast.CallExpression:
		switch obj, name := a.callee(x); {
		case obj == "agent" && name == "call":
			return kCallResult
		case obj == "agent":
			return kOutput
		case obj == "crw" && name == "tierStep":
			return kTierStep
		}
	case *ast.Identifier:
		if b := a.refs[x]; b != nil {
			return a.bindingKind(b, depth+1)
		}
	case *ast.DotExpression:
		if a.exprKindDepth(x.Left, depth+1) == kCallResult {
			switch x.Identifier.Name {
			case "handle":
				return kHandle
			case "output":
				return kOutput
			}
			return kBare
		}
	case *ast.ObjectLiteral, *ast.ArrayLiteral, *ast.NumberLiteral, *ast.BooleanLiteral, *ast.NullLiteral:
		return kBare
	case *ast.StringLiteral:
		if strings.HasPrefix(string(x.Value), "crwh1:") {
			return kHandle
		}
		return kBare
	}
	return kUnknown
}

// bindingKind is the kind every value a name is given has, or unknown when they differ or the name is also written some
// way the text does not show.
func (a *analysis) bindingKind(b *binding, depth int) kind {
	if b.opaque || len(b.assigned) == 0 {
		return kUnknown
	}
	k := a.exprKindDepth(b.assigned[0], depth)
	for _, e := range b.assigned[1:] {
		if a.exprKindDepth(e, depth) != k {
			return kUnknown
		}
	}
	return k
}

// staticString is the value of a string literal, an untagged template without substitutions, or a const bound to one.
func (a *analysis) staticString(e ast.Expression) (string, bool) {
	switch x := e.(type) {
	case *ast.StringLiteral:
		return string(x.Value), true
	case *ast.TemplateLiteral:
		if x.Tag == nil && len(x.Expressions) == 0 && len(x.Elements) == 1 && x.Elements[0].Valid {
			return string(x.Elements[0].Parsed), true
		}
	case *ast.Identifier:
		if b := a.refs[x]; b != nil && b.kind == "const" && !b.opaque && len(b.assigned) == 1 {
			if _, isID := b.assigned[0].(*ast.Identifier); !isID {
				return a.staticString(b.assigned[0])
			}
		}
	}
	return "", false
}

// agentCall is one agent() or agent.call(): the options object read as far as it is static.
type agentCall struct {
	node   *ast.CallExpression
	fn     *fnInfo
	plain  bool
	line   int
	prompt ast.Expression
	opts   map[string]ast.Expression
	// spread is set when the options hold a spread or a computed key, so any option may be set.
	spread bool
	// resume is set when the call continues a thread.
	resume bool
	// label is the label when it is static; hasLabel is set when the call states one at all.
	label       string
	hasLabel    bool
	labelStatic bool
	// role is the role the call resolves to when every option that decides it is static; refusal is why it resolves to
	// none.
	role   string
	family string
	// modelFamily is the vendor family of the model the role's pair runs ("claude", "gpt"): what a review's independence
	// is judged on.
	modelFamily string
	refusal     *engine.Refusal
}

// agentFacts reads the options of an agent call.
func (a *analysis) agentFacts(c *ast.CallExpression, fn *fnInfo, policy *engine.Policy) (*agentCall, bool) {
	obj, name := a.callee(c)
	if obj != "agent" {
		return nil, false
	}
	call := &agentCall{node: c, fn: fn, plain: name == "", line: a.lineOf(c), opts: map[string]ast.Expression{}}
	if len(c.ArgumentList) > 0 {
		call.prompt = c.ArgumentList[0]
	}
	if len(c.ArgumentList) > 1 {
		lit, ok := c.ArgumentList[1].(*ast.ObjectLiteral)
		if !ok {
			call.spread = true
		} else {
			for _, p := range lit.Value {
				switch pp := p.(type) {
				case *ast.PropertyKeyed:
					key := ""
					switch k := pp.Key.(type) {
					case *ast.StringLiteral:
						key = string(k.Value)
					case *ast.Identifier:
						key = string(k.Name)
					}
					if pp.Computed || key == "" || pp.Kind != ast.PropertyKindValue {
						call.spread = true
						continue
					}
					call.opts[key] = pp.Value
				case *ast.PropertyShort:
					id := pp.Name
					call.opts[string(id.Name)] = &id
					a.refs[&id] = a.refs[&pp.Name]
				default:
					call.spread = true
				}
			}
		}
	}
	_, call.resume = call.opts["resume"]
	if l, ok := call.opts["label"]; ok {
		call.hasLabel = true
		call.label, call.labelStatic = a.staticString(l)
	}
	if policy != nil && !call.resume && !call.spread {
		a.resolveRole(call, policy)
	}
	return call, true
}

// resolveRole decides the role of a fresh call when its options are static.
func (a *analysis) resolveRole(call *agentCall, policy *engine.Policy) {
	var o engine.CallOpts
	for name, into := range map[string]*string{"agentType": &o.AgentType, "model": &o.Model, "effort": &o.Effort, "isolation": &o.Isolation} {
		e, present := call.opts[name]
		if !present {
			continue
		}
		v, ok := a.staticString(e)
		if !ok {
			return
		}
		*into = v
	}
	r, refusal := policy.ResolveFresh(o)
	if refusal != nil {
		call.refusal = refusal
		return
	}
	call.role, call.family, call.modelFamily = r.Role, r.Spec.Family, execution.ModelFamily(r.Spec.Pair.Model)
}

// resumeFamily is the model family of the thread a resume option continues, as far as the text shows: the thread keeps
// the pair of the call that started it. It is "" when the option is not a handle of a single agent.call whose pair is
// static.
func (a *analysis) resumeFamily(e ast.Expression, policy *engine.Policy, depth int) string {
	if depth > 8 || policy == nil || e == nil {
		return ""
	}
	switch x := e.(type) {
	case *ast.AwaitExpression:
		return a.resumeFamily(x.Argument, policy, depth+1)
	case *ast.DotExpression:
		if x.Identifier.Name == "handle" || x.Identifier.Name == "output" {
			return a.resumeFamily(x.Left, policy, depth+1)
		}
	case *ast.CallExpression:
		call, ok := a.agentFacts(x, nil, policy)
		if !ok {
			return ""
		}
		if call.resume {
			return a.resumeFamily(call.opts["resume"], policy, depth+1)
		}
		return call.modelFamily
	case *ast.Identifier:
		b := a.refs[x]
		if b == nil || b.opaque || len(b.assigned) == 0 {
			return ""
		}
		family := a.resumeFamily(b.assigned[0], policy, depth+1)
		for _, v := range b.assigned[1:] {
			if a.resumeFamily(v, policy, depth+1) != family {
				return ""
			}
		}
		return family
	}
	return ""
}

// nodeKey names the executor node an expression stands for, so two expressions that read the same node get the same
// key: a static string by its value, a name by its binding, a property by its path, anything else by its text. A missing
// expression is "".
func (a *analysis) nodeKey(e ast.Expression) string {
	if e == nil {
		return ""
	}
	if v, ok := a.staticString(e); ok {
		return "s:" + v
	}
	switch x := e.(type) {
	case *ast.Identifier:
		if b := a.refs[x]; b != nil {
			return fmt.Sprintf("b%p", b)
		}
		return "g:" + string(x.Name)
	case *ast.DotExpression:
		return a.nodeKey(x.Left) + "." + string(x.Identifier.Name)
	case *ast.AwaitExpression:
		return a.nodeKey(x.Argument)
	}
	lo, hi := int(e.Idx0())-1, int(e.Idx1())-1
	if lo >= 0 && hi <= len(a.src) && lo <= hi {
		return "x:" + a.src[lo:hi]
	}
	return "?"
}
