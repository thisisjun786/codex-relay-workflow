package script

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
	"github.com/evanw/esbuild/pkg/api"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows/lib"
)

// TransformTarget is the esbuild target the script body and its helpers are lowered to. goja runs ES2017 async
// functions, so the body keeps async/await (TestLoaderRunsMetaHelperAwaitReturn decides this; docs/flow/script.md
// states the outcome).
const TransformTarget = "ES2017"

var transformTarget = api.ES2017

// LoadError is any failure to turn a script into something that can run: a header or body the grammar refuses, a
// helper that cannot be resolved or bundled, a meta that is not a JSON-serialisable literal, a strict-schema
// violation in crw:lib/schemas, or a body that does not parse.
type LoadError struct {
	Name string
	Msg  string
}

func (e *LoadError) Error() string {
	if e.Name == "" {
		return "load: " + e.Msg
	}
	return fmt.Sprintf("load %s: %s", e.Name, e.Msg)
}

// Source is a script to load.
type Source struct {
	// Name is the script's display name (its file name).
	Name string
	// Text is the script's bytes.
	Text []byte
	// Dir is the script's workflows directory, the only place a relative import may read; nil allows none.
	Dir fs.FS
	// Lib is the embedded helper library behind crw:lib/<name>; nil is the library the binary embeds.
	Lib fs.FS
}

// Program is a loaded script: its meta, the digests the run record keeps, and the compiled programs a Runtime runs.
type Program struct {
	Name     string
	Digest   string
	Meta     map[string]any
	MetaJSON []byte
	Helpers  []Helper

	libs []*goja.Program
	main *goja.Program
	// exports lists, per helper, the names the script's imports read from it.
	exports [][]string
}

// Load turns a CRW workflow script into a Program.
//
// A script is a header and a body. The header is zero or more static imports followed by one
// `export const meta = <object literal>`; the body is everything after it, and may use top-level await and top-level
// return (a CRW extension: the body runs as an async function body). goja has no modules, so Load works in four steps:
// a lexer finds the end of the imports and of the meta literal; the meta literal is evaluated in a fresh runtime with no
// host globals and recorded as JSON; each imported helper is bundled by esbuild into an IIFE (the resolver admits only
// crw:lib/<name> and relative paths inside the workflows directory); and the body is wrapped in
// `(async function __crw_main(){ const {a, b:c} = __crw_lib_<n>; ...body... })`, syntax-checked and lowered by esbuild's
// Transform, and compiled by goja.
func Load(src Source) (*Program, error) {
	p, err := load(src)
	if err != nil {
		var le *LoadError
		if errors.As(err, &le) {
			return nil, err
		}
		return nil, &LoadError{Name: src.Name, Msg: err.Error()}
	}
	return p, nil
}

func load(src Source) (*Program, error) {
	name := src.Name
	if name == "" {
		name = "script.js"
	}
	text := string(src.Text)
	toks, err := tokenize(text)
	if err != nil {
		var le *lexError
		if errors.As(err, &le) {
			return nil, errAt(name, text, le.off, "%s", le.msg)
		}
		return nil, err
	}
	h, err := parseHeader(name, text, toks)
	if err != nil {
		return nil, err
	}
	if err := checkBody(name, text, toks, h.bodyStart); err != nil {
		return nil, err
	}
	p := &Program{Name: name, Digest: sha(src.Text)}
	if err := p.evalMeta(name, text, h); err != nil {
		return nil, err
	}

	libFS := src.Lib
	if libFS == nil {
		libFS = lib.FS
	}
	b := &bundler{lib: libFS, dir: src.Dir}
	byspec := map[string]int{}
	var wrapper strings.Builder
	wrapper.WriteString("(async function __crw_main(){ ")
	for _, imp := range h.imports {
		idx, seen := byspec[imp.spec]
		if !seen {
			idx = len(p.Helpers)
			byspec[imp.spec] = idx
			helper, err := b.bundle(imp.spec, fmt.Sprintf("__crw_lib_%d", idx))
			if err != nil {
				return nil, errAt(name, text, imp.off, "%s", err.Error())
			}
			p.Helpers = append(p.Helpers, *helper)
			p.exports = append(p.exports, nil)
			prog, err := goja.Compile(helper.GlobalName+".js", helper.code, false)
			if err != nil {
				return nil, errAt(name, text, imp.off, "helper %q does not compile: %v", imp.spec, err)
			}
			p.libs = append(p.libs, prog)
		}
		g := p.Helpers[idx].GlobalName
		var named []string
		for _, bd := range imp.bindings {
			switch bd.kind {
			case bindDefault:
				fmt.Fprintf(&wrapper, "const %s = %s.default; ", bd.local, g)
			case bindNamespace:
				fmt.Fprintf(&wrapper, "const %s = %s; ", bd.local, g)
			case bindNamed:
				if bd.export == bd.local {
					named = append(named, bd.local)
				} else {
					named = append(named, bd.export+": "+bd.local)
				}
				p.exports[idx] = append(p.exports[idx], bd.export)
			}
		}
		if len(named) > 0 {
			fmt.Fprintf(&wrapper, "const {%s} = %s; ", strings.Join(named, ", "), g)
		}
	}
	if b.usesSchemas {
		if err := lintSchemasModule(b); err != nil {
			return nil, &LoadError{Name: name, Msg: err.Error()}
		}
	}

	// The header is replaced by its newlines, so the body keeps its own line numbers in the wrapped text.
	wrapper.WriteString(strings.Repeat("\n", strings.Count(text[:h.bodyStart], "\n")))
	wrapper.WriteString(text[h.bodyStart:])
	wrapper.WriteString("\n})")
	tr := api.Transform(wrapper.String(), api.TransformOptions{
		Target:     transformTarget,
		Loader:     api.LoaderJS,
		Sourcemap:  api.SourceMapExternal,
		Sourcefile: name,
		LogLevel:   api.LogLevelSilent,
	})
	if len(tr.Errors) > 0 {
		return nil, &LoadError{Name: name, Msg: messages(tr.Errors)}
	}
	prog, err := compileWithMap(name, string(tr.Code), tr.Map)
	if err != nil {
		return nil, &LoadError{Name: name, Msg: err.Error()}
	}
	p.main = prog
	return p, nil
}

func compileWithMap(name, code string, sourceMap []byte) (*goja.Program, error) {
	var opts []parser.Option
	if len(sourceMap) > 0 {
		code += "\n//# sourceMappingURL=crw-main.map"
		opts = append(opts, parser.WithSourceMapLoader(func(string) ([]byte, error) { return sourceMap, nil }))
	}
	ast, err := goja.Parse(name, code, opts...)
	if err != nil {
		return nil, err
	}
	return goja.CompileAST(ast, false)
}

// evalMeta evaluates the meta literal as an expression in a fresh runtime that has no host globals and records it
// as JSON.
func (p *Program) evalMeta(name, text string, h *header) error {
	vm := goja.New()
	timer := time.AfterFunc(2*time.Second, func() { vm.Interrupt("meta evaluation took too long") })
	defer timer.Stop()
	// meta is part of the run record, so it is evaluated under the determinism stubs of the body.
	if _, err := vm.RunString(stubs); err != nil {
		return &LoadError{Name: name, Msg: "meta: determinism stubs: " + err.Error()}
	}
	v, err := vm.RunString("(" + h.metaSrc + ")")
	if err != nil {
		return &LoadError{Name: name, Msg: "meta: " + err.Error()}
	}
	obj, ok := v.(*goja.Object)
	if !ok || obj.ClassName() != "Object" {
		return &LoadError{Name: name, Msg: "meta must be an object literal"}
	}
	meta, ok := v.Export().(map[string]any)
	if !ok {
		return &LoadError{Name: name, Msg: "meta must be an object literal"}
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return &LoadError{Name: name, Msg: "meta must serialise to JSON: " + err.Error()}
	}
	p.Meta, p.MetaJSON = meta, data
	return nil
}

// lintSchemasModule builds crw:lib/schemas alone, evaluates it and checks every export against the strict
// structured-output rules. A violation is a load error.
func lintSchemasModule(b *bundler) error {
	h, err := b.bundle(LibPrefix+"schemas", "__crw_schemas")
	if err != nil {
		return err
	}
	vm := goja.New()
	timer := time.AfterFunc(2*time.Second, func() { vm.Interrupt("schemas evaluation took too long") })
	defer timer.Stop()
	if _, err := vm.RunString(h.code); err != nil {
		return fmt.Errorf("crw:lib/schemas: %v", err)
	}
	ns := vm.Get("__crw_schemas")
	obj, ok := ns.(*goja.Object)
	if !ok {
		return errors.New("crw:lib/schemas: the module has no exports")
	}
	keys := obj.Keys()
	sort.Strings(keys)
	var problems []string
	for _, k := range keys {
		exported := obj.Get(k).Export()
		raw, err := json.Marshal(exported)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: not a JSON schema: %v", k, err))
			continue
		}
		for _, e := range LintStrictSchemaJSON(raw) {
			problems = append(problems, k+e)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("crw:lib/schemas is not strict structured-output: %s", strings.Join(problems, "; "))
	}
	return nil
}
