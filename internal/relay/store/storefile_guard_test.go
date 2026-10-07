package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-846 guard: no non-test file under internal/relay opens a store file outside the registry.
//
// This is a regression guard, not enforcement (docs/relay/invariants.md I-563 and the plan's
// "Guard bypass" section): it recognises a literal store-file path and a concatenation with one
// of storeFileSuffixes, and, inside the four functions the issue names, any os/unix open that is
// not routed through the registry. It cannot follow a computed path handed to a helper, so the
// multi-process test -- not this guard -- is the proof that the rule holds.

// storeFileOpenFuncs are the functions the issue names as the raw opens of a store file. A call
// to one of the open functions inside any of them must be the registry's.
var storeFileOpenFuncs = map[string]bool{
	"holdDatabase":    true,
	"CopySnapshot":    true,
	"storeSocket":     true,
	"emitConfirmTurn": true,
}

// storeFileRegistryFiles hold the registry itself.
var storeFileRegistryFiles = map[string]bool{"storefile.go": true}

// storeFileOpenCalls are the open functions the guard watches.
var storeFileOpenCalls = map[string]bool{
	"os.Open":      true,
	"os.OpenFile":  true,
	"os.ReadFile":  true,
	"unix.Open":    true,
	"syscall.Open": true,
}

func TestStoreFileOpensAreRouted(t *testing.T) {
	root := filepath.Join(repositoryRoot(t), "internal", "relay")
	var scanned int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for _, finding := range storeFileGuardFindings(path, rel, raw) {
			t.Errorf("%s", finding)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("the guard scanned no file: it would pass vacuously")
	}
}

// storeFileGuardFindings reports the opens of a store file that bypass the registry in one file.
func storeFileGuardFindings(path, rel string, raw []byte) []string {
	if storeFileRegistryFiles[filepath.Base(path)] {
		return nil
	}
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, path, raw, 0)
	if err != nil {
		return []string{rel + ": " + err.Error()}
	}
	var findings []string
	for _, decl := range tree.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		named := storeFileOpenFuncs[fn.Name.Name]
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !storeFileOpenCalls[calleeName(call.Fun)] {
				return true
			}
			if storeFileDestinationCopy(call) {
				return true
			}
			line := fset.Position(call.Pos()).Line
			if named {
				findings = append(findings, rel+":"+strconv.Itoa(line)+": "+fn.Name.Name+" opens a file directly; a store file must go through holdStoreFile / ownership.HoldStoreFile (CRW-846)")
				return true
			}
			if len(call.Args) > 0 && storeFilePathArgument(call.Args[0]) {
				findings = append(findings, rel+":"+strconv.Itoa(line)+": a literal store-file path is opened directly; route it through the registry (CRW-846)")
			}
			return true
		})
	}
	return findings
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok {
			return pkg.Name + "." + f.Sel.Name
		}
	case *ast.Ident:
		return f.Name
	}
	return ""
}

// storeFileDestinationCopy is CopySnapshot's one legitimate open: the temporary copy it writes,
// recognised by its exclusive-create flags and by its dst variable.
func storeFileDestinationCopy(call *ast.CallExpr) bool {
	if calleeName(call.Fun) != "os.OpenFile" || len(call.Args) < 2 {
		return false
	}
	if !namesDst(call.Args[0]) {
		return false
	}
	flags, ok := call.Args[1].(*ast.BinaryExpr)
	if !ok {
		return false
	}
	text := renderExpr(flags)
	return strings.Contains(text, "O_CREATE") && strings.Contains(text, "O_EXCL")
}

// namesDst reports whether an expression is the copy destination: dst itself or dst plus a
// sidecar suffix (dst+suffix), which is how CopySnapshot names the file it writes.
func namesDst(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "dst"
	case *ast.BinaryExpr:
		return x.Op == token.ADD && namesDst(x.X)
	}
	return false
}

func renderExpr(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		return renderExpr(x.X) + "|" + renderExpr(x.Y)
	case *ast.SelectorExpr:
		return calleeName(x)
	}
	return ""
}

// storeFilePathArgument reports whether an argument names a store file: a literal containing
// relay.sqlite3, or a concatenation that adds one of the sidecar suffixes.
func storeFilePathArgument(arg ast.Expr) bool {
	switch x := arg.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return false
		}
		if strings.Contains(x.Value, "relay.sqlite3") {
			return true
		}
		text, err := strconv.Unquote(x.Value)
		if err != nil {
			return false
		}
		for _, suffix := range storeFileSuffixes {
			if suffix != "" && text == suffix {
				return true
			}
		}
		return false
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return storeFilePathArgument(x.X) || storeFilePathArgument(x.Y)
		}
	case *ast.Ident:
		return strings.Contains(x.Name, "relay")
	}
	return false
}

// TestStoreFileGuard_recognition pins the guard's recognition on synthetic sources, so the rules
// above cannot silently stop matching (the guard itself is a regression check, not enforcement).
func TestStoreFileGuard_recognition(t *testing.T) {
	literal := "package store" + "\n" + "func f() { os.Open(\"/s/relay.sqlite3\") }" + "\n"
	sidecar := "package store" + "\n" + "func f(p string) { os.Open(p + \"-wal\") }" + "\n"
	inHold := "package store" + "\n" + "func holdDatabase(p string) { os.Open(p) }" + "\n"
	inCopy := "package ownership" + "\n" + "func CopySnapshot(p string) { os.Open(p) }" + "\n"
	registry := "package store" + "\n" + "func holdStoreFile(p string) (*os.File, error) { return os.OpenFile(p, 0, 0) }" + "\n"
	destination := "package ownership" + "\n" + "func CopySnapshot(path string) { os.OpenFile(dst+suffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) }" + "\n"
	other := "package store" + "\n" + "func f(d string) { os.Open(filepath.Join(d, \"takeover.json\")) }" + "\n"

	cases := []struct {
		name string
		file string
		code string
		want int
	}{
		{"a literal store path outside the named functions is caught", "other.go", literal, 1},
		{"a sidecar concatenation is caught", "other.go", sidecar, 1},
		{"a raw open inside holdDatabase is caught even with a computed path", "diagnostic.go", inHold, 1},
		{"a raw open inside CopySnapshot is caught even with a computed path", "record.go", inCopy, 1},
		{"the registry file is exempt", "storefile.go", registry, 0},
		{"CopySnapshot's destination copy is exempt", "record.go", destination, 0},
		{"a non-store open is left alone", "other.go", other, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := storeFileGuardFindings(c.file, c.file, []byte(c.code))
			if len(got) != c.want {
				t.Fatalf("findings = %v, want %d", got, c.want)
			}
		})
	}
}
