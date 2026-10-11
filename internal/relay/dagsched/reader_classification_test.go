package dagsched

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// CRW-1212 (R04): the reader set of the frozen execution, release, acceptance, verified-head and integration-stage tables, and the document that classifies it
// (docs/relay/agent-execution-readers.md).
//
// An agent node's rows are in other tables (dag_agent_executions and, from R05, the agent acceptance and stage tables), so a reader of the frozen tables that answers a question about a NODE
// sees only the relationship side of it. Which readers those are is not a list to keep by hand and not a grep: a file that reads the table through a helper (currentRelationshipOf, loadActiveAcceptance,
// commitAccepted, integrationRefsOf, standOf) never spells the table. The set is therefore computed from the code:
//
//   - every non-test Go file under internal/relay and internal/manage that spells one of the tables in a string literal (the SQL it runs), and
//   - every such file that uses a function which reads one of them: a function whose body, or a package-level constant or variable it uses, spells a table. The functions are resolved with go/types over
//     the packages of those two trees (a package outside them is an empty stub: only what the trees define is followed), so a call across packages (manage calling dagsched) is followed too.
//
// The set is computed for linux/amd64, with the build constraints of that platform, so the same files make it wherever the test runs; the files of other platforms are read for the tables they spell.
// The document gives every file of the set one row (file, class, owner issue, migrated), and TestEveryZoneReaderClassified fails on a file without a row, a row for a file outside the set, and a row
// without an owner. The migrated column is R06's: it moves the node-scoped rows to the union readers (executionOf, releaseOf, acceptanceOf, agentIntegratedAt).

// readerTables are the frozen tables whose readers are classified.
var readerTables = []string{"dag_releases", "dag_node_executions", "dag_acceptances", "dag_verified_heads", "dag_integration_stages", "dag_acceptance_premerge", "dag_acceptance_verifications"}

var readerTablePattern = regexp.MustCompile(`\b(` + strings.Join(readerTables, "|") + `)\b`)

// readerEntry is why a file is in the set: the tables it spells, and the reading functions it uses (functions of the trees, by full name).
type readerEntry struct {
	Tables []string
	Via    []string
}

// schemaStatement is a statement that defines a table, trigger, index or view: the zone ledger spells the tables in the statements that create them, and installing the zone at every open is not a read of
// them, so a function that holds such a statement does not make the callers of the store's open readers. (The file that spells them is in the set all the same, class store.)
var schemaStatement = regexp.MustCompile(`(?is)^\s*(CREATE|DROP|ALTER)\s`)

// tablesIn is the frozen tables a string spells.
func tablesIn(text string) []string { return readerTablePattern.FindAllString(text, -1) }

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// moduleOf is the module path of the module whose go.mod is in root.
func moduleOf(t testing.TB, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	t.Fatalf("%s/go.mod names no module", root)
	return ""
}

// readerPackage is a directory of the trees: its files for the platform, and every non-test file of the directory.
type readerPackage struct {
	dir     string // relative to the module root, slash-separated
	path    string // import path
	files   []*ast.File
	names   []string // the relative path of each of files
	pkg     *types.Package
	info    *types.Info
	checked bool
}

// readerAnalysis loads the packages under roots (relative to root) and type-checks them.
type readerAnalysis struct {
	t      testing.TB
	root   string
	module string
	fset   *token.FileSet
	pkgs   map[string]*readerPackage // by import path
	// every holds every non-test file of the trees, whatever its platform, for the literals it spells
	every map[string]*ast.File
}

func newReaderAnalysis(t testing.TB, root string, trees ...string) *readerAnalysis {
	t.Helper()
	a := &readerAnalysis{t: t, root: root, module: moduleOf(t, root), fset: token.NewFileSet(), pkgs: map[string]*readerPackage{}, every: map[string]*ast.File{}}
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "linux", "amd64", false
	for _, tree := range trees {
		err := filepath.WalkDir(filepath.Join(root, tree), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			file, err := parser.ParseFile(a.fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			a.every[rel] = file
			if ok, err := ctx.MatchFile(filepath.Dir(p), name); err != nil || !ok {
				return err
			}
			dir := filepath.ToSlash(filepath.Dir(rel))
			path := a.module + "/" + dir
			pkg := a.pkgs[path]
			if pkg == nil {
				pkg = &readerPackage{dir: dir, path: path}
				a.pkgs[path] = pkg
			}
			pkg.files = append(pkg.files, file)
			pkg.names = append(pkg.names, rel)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, pkg := range a.pkgs {
		a.check(pkg)
	}
	return a
}

// Import makes the packages of the trees importable by each other; any other package is an empty, complete stub.
func (a *readerAnalysis) Import(path string) (*types.Package, error) {
	if pkg, ok := a.pkgs[path]; ok {
		a.check(pkg)
		return pkg.pkg, nil
	}
	stub := types.NewPackage(path, filepath.Base(path))
	stub.MarkComplete()
	return stub, nil
}

func (a *readerAnalysis) check(pkg *readerPackage) {
	if pkg.checked {
		return
	}
	pkg.checked = true
	pkg.info = &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: a, Error: func(error) {}, FakeImportC: true}
	pkg.pkg, _ = conf.Check(pkg.path, a.fset, pkg.files, pkg.info)
}

// literalsIn is the string literals under a node.
func literalsIn(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(node ast.Node) bool {
		if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if text, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, text)
			}
		}
		return true
	})
	return out
}

// readerSet is the set of files that spell a table or use a function that reads one.
func (a *readerAnalysis) readerSet() map[string]readerEntry {
	set := map[string]readerEntry{}
	direct := map[string][]string{}
	for rel, file := range a.every {
		var tables []string
		for _, text := range literalsIn(file) {
			tables = append(tables, tablesIn(text)...)
		}
		if len(tables) > 0 {
			direct[rel] = uniqueSorted(tables)
			set[rel] = readerEntry{Tables: direct[rel]}
		}
	}
	// the objects that read a table: a function whose body spells one, and a package-level constant or variable whose value does
	readers := map[types.Object]string{}
	type declaration struct {
		pkg  *readerPackage
		obj  types.Object
		body ast.Node
	}
	var declarations []declaration
	for _, pkg := range a.pkgs {
		for _, file := range pkg.files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body == nil {
						continue
					}
					if obj := pkg.info.Defs[d.Name]; obj != nil {
						declarations = append(declarations, declaration{pkg, obj, d.Body})
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if v, ok := spec.(*ast.ValueSpec); ok {
							for _, name := range v.Names {
								if obj := pkg.info.Defs[name]; obj != nil {
									declarations = append(declarations, declaration{pkg, obj, v})
								}
							}
						}
					}
				}
			}
		}
	}
	for _, d := range declarations {
		for _, text := range literalsIn(d.body) {
			if len(tablesIn(text)) > 0 && !schemaStatement.MatchString(text) {
				readers[d.obj] = qualified(d.pkg, d.obj)
				break
			}
		}
	}
	// a declaration that uses a reader reads through it: the readers are followed to a fixed point, so a file that only calls the outer wrapper of a chain of calls is in the set (the calls are the uses
	// go/types resolved: functions, methods of the trees' types, constants and variables; a call through an interface value or a function value is not followed)
	for changed := true; changed; {
		changed = false
		for _, d := range declarations {
			if _, ok := readers[d.obj]; ok {
				continue
			}
			reads := false
			ast.Inspect(d.body, func(n ast.Node) bool {
				if ident, ok := n.(*ast.Ident); ok {
					if used := d.pkg.info.Uses[ident]; used != nil && used != d.obj {
						if _, ok := readers[used]; ok {
							reads = true
						}
					}
				}
				return !reads
			})
			if reads {
				readers[d.obj] = qualified(d.pkg, d.obj)
				changed = true
			}
		}
	}
	for _, pkg := range a.pkgs {
		for i, file := range pkg.files {
			rel := pkg.names[i]
			var via []string
			ast.Inspect(file, func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				obj := pkg.info.Uses[ident]
				if obj == nil {
					return true
				}
				if name, ok := readers[obj]; ok && !a.declaredIn(pkg, file, obj) {
					via = append(via, name)
				}
				return true
			})
			if len(via) > 0 {
				entry := set[rel]
				entry.Via = uniqueSorted(via)
				set[rel] = entry
			}
		}
	}
	return set
}

// declaredIn is whether obj is declared in file: a use inside the file that declares a reader adds nothing to the file's place in the set (it is in it already, for the literal).
func (a *readerAnalysis) declaredIn(pkg *readerPackage, file *ast.File, obj types.Object) bool {
	pos := obj.Pos()
	return pos >= file.Pos() && pos <= file.End() && obj.Pkg() == pkg.pkg
}

func qualified(pkg *readerPackage, obj types.Object) string {
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			recv := sig.Recv().Type()
			if p, ok := recv.(*types.Pointer); ok {
				recv = p.Elem()
			}
			if named, ok := recv.(*types.Named); ok {
				return named.Obj().Name() + "." + fn.Name()
			}
		}
	}
	return obj.Name()
}

// writeFixture writes files (path -> text) under a new directory and returns it.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A file that never spells a table but uses a function that reads one is a reader, within a package and across packages, and the files that have nothing to do with the tables are not.
func TestReaderSetIncludesHelperCallers(t *testing.T) {
	t.Parallel()
	root := writeFixture(t, map[string]string{
		"go.mod": "module fixture.example/m\n\ngo 1.27\n",
		"internal/relay/pkg/helper.go": `package pkg

import "context"

// the helper reads the table; nothing else in the package spells it
func loadActiveAcceptance(ctx context.Context, q interface{ Query(string) }) { q.Query("SELECT * FROM dag_acceptances WHERE state = 'active'") }

// LoadActive is the exported form another package calls
func LoadActive(ctx context.Context, q interface{ Query(string) }) {
	q.Query("SELECT 1 FROM dag_acceptances")
	loadActiveAcceptance(ctx, q)
}

const standQuery = "SELECT head_sha FROM dag_integration_stages"

func stand() string { return standQuery }

type Reader struct{}

// Current is a method that reads a table
func (Reader) Current() string { return "SELECT head_sha FROM dag_verified_heads" }
`,
		"internal/relay/pkg/caller.go": `package pkg

import "context"

// caller never spells a table: it only calls the helper
func caller(ctx context.Context) { loadActiveAcceptance(ctx, nil) }
`,
		"internal/relay/pkg/constcaller.go": `package pkg

// constcaller uses a constant that spells a table
func constcaller() string { return standQuery }
`,
		"internal/relay/other/cross.go": `package other

import (
	"context"

	"fixture.example/m/internal/relay/pkg"
)

func cross(ctx context.Context) { pkg.LoadActive(ctx, nil) }
`,
		"internal/relay/other/method.go": `package other

import "fixture.example/m/internal/relay/pkg"

func method() string { return pkg.Reader{}.Current() }
`,
		"internal/relay/other/unrelated.go": `package other

import "fmt"

func unrelated() string { return fmt.Sprint("dag_acceptances_active is an index name, and the word acceptances alone is nothing") }
`,
		"internal/relay/other/unrelated_test.go": `package other

const testOnly = "SELECT * FROM dag_releases"
`,
		"internal/relay/other/platform_other.go": "//go:build windows\n\npackage other\n\nconst windowsQuery = \"SELECT 1 FROM dag_verified_heads\"\n",
		"internal/manage/use.go": `package manage

import "fixture.example/m/internal/relay/pkg"

func use() { pkg.LoadActive(nil, nil) }
`,
		"internal/elsewhere/outside.go": `package elsewhere

const outside = "SELECT * FROM dag_releases"
`,
	})
	set := newReaderAnalysis(t, root, "internal/relay", "internal/manage").readerSet()
	want := map[string]readerEntry{
		"internal/relay/pkg/helper.go":           {Tables: []string{"dag_acceptances", "dag_integration_stages", "dag_verified_heads"}},
		"internal/relay/pkg/caller.go":           {Via: []string{"loadActiveAcceptance"}},
		"internal/relay/pkg/constcaller.go":      {Via: []string{"standQuery"}},
		"internal/relay/other/cross.go":          {Via: []string{"LoadActive"}},
		"internal/relay/other/method.go":         {Via: []string{"Reader.Current"}},
		"internal/manage/use.go":                 {Via: []string{"LoadActive"}},
		"internal/relay/other/platform_other.go": {Tables: []string{"dag_verified_heads"}},
	}
	for file, entry := range want {
		got, ok := set[file]
		if !ok {
			t.Errorf("%s is not in the reader set", file)
			continue
		}
		if len(entry.Tables) > 0 && strings.Join(got.Tables, ",") != strings.Join(entry.Tables, ",") {
			t.Errorf("%s spells %v, want %v", file, got.Tables, entry.Tables)
		}
		for _, via := range entry.Via {
			if !contains(got.Via, via) {
				t.Errorf("%s uses %v, want %s among them", file, got.Via, via)
			}
		}
	}
	for file := range set {
		if _, ok := want[file]; !ok {
			t.Errorf("%s is in the reader set although it has nothing to do with the tables: %+v", file, set[file])
		}
	}
	if entry := set["internal/relay/pkg/caller.go"]; len(entry.Tables) != 0 {
		t.Errorf("the caller spells no table, found %v", entry.Tables)
	}
}

// A reader is followed to a fixed point: a function that calls a function that reads is a reader itself, so a file that only calls the outer wrapper (any number of calls above the SQL, in the package or
// across packages, through functions, methods and constants, in a cycle as well) is in the set.
func TestReaderSetFollowsWrappersToAFixedPoint(t *testing.T) {
	t.Parallel()
	root := writeFixture(t, map[string]string{
		"go.mod": "module fixture.example/m\n\ngo 1.27\n",
		"internal/relay/pkg/reader.go": `package pkg

func read() string { return "SELECT * FROM dag_acceptances" }

const query = "SELECT head_sha FROM dag_verified_heads"
`,
		"internal/relay/pkg/wrapper.go": `package pkg

func wrap() string { return read() }

// ping and pong call each other; pong reaches the reader
func ping(n int) string {
	if n > 0 {
		return pong(n - 1)
	}
	return ""
}

func pong(n int) string {
	if n > 0 {
		return ping(n - 1)
	}
	return wrap()
}

type Source struct{}

func (Source) Rows() string { return wrap() }

func Exported() string { return Source{}.Rows() }

func viaConst() string { return query }
`,
		"internal/relay/pkg/caller.go": `package pkg

func caller() string { return wrap() }
`,
		"internal/relay/pkg/third.go": `package pkg

func third() string { return caller() }
`,
		"internal/relay/pkg/cyclecaller.go": `package pkg

func cycleCaller() string { return ping(3) }
`,
		"internal/relay/pkg/constcaller.go": `package pkg

func constCaller() string { return viaConst() }
`,
		"internal/relay/other/cross.go": `package other

import "fixture.example/m/internal/relay/pkg"

func cross() string { return pkg.Exported() }
`,
		"internal/manage/top.go": `package manage

import "fixture.example/m/internal/relay/other"

func top() string { return other.Chain() }
`,
		"internal/relay/other/chain.go": `package other

func Chain() string { return cross() }
`,
		"internal/relay/store/zone.go": `package store

// the zone ledger spells the tables in the statements that create them; installing it at every open is not a read
var ledger = []string{"CREATE TABLE IF NOT EXISTS dag_releases (id TEXT)", "CREATE TRIGGER IF NOT EXISTS dag_releases_no_update BEFORE UPDATE ON dag_releases BEGIN SELECT 1; END"}

func install() int { return len(ledger) }

func Open() int { return install() }
`,
		"internal/relay/other/opens.go": `package other

import "fixture.example/m/internal/relay/store"

func opens() int { return store.Open() }
`,
		"internal/relay/other/alone.go": `package other

func alone() string { return "nothing of the tables" }

func aloneToo() string { return alone() }
`,
	})
	set := newReaderAnalysis(t, root, "internal/relay", "internal/manage").readerSet()
	for _, file := range []string{
		"internal/relay/pkg/wrapper.go", "internal/relay/pkg/caller.go", "internal/relay/pkg/third.go", "internal/relay/pkg/cyclecaller.go", "internal/relay/pkg/constcaller.go",
		"internal/relay/other/cross.go", "internal/relay/other/chain.go", "internal/manage/top.go",
	} {
		if _, ok := set[file]; !ok {
			t.Errorf("%s calls a reader through wrappers and is not in the reader set", file)
		}
	}
	if _, ok := set["internal/relay/other/alone.go"]; ok {
		t.Error("a function that reads nothing, and its caller, are in the set")
	}
	if _, ok := set["internal/relay/store/zone.go"]; !ok {
		t.Error("the file that spells the tables in the zone ledger is not in the set")
	}
	for _, file := range []string{"internal/relay/other/opens.go"} {
		if entry, ok := set[file]; ok {
			t.Errorf("%s only opens the store, which installs the zone, and is in the set: %+v", file, entry)
		}
	}
	if entry := set["internal/relay/pkg/third.go"]; !contains(entry.Via, "caller") {
		t.Errorf("third.go uses %v, want caller among them", entry.Via)
	}
}

// The set of the tree holds the readers that reach a table through several calls: progress_replay.go calls Progress, which calls the readers of the tables.
func TestReaderSetOfTheTreeHoldsIndirectReaders(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	set := newReaderAnalysis(t, root, "internal/relay", "internal/manage").readerSet()
	if _, ok := set["internal/relay/dagsched/progress_replay.go"]; !ok {
		t.Fatal("internal/relay/dagsched/progress_replay.go reaches the frozen tables through ProgressSnapshotOf, Progress and nodeFacts and is not in the reader set")
	}
}

// readersDocument is the document that classifies the reader set.
const readersDocument = "docs/relay/agent-execution-readers.md"

// The classes of a row, and the issues that own them. A node-scoped row is a reader that answers a question about a node and so must see both sides (R06 moves it to the union readers); a writer path is
// changed by the issue that adds the agent writer (R04 the release, R05 accept and integration); the lifecycle is R07's; a relationship-scoped reader names a relationship and stays as it is; a store
// row is the zone itself.
var (
	readerClasses = map[string]bool{"union": true, "caller": true, "node-scoped-release": true, "node-scoped-acceptance": true, "agent-writer": true, "agent-lifecycle": true, "relationship-scoped": true, "store": true}
	readerOwners  = map[string]bool{"R04": true, "R05": true, "R06": true, "R07": true}
	migratedWords = map[string]bool{"yes": true, "no": true, "n/a": true}
)

// readerRow is a row of the document.
type readerRow struct {
	File, Class, Owner, Migrated string
	Line                         int
}

// parseReaderRows reads the rows of the document's table: a line of five cells whose first cell is a code span naming a file, in the order file, class, owner, migrated, note.
func parseReaderRows(t testing.TB, text string) []readerRow {
	t.Helper()
	var rows []readerRow
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for j := range cells {
			cells[j] = strings.TrimSpace(cells[j])
		}
		if len(cells) != 5 || !strings.HasPrefix(cells[0], "`") {
			continue
		}
		rows = append(rows, readerRow{File: strings.Trim(cells[0], "`"), Class: strings.Trim(cells[1], "`"), Owner: strings.Trim(cells[2], "`"), Migrated: strings.Trim(cells[3], "`"), Line: i + 1})
	}
	return rows
}

// designClassification is the classification table of the design (relay.added): the files it names, by short name, with the class(es) and the issue(s) it gives them. The document must agree with it for
// every file it names; the files it leaves to "all unchanged otherwise" are classified in the document alone.
var designClassification = map[string]struct{ classes, owners string }{
	"internal/relay/dagsched/progress.go":            {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/progress_replay.go":     {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/nodestate.go":           {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/edges.go":               {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/ready.go":               {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/landingresult.go":       {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/measure.go":             {"node-scoped-release", "R06"},
	"internal/relay/dagsched/liveheads.go":           {"node-scoped-release", "R06"},
	"internal/relay/dagsched/livesweep.go":           {"node-scoped-release", "R06"},
	"internal/relay/dagsched/mergeorder_derive.go":   {"node-scoped-release", "R06"},
	"internal/relay/dagsched/bundles.go":             {"node-scoped-release", "R06"},
	"internal/relay/dagsched/release_recovery.go":    {"node-scoped-release", "R06"},
	"internal/manage/capacity_branches.go":           {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/manage/dagreview.go":                   {"node-scoped-release", "R06"},
	"internal/manage/dagreview_store.go":             {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/manage/checkpoint.go":                  {"node-scoped-release+node-scoped-acceptance", "R06"},
	"internal/manage/relayread.go":                   {"node-scoped-release", "R06"},
	"internal/relay/dagsched/baserefresh.go":         {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/optimism.go":            {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/stagnation.go":          {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/mergechecks.go":         {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/mergejudge.go":          {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/accept_refresh.go":      {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/lifecycle.go":           {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/targets.go":             {"node-scoped-acceptance", "R06"},
	"internal/relay/acceptance/stand.go":             {"node-scoped-acceptance", "R06"},
	"internal/relay/dagsched/release.go":             {"agent-writer", "R04"},
	"internal/relay/dagsched/accept.go":              {"agent-writer", "R05"},
	"internal/relay/dagsched/integration_batch.go":   {"agent-writer", "R05"},
	"internal/relay/dagsched/integration.go":         {"agent-writer", "R05"},
	"internal/relay/dagsched/candidates.go":          {"agent-writer", "R05"},
	"internal/relay/dagsched/commit_targets.go":      {"agent-writer", "R05"},
	"internal/relay/dagsched/integration_push.go":    {"agent-writer", "R05"},
	"internal/relay/dagsched/epoch.go":               {"agent-lifecycle", "R07"},
	"internal/relay/dagsched/revalidation.go":        {"agent-lifecycle", "R07"},
	"internal/relay/dagsched/invalidation.go":        {"node-scoped-acceptance+agent-lifecycle", "R06,R07"},
	"internal/relay/dagsched/accept_commit.go":       {"relationship-scoped", "R06"},
	"internal/relay/dagsched/accept_premerge.go":     {"relationship-scoped", "R06"},
	"internal/relay/dagsched/correction.go":          {"relationship-scoped", "R06"},
	"internal/relay/dagsched/correction_decision.go": {"relationship-scoped", "R06"},
	"internal/relay/dagsched/handopen_guard.go":      {"relationship-scoped", "R06"},
	"internal/relay/dagsched/withdraw.go":            {"relationship-scoped", "R06"},
	"internal/relay/dagsched/mergecheck.go":          {"relationship-scoped", "R06"},
	"internal/relay/store/dag_zone.go":               {"store", "R04,R05"},
	"internal/relay/store/dag_integration_stages.go": {"store", "R05"},
	"internal/relay/store/domain_rows.go":            {"store", "R06"},
}

// The set is computed from the code (the AST and the type-checked call graph) and every file of it has a classified row with an owner issue; no row names a file outside the set; the classification of the
// files the design names is the design's; and every node-scoped row is migrated (R06).
func TestEveryZoneReaderClassified(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	set := newReaderAnalysis(t, root, "internal/relay", "internal/manage").readerSet()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(readersDocument)))
	if err != nil {
		t.Fatalf("the classification document: %v", err)
	}
	rows := parseReaderRows(t, string(data))
	byFile := map[string]readerRow{}
	for _, row := range rows {
		if previous, dup := byFile[row.File]; dup {
			t.Errorf("%s has two rows (lines %d and %d)", row.File, previous.Line, row.Line)
		}
		byFile[row.File] = row
	}
	for file, entry := range set {
		row, ok := byFile[file]
		if !ok {
			t.Errorf("%s reads the frozen tables (%s) and has no row in %s", file, why(entry), readersDocument)
			continue
		}
		for _, class := range strings.Split(row.Class, "+") {
			if !readerClasses[class] {
				t.Errorf("%s: class %q (line %d) is not one of the classes", file, row.Class, row.Line)
			}
		}
		if row.Owner == "" {
			t.Errorf("%s: the row (line %d) names no issue that owns it", file, row.Line)
		}
		for _, owner := range strings.Split(row.Owner, ",") {
			if owner != "" && !readerOwners[owner] {
				t.Errorf("%s: owner %q (line %d) is not an issue of the design (R04 to R07)", file, row.Owner, row.Line)
			}
		}
		if !migratedWords[row.Migrated] {
			t.Errorf("%s: migrated %q (line %d) is yes, no or n/a", file, row.Migrated, row.Line)
		}
		node := strings.Contains(row.Class, "node-scoped")
		if node && row.Migrated == "n/a" || !node && row.Migrated != "n/a" {
			t.Errorf("%s: class %s with migrated %s (line %d): only a node-scoped row is migrated or not, any other is n/a", file, row.Class, row.Migrated, row.Line)
		}
		// R06 (CRW-1214): every reader that answers about a node reads both sides of it
		if node && row.Migrated != "yes" {
			t.Errorf("%s: the node-scoped row (line %d) is migrated %q, want yes: the reader has to read the union of the two sides", file, row.Line, row.Migrated)
		}
	}
	for file, row := range byFile {
		if _, ok := set[file]; !ok {
			t.Errorf("line %d classifies %s, which is not in the reader set", row.Line, file)
		}
	}
	for file, want := range designClassification {
		row, ok := byFile[file]
		if !ok {
			continue // a file the code no longer has is reported above as a row outside the set, or is simply gone
		}
		if row.Class != want.classes || row.Owner != want.owners {
			t.Errorf("%s: the document says %s / %s, the design's classification table says %s / %s", file, row.Class, row.Owner, want.classes, want.owners)
		}
	}
	if len(set) < 30 {
		t.Errorf("the reader set holds only %d files", len(set))
	}
}

func why(e readerEntry) string {
	var parts []string
	if len(e.Tables) > 0 {
		parts = append(parts, "spells "+strings.Join(e.Tables, ", "))
	}
	if len(e.Via) > 0 {
		parts = append(parts, "uses "+strings.Join(e.Via, ", "))
	}
	return strings.Join(parts, "; ")
}

// printReaderSet lists the set for whoever writes the document: CRW_READER_SET=1 go test -run TestPrintReaderSet -v.
func TestPrintReaderSet(t *testing.T) {
	if os.Getenv("CRW_READER_SET") == "" {
		t.Skip("set CRW_READER_SET=1 to print the reader set")
	}
	set := newReaderAnalysis(t, filepath.Join("..", "..", ".."), "internal/relay", "internal/manage").readerSet()
	var files []string
	for file := range set {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		fmt.Printf("READER %s | %s\n", file, why(set[file]))
	}
}
