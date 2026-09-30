package skill

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// hookMarkerSources are the packages the replay decision path runs through; every return-site
// marker lives in one of them.
var hookMarkerSources = []string{"internal/skill", "internal/relay/delivery", "internal/relay/hook"}

// hookSourceMarkers reads every return-site marker call out of the Go source: markHookReplay,
// reach, MarkReturn (bare or package-qualified) and Reach with a literal function name and
// ordinal, and probeState's answer(N, ...), which marks observe_state N. A marker whose function
// or ordinal is not a literal is refused, except the forwarding bodies that define the markers
// and the answer closure, so no marker can reach the denominator unseen.
func hookSourceMarkers(t *testing.T) map[string][]string {
	t.Helper()
	root := repositoryRoot()
	markers := map[string][]string{}
	fset := token.NewFileSet()
	for _, dir := range hookMarkerSources {
		paths, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				forwarding := fn.Name.Name == "markHookReplay" || fn.Name.Name == "reach" || fn.Name.Name == "MarkReturn"
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					where := fset.Position(call.Pos()).String()
					where = strings.TrimPrefix(where, root+string(filepath.Separator))
					name := ""
					switch fun := call.Fun.(type) {
					case *ast.Ident:
						name = fun.Name
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					}
					switch name {
					case "answer":
						if fn.Name.Name != "probeState" {
							return true
						}
						ordinal, ok := intLiteral(call.Args[0])
						if !ok {
							t.Errorf("%s: probeState answers with a non-literal ordinal", where)
							return true
						}
						key := fmt.Sprintf("observe_state:%d", ordinal)
						markers[key] = append(markers[key], where)
					case "markHookReplay", "reach", "MarkReturn", "Reach":
						args := call.Args
						if name != "Reach" && len(args) > 0 {
							args = args[1:]
						}
						if len(args) != 2 {
							t.Errorf("%s: %s takes a function and an ordinal", where, name)
							return true
						}
						function, functionOK := stringLiteral(args[0])
						ordinal, ordinalOK := intLiteral(args[1])
						switch {
						case functionOK && ordinalOK:
							key := fmt.Sprintf("%s:%d", function, ordinal)
							markers[key] = append(markers[key], where)
						case forwarding:
						case fn.Name.Name == "probeState" && functionOK && function == "observe_state" && name == "markHookReplay":
						default:
							t.Errorf("%s: a return-site marker must name its function and ordinal literally", where)
						}
					}
					return true
				})
			}
		}
	}
	return markers
}

func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func intLiteral(expr ast.Expr) (int, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.INT {
		return 0, false
	}
	value, err := strconv.Atoi(literal.Value)
	return value, err == nil
}

// The denominator is declared in hookReturnSites and derived from the source here: each row is
// a return some marker records, each marker records a row, and each function's ordinals run
// 1..n, so a return added or removed without its row fails.
func TestHookReturnSitesEqualTheSourceMarkers(t *testing.T) {
	markers := hookSourceMarkers(t)
	table := map[string]bool{}
	ordinals := map[string][]int{}
	for _, site := range hookReturnSites {
		if table[site.key()] {
			t.Errorf("%s is listed twice", site.key())
		}
		table[site.key()] = true
		ordinals[site.function] = append(ordinals[site.function], site.ordinal)
		if strings.TrimSpace(site.label) == "" {
			t.Errorf("%s has no label", site.key())
		}
		if len(markers[site.key()]) == 0 {
			t.Errorf("%s is in the denominator but no marker records it", site.key())
		}
	}
	for key, where := range markers {
		if !table[key] {
			t.Errorf("%s is recorded at %s but is not in hookReturnSites", key, strings.Join(where, ", "))
		}
	}
	if !slices.IsSortedFunc(hookReturnSites, func(a, b hookReplaySite) int {
		return cmp.Or(strings.Compare(a.function, b.function), a.ordinal-b.ordinal)
	}) {
		t.Error("hookReturnSites is not in (function, ordinal) order, the order replay reports them in")
	}
	for function, got := range ordinals {
		for i, ordinal := range got {
			if ordinal != i+1 {
				t.Errorf("%s ordinals are %v, not 1..%d in order", function, got, len(got))
				break
			}
		}
	}
}

// A replay over a subset of the fixtures names exactly the returns that subset leaves unexecuted,
// fails without --allow-unreached, and passes with it.
func TestHookReplayNamesEachUnreachedReturn(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(), "plugins", defaultFixture("decisions"), "unmanaged-session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unmanaged-session.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reached := []string{"decide:1", "observe_state:3", "selected_marker:1"}
	var want []string
	for _, site := range hookReturnSites {
		if !slices.Contains(reached, site.key()) {
			want = append(want, fmt.Sprintf("  UNREACHED %s#%d  %s", site.function, site.ordinal, site.label))
		}
	}
	unreachedLines := func(out string) []string {
		var lines []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "  UNREACHED ") {
				lines = append(lines, line)
			}
		}
		return lines
	}

	code, out, _ := call([]string{"hook-probe", "replay", "--fixtures", dir}, "")
	if code != 1 || !strings.Contains(out, fmt.Sprintf("return-site coverage: %d/%d sites reached\n", len(reached), len(hookReturnSites))) ||
		!strings.Contains(out, "A return site no fixture executes is an untested decision path.") {
		t.Fatalf("subset replay did not fail on unreached returns: %d\n%s", code, out)
	}
	if got := unreachedLines(out); !slices.Equal(got, want) {
		t.Fatalf("unreached returns\ngot:  %q\nwant: %q", got, want)
	}

	code, out, _ = call([]string{"hook-probe", "replay", "--fixtures", dir, "--allow-unreached"}, "")
	if code != 0 || !strings.Contains(out, "WAIVED: --allow-unreached was passed, so unreached return sites did not fail this run.") {
		t.Fatalf("--allow-unreached did not waive the gap: %d\n%s", code, out)
	}
	if got := unreachedLines(out); !slices.Equal(got, want) {
		t.Fatalf("waived run names other returns\ngot:  %q\nwant: %q", got, want)
	}
}
