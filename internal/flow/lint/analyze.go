package lint

import (
	"reflect"
	"sort"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

// wrapperHead opens the function the script's body is wrapped in; the body keeps its own line numbers because the
// header is replaced by newlines in script.Program.Body and the head shares the first line.
const wrapperHead = "(async function __crw_main(){ "

var astPkg = reflect.TypeOf(ast.Program{}).PkgPath()

func isASTPtr(t reflect.Type) bool {
	return t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Struct && t.Elem().PkgPath() == astPkg
}

// children are the AST nodes a node holds directly, in the order of its fields (which is source order for every node
// that matters to a rule; control flow nodes are handled by name). Function declaration lists and the file are not
// children.
func children(n any) []any {
	v := reflect.ValueOf(n)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	var out []any
	add := func(f reflect.Value) {
		if f.Kind() != reflect.Interface && f.Kind() != reflect.Ptr {
			return
		}
		if f.IsNil() {
			return
		}
		e := f
		if f.Kind() == reflect.Interface {
			e = f.Elem()
		}
		if isASTPtr(e.Type()) {
			out = append(out, e.Interface())
		}
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		switch t.Field(i).Name {
		case "DeclarationList", "File":
			continue
		}
		f := v.Field(i)
		if f.Kind() == reflect.Slice {
			for k := 0; k < f.Len(); k++ {
				add(f.Index(k))
			}
			continue
		}
		add(f)
	}
	return out
}

// binding is a name declared in the script.
type binding struct {
	name  string
	kind  string // let, const, var, param, function, class, catch
	owner *fnInfo
	// assigned are the expressions the name is given: its initializer and the right side of every plain assignment;
	// opaque is set when it is also written some other way (a compound assignment, a destructuring, a loop).
	assigned []ast.Expression
	opaque   bool
	// reassigned is set when the name is assigned after its declaration; line is the first such assignment.
	reassigned bool
	line       int
	// decl is the function literal a function declaration or a const function expression holds.
	decl any
}

type scope struct {
	parent *scope
	fn     *fnInfo
	vars   map[string]*binding
}

type read struct {
	b    *binding
	line int
}

// fnInfo is one function of the script: the wrapped main body, a function, an arrow, a method.
type fnInfo struct {
	node      any
	parent    *fnInfo
	scope     *scope
	freeReads []read
	// life is set when the function makes a lifecycle call itself (callEvent): crw.commit, crw.verify, crw.review,
	// crw.land, crw.unitCheck or a land release.
	life bool
}

// analysis is a parsed script with its scopes resolved.
type analysis struct {
	src    string
	lines  []int // offsets of the starts of lines
	prog   *ast.Program
	main   *fnInfo
	fns    map[any]*fnInfo
	scopes map[any]*scope
	refs   map[*ast.Identifier]*binding
	order  []*fnInfo
}

// parse parses the wrapped body and resolves every identifier to its declaration. The names the header imports are
// declared on the wrapper's first line (the executable wrapper declares them as consts there), so the body's lines
// are unchanged and a reference to an import resolves to a script binding, not to an engine global.
func parse(body string, imports []string) (*analysis, error) {
	decl := ""
	if len(imports) > 0 {
		decl = "let " + strings.Join(imports, ", ") + "; "
	}
	src := wrapperHead + decl + body + "\n})"
	prog, err := parser.ParseFile(nil, "script.js", src, 0)
	if err != nil {
		return nil, err
	}
	a := &analysis{src: src, prog: prog, fns: map[any]*fnInfo{}, scopes: map[any]*scope{}, refs: map[*ast.Identifier]*binding{}}
	off := 0
	a.lines = append(a.lines, 0)
	for {
		i := strings.IndexByte(src[off:], '\n')
		if i < 0 {
			break
		}
		off += i + 1
		a.lines = append(a.lines, off)
	}
	root := &scope{vars: map[string]*binding{}}
	for _, st := range prog.Body {
		a.declare(st, root, nil)
	}
	for _, st := range prog.Body {
		a.resolve(st, root, nil)
	}
	a.main = a.fns[firstFunction(prog)]
	return a, nil
}

// firstFunction is the wrapper function literal.
func firstFunction(prog *ast.Program) any {
	var found any
	var visit func(n any) bool
	visit = func(n any) bool {
		if f, ok := n.(*ast.FunctionLiteral); ok {
			found = f
			return true
		}
		for _, c := range children(n) {
			if visit(c) {
				return true
			}
		}
		return false
	}
	for _, st := range prog.Body {
		if visit(st) {
			break
		}
	}
	return found
}

// lineAt is the 1-based line of a 1-based source offset.
func (a *analysis) lineAt(offset int) int {
	offset--
	if offset < 0 {
		offset = 0
	}
	return sort.Search(len(a.lines), func(i int) bool { return a.lines[i] > offset })
}

func (a *analysis) lineOf(n ast.Node) int { return a.lineAt(int(n.Idx0())) }

func (sc *scope) lookup(name string) *binding {
	for s := sc; s != nil; s = s.parent {
		if b, ok := s.vars[name]; ok {
			return b
		}
	}
	return nil
}

func (sc *scope) fnScope() *scope {
	s := sc
	for s.parent != nil && s.fn == s.parent.fn {
		s = s.parent
	}
	return s
}

func (a *analysis) newFn(node any, parent *fnInfo, sc *scope) *fnInfo {
	f := &fnInfo{node: node, parent: parent}
	f.scope = &scope{parent: sc, fn: f, vars: map[string]*binding{}}
	a.fns[node] = f
	a.order = append(a.order, f)
	return f
}

func (a *analysis) declName(sc *scope, name, kind string, fn *fnInfo) *binding {
	if existing, ok := sc.vars[name]; ok && kind == "var" {
		return existing
	}
	b := &binding{name: name, kind: kind, owner: fn}
	sc.vars[name] = b
	return b
}

// patternNames calls add for each identifier a binding target declares.
func patternNames(t any, add func(id *ast.Identifier)) {
	switch p := t.(type) {
	case *ast.Identifier:
		add(p)
	case *ast.ArrayPattern:
		for _, e := range p.Elements {
			patternNames(e, add)
		}
		patternNames(p.Rest, add)
	case *ast.ObjectPattern:
		for _, prop := range p.Properties {
			switch pp := prop.(type) {
			case *ast.PropertyShort:
				add(&pp.Name)
			case *ast.PropertyKeyed:
				patternNames(pp.Value, add)
			}
		}
		patternNames(p.Rest, add)
	case *ast.AssignExpression:
		patternNames(p.Left, add)
	case *ast.SpreadElement:
		patternNames(p.Expression, add)
	}
}

// declareBinding declares the names of one binding target; a plain identifier target remembers its initializer.
func (a *analysis) declareBinding(b *ast.Binding, kind string, sc *scope, fn *fnInfo) {
	target := sc
	if kind == "var" {
		target = sc.fnScope()
	}
	_, simple := b.Target.(*ast.Identifier)
	patternNames(b.Target, func(id *ast.Identifier) {
		bd := a.declName(target, string(id.Name), kind, fn)
		if simple {
			if b.Initializer != nil {
				bd.assigned = append(bd.assigned, b.Initializer)
				switch init := b.Initializer.(type) {
				case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
					bd.decl = init
				}
			}
		} else {
			bd.opaque = true
		}
	})
}

// declare registers the declarations of a node and the functions and scopes below it.
func (a *analysis) declare(n any, sc *scope, fn *fnInfo) {
	switch s := n.(type) {
	case nil:
		return
	case *ast.FunctionDeclaration:
		if s.Function.Name != nil {
			b := a.declName(sc, string(s.Function.Name.Name), "function", fn)
			b.decl = s.Function
		}
		a.declare(s.Function, sc, fn)
		return
	case *ast.FunctionLiteral:
		f := a.newFn(s, fn, sc)
		if s.Name != nil {
			// A function expression's own name is visible inside it.
			a.declName(f.scope, string(s.Name.Name), "function", f).decl = s
		}
		a.declareParams(s.ParameterList, f)
		if s.Body != nil {
			for _, st := range s.Body.List {
				a.declare(st, f.scope, f)
			}
		}
		return
	case *ast.ArrowFunctionLiteral:
		f := a.newFn(s, fn, sc)
		a.declareParams(s.ParameterList, f)
		switch body := s.Body.(type) {
		case *ast.BlockStatement:
			for _, st := range body.List {
				a.declare(st, f.scope, f)
			}
		case *ast.ExpressionBody:
			a.declare(body.Expression, f.scope, f)
		}
		return
	case *ast.ClassDeclaration:
		if s.Class.Name != nil {
			a.declName(sc, string(s.Class.Name.Name), "class", fn)
		}
	case *ast.VariableStatement:
		for _, b := range s.List {
			a.declareBinding(b, "var", sc, fn)
		}
	case *ast.LexicalDeclaration:
		kind := "let"
		if s.Token == token.CONST {
			kind = "const"
		}
		for _, b := range s.List {
			a.declareBinding(b, kind, sc, fn)
		}
	case *ast.BlockStatement:
		inner := &scope{parent: sc, fn: fn, vars: map[string]*binding{}}
		a.scopes[s] = inner
		for _, st := range s.List {
			a.declare(st, inner, fn)
		}
		return
	case *ast.ForStatement:
		inner := &scope{parent: sc, fn: fn, vars: map[string]*binding{}}
		a.scopes[s] = inner
		switch init := s.Initializer.(type) {
		case *ast.ForLoopInitializerVarDeclList:
			for _, b := range init.List {
				a.declareBinding(b, "var", inner, fn)
			}
		case *ast.ForLoopInitializerLexicalDecl:
			kind := "let"
			if init.LexicalDeclaration.Token == token.CONST {
				kind = "const"
			}
			for _, b := range init.LexicalDeclaration.List {
				a.declareBinding(b, kind, inner, fn)
			}
		case *ast.ForLoopInitializerExpression:
			a.declare(init.Expression, inner, fn)
		}
		a.declare(s.Test, inner, fn)
		a.declare(s.Update, inner, fn)
		a.declare(s.Body, inner, fn)
		return
	case *ast.ForInStatement:
		a.declareForInto(s, s.Into, s.Source, s.Body, sc, fn)
		return
	case *ast.ForOfStatement:
		a.declareForInto(s, s.Into, s.Source, s.Body, sc, fn)
		return
	case *ast.CatchStatement:
		inner := &scope{parent: sc, fn: fn, vars: map[string]*binding{}}
		a.scopes[s] = inner
		patternNames(s.Parameter, func(id *ast.Identifier) { a.declName(inner, string(id.Name), "catch", fn).opaque = true })
		if s.Body != nil {
			for _, st := range s.Body.List {
				a.declare(st, inner, fn)
			}
		}
		return
	case *ast.SwitchStatement:
		inner := &scope{parent: sc, fn: fn, vars: map[string]*binding{}}
		a.scopes[s] = inner
		a.declare(s.Discriminant, sc, fn)
		for _, c := range s.Body {
			a.declare(c.Test, inner, fn)
			for _, st := range c.Consequent {
				a.declare(st, inner, fn)
			}
		}
		return
	}
	for _, c := range children(n) {
		a.declare(c, sc, fn)
	}
}

func (a *analysis) declareForInto(loop any, into ast.ForInto, source ast.Expression, body ast.Statement, sc *scope, fn *fnInfo) {
	inner := &scope{parent: sc, fn: fn, vars: map[string]*binding{}}
	a.scopes[loop] = inner
	switch i := into.(type) {
	case *ast.ForIntoVar:
		patternNames(i.Binding.Target, func(id *ast.Identifier) { a.declName(inner.fnScope(), string(id.Name), "var", fn).opaque = true })
	case *ast.ForDeclaration:
		kind := "let"
		if i.IsConst {
			kind = "const"
		}
		patternNames(i.Target, func(id *ast.Identifier) { a.declName(inner, string(id.Name), kind, fn).opaque = true })
	case *ast.ForIntoExpression:
		a.declare(i.Expression, inner, fn)
	}
	a.declare(source, sc, fn)
	a.declare(body, inner, fn)
}

func (a *analysis) declareParams(pl *ast.ParameterList, f *fnInfo) {
	if pl == nil {
		return
	}
	for _, p := range pl.List {
		patternNames(p.Target, func(id *ast.Identifier) { a.declName(f.scope, string(id.Name), "param", f).opaque = true })
		a.declare(p.Initializer, f.scope, f)
	}
	patternNames(pl.Rest, func(id *ast.Identifier) { a.declName(f.scope, string(id.Name), "param", f).opaque = true })
}

// ref resolves one identifier occurrence. write marks the binding assigned; read records a use from a function that
// is not the one the binding belongs to.
func (a *analysis) ref(id *ast.Identifier, sc *scope, fn *fnInfo, read_, write bool, rhs ast.Expression) {
	b := sc.lookup(string(id.Name))
	a.refs[id] = b
	if b == nil {
		return
	}
	if write {
		if !b.reassigned {
			b.reassigned, b.line = true, a.lineOf(id)
		}
		if rhs != nil {
			b.assigned = append(b.assigned, rhs)
			switch r := rhs.(type) {
			case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
				b.decl = r
			}
		} else {
			b.opaque = true
		}
	}
	if read_ {
		for f := fn; f != nil && f != b.owner; f = f.parent {
			f.freeReads = append(f.freeReads, read{b: b, line: a.lineOf(id)})
		}
	}
}

// resolvePattern resolves the identifiers of an assignment target (write) and the defaults and computed keys inside it.
func (a *analysis) resolvePattern(t any, sc *scope, fn *fnInfo, write bool) {
	switch p := t.(type) {
	case nil:
	case *ast.Identifier:
		if write {
			a.ref(p, sc, fn, false, true, nil)
		} else {
			a.refs[p] = sc.lookup(string(p.Name))
		}
	case *ast.ArrayPattern:
		for _, e := range p.Elements {
			a.resolvePattern(e, sc, fn, write)
		}
		a.resolvePattern(p.Rest, sc, fn, write)
	case *ast.ObjectPattern:
		for _, prop := range p.Properties {
			switch pp := prop.(type) {
			case *ast.PropertyShort:
				if write {
					a.ref(&pp.Name, sc, fn, false, true, nil)
				} else {
					a.refs[&pp.Name] = sc.lookup(string(pp.Name.Name))
				}
				a.resolve(pp.Initializer, sc, fn)
			case *ast.PropertyKeyed:
				if pp.Computed {
					a.resolve(pp.Key, sc, fn)
				}
				a.resolvePattern(pp.Value, sc, fn, write)
			}
		}
		a.resolvePattern(p.Rest, sc, fn, write)
	case *ast.AssignExpression:
		a.resolvePattern(p.Left, sc, fn, write)
		a.resolve(p.Right, sc, fn)
	case *ast.SpreadElement:
		a.resolvePattern(p.Expression, sc, fn, write)
	default:
		a.resolve(t, sc, fn)
	}
}

// resolve walks a node, binding every identifier reference to its declaration.
func (a *analysis) resolve(n any, sc *scope, fn *fnInfo) {
	switch s := n.(type) {
	case nil:
		return
	case *ast.Identifier:
		a.ref(s, sc, fn, true, false, nil)
	case *ast.FunctionDeclaration:
		a.resolve(s.Function, sc, fn)
	case *ast.FunctionLiteral:
		f := a.fns[s]
		a.resolveParams(s.ParameterList, f)
		if s.Body != nil {
			for _, st := range s.Body.List {
				a.resolve(st, f.scope, f)
			}
		}
	case *ast.ArrowFunctionLiteral:
		f := a.fns[s]
		a.resolveParams(s.ParameterList, f)
		switch body := s.Body.(type) {
		case *ast.BlockStatement:
			for _, st := range body.List {
				a.resolve(st, f.scope, f)
			}
		case *ast.ExpressionBody:
			a.resolve(body.Expression, f.scope, f)
		}
	case *ast.BlockStatement:
		inner := a.scopes[s]
		if inner == nil {
			inner = sc
		}
		for _, st := range s.List {
			a.resolve(st, inner, fn)
		}
	case *ast.ForStatement:
		inner := a.scopes[s]
		switch init := s.Initializer.(type) {
		case *ast.ForLoopInitializerVarDeclList:
			a.resolveBindings(init.List, inner, fn)
		case *ast.ForLoopInitializerLexicalDecl:
			a.resolveBindings(init.LexicalDeclaration.List, inner, fn)
		case *ast.ForLoopInitializerExpression:
			a.resolve(init.Expression, inner, fn)
		}
		a.resolve(s.Test, inner, fn)
		a.resolve(s.Update, inner, fn)
		a.resolve(s.Body, inner, fn)
	case *ast.ForInStatement:
		a.resolveForInto(a.scopes[s], s.Into, s.Source, s.Body, sc, fn)
	case *ast.ForOfStatement:
		a.resolveForInto(a.scopes[s], s.Into, s.Source, s.Body, sc, fn)
	case *ast.CatchStatement:
		inner := a.scopes[s]
		if s.Body != nil {
			for _, st := range s.Body.List {
				a.resolve(st, inner, fn)
			}
		}
	case *ast.SwitchStatement:
		inner := a.scopes[s]
		a.resolve(s.Discriminant, sc, fn)
		for _, c := range s.Body {
			a.resolve(c.Test, inner, fn)
			for _, st := range c.Consequent {
				a.resolve(st, inner, fn)
			}
		}
	case *ast.VariableStatement:
		a.resolveBindings(s.List, sc, fn)
	case *ast.LexicalDeclaration:
		a.resolveBindings(s.List, sc, fn)
	case *ast.ClassDeclaration:
		a.resolve(s.Class, sc, fn)
	case *ast.ClassLiteral:
		a.resolve(s.SuperClass, sc, fn)
		for _, el := range s.Body {
			switch e := el.(type) {
			case *ast.MethodDefinition:
				if e.Computed {
					a.resolve(e.Key, sc, fn)
				}
				a.resolve(e.Body, sc, fn)
			case *ast.FieldDefinition:
				if e.Computed {
					a.resolve(e.Key, sc, fn)
				}
				a.resolve(e.Initializer, sc, fn)
			case *ast.ClassStaticBlock:
				a.resolve(e.Block, sc, fn)
			}
		}
	case *ast.AssignExpression:
		switch left := s.Left.(type) {
		case *ast.Identifier:
			if s.Operator == token.ASSIGN {
				a.ref(left, sc, fn, false, true, s.Right)
			} else {
				a.ref(left, sc, fn, true, true, nil)
			}
		case *ast.ArrayPattern, *ast.ObjectPattern:
			a.resolvePattern(left, sc, fn, true)
		default:
			a.resolve(s.Left, sc, fn)
		}
		a.resolve(s.Right, sc, fn)
	case *ast.UnaryExpression:
		if id, ok := s.Operand.(*ast.Identifier); ok && (s.Operator == token.INCREMENT || s.Operator == token.DECREMENT) {
			a.ref(id, sc, fn, true, true, nil)
			return
		}
		a.resolve(s.Operand, sc, fn)
	case *ast.DotExpression:
		a.resolve(s.Left, sc, fn)
	case *ast.PrivateDotExpression:
		a.resolve(s.Left, sc, fn)
	case *ast.PropertyKeyed:
		if s.Computed {
			a.resolve(s.Key, sc, fn)
		}
		a.resolve(s.Value, sc, fn)
	case *ast.PropertyShort:
		a.ref(&s.Name, sc, fn, true, false, nil)
		a.resolve(s.Initializer, sc, fn)
	case *ast.BranchStatement:
	case *ast.LabelledStatement:
		a.resolve(s.Statement, sc, fn)
	case *ast.MetaProperty:
	default:
		for _, c := range children(n) {
			a.resolve(c, sc, fn)
		}
	}
}

func (a *analysis) resolveBindings(list []*ast.Binding, sc *scope, fn *fnInfo) {
	for _, b := range list {
		a.resolvePattern(b.Target, sc, fn, false)
		a.resolve(b.Initializer, sc, fn)
	}
}

func (a *analysis) resolveParams(pl *ast.ParameterList, f *fnInfo) {
	if pl == nil {
		return
	}
	for _, p := range pl.List {
		a.resolvePattern(p.Target, f.scope, f, false)
		a.resolve(p.Initializer, f.scope, f)
	}
	a.resolvePattern(pl.Rest, f.scope, f, false)
}

func (a *analysis) resolveForInto(inner *scope, into ast.ForInto, source ast.Expression, body ast.Statement, sc *scope, fn *fnInfo) {
	switch i := into.(type) {
	case *ast.ForIntoVar:
		a.resolvePattern(i.Binding.Target, inner, fn, false)
	case *ast.ForDeclaration:
		a.resolvePattern(i.Target, inner, fn, false)
	case *ast.ForIntoExpression:
		a.resolvePattern(i.Expression, inner, fn, true)
	}
	a.resolve(source, sc, fn)
	a.resolve(body, inner, fn)
}
