//go:build dev

package ci

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// CRW-1432 (bundle CRW-1422, Jun/Sion REFAC1015 brief A.2): text a person reads names the session layers
// 부모 and 자식, not the relay role ids. The relay role id `supervisor` is the 부모 and the relay role id
// `parent` is the 자식 (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"), so a bare
// "parent" in relay text names the wrong layer. This ratchet counts the bare role words that
// layername.Find reports in the Go string literals of internal/ and holds each file at or below its line in
// testdata/role_words_baseline.txt; an entry may only shrink. The role ids themselves stay in stored rows,
// flags, wire fields, refusal codes and SQL, which the scan skips.

const roleWordsBaselineFile = "testdata/role_words_baseline.txt"

// roleWordsHeader is the header a new baseline file is written with.
const roleWordsHeader = `# Bare relay role words (parent, supervisor) in the human text of internal/ Go string literals, per file
# (CRW-1432; counted by layername.Find, checked by internal/dev/ci/role_words_test.go). An entry may only shrink.
# Lower the lines of the files you changed:
#   CRW_ROLE_WORDS=update CRW_ROLE_WORDS_PATHS=<prefixes> go test -tags dev ./internal/dev/ci/ -run TestRoleWordsBaselineOnlyShrinks
`

// roleWordsSQLStatement and roleWordsSQLFragment mark an SQL literal, whose role values are data: the
// scan skips it, and SQL bytes are never edited to satisfy the scan. A literal is SQL by how it starts:
// a statement keyword in upper case with the clause it needs (SELECT ... FROM, INSERT INTO, UPDATE t SET,
// DELETE FROM, WITH t AS (, CREATE TABLE ...), or, for a piece of a statement built up in code, an upper-case
// clause keyword (WHERE, AND, JOIN ...) or a column predicate (role = 'parent' AND ...). A keyword or a
// predicate inside a sentence does not make it SQL: "UPDATE failed; ask the parent to retry" is counted.
var (
	roleWordsSQLStatement = regexp.MustCompile(`^\s*(?s:SELECT\s.*\sFROM\s|INSERT\s+(OR\s+[A-Z]+\s+)?INTO\s|REPLACE\s+INTO\s|UPDATE\s+[a-z_][a-z0-9_]*\s+SET\s|DELETE\s+FROM\s|WITH\s+(RECURSIVE\s+)?[a-z_][a-z0-9_]*\s*(\([^)]*\)\s*)?AS\s*\(|(CREATE|DROP)\s+(TABLE|(UNIQUE\s+)?INDEX|VIEW|TRIGGER|TEMP)\b|ALTER\s+TABLE\s)`)
	roleWordsSQLFragment  = regexp.MustCompile(`^\s*((AND|OR|WHERE|FROM|JOIN|LEFT\s+JOIN|INNER\s+JOIN|ON|SET|ORDER\s+BY|GROUP\s+BY|HAVING|UNION|VALUES)\s|\(?\s*([a-z_]\w*\.)?[a-z_]\w*\s*(=|!=|<>|IN\s*\()\s*['?(].*\s(AND|OR|IS|IN)\s)`)
)

// nonRoleLiterals are literals whose role word is no relay role (a git commit's parent, a request field
// name), keyed by roleWordsKey(path, unquoted literal) so a moved line keeps its entry. A role-sense
// literal never goes here: it is reworded or stays counted in the baseline.
var nonRoleLiterals = roleWordsAllowed([]struct{ path, literal, reason string }{
	{"internal/relay/dagsched/accept_premerge.go", "git could not read the parents of %s in %s: %v", "git: the parent commits of a merge"},
	{"internal/relay/dagsched/accept_premerge.go", "the merge %s is not the automatic merge of its parents (a hand-resolved merge or a merge outside the base)", "git: the parent commits of a merge"},
	{"internal/relay/dagsched/baserefresh_git.go", "its parent's manifest cannot be read apart from its version: ", "git: the plugin manifest of the parent commit"},
	{"internal/relay/dagsched/baserefresh_git.go", "its manifest differs from its parent's outside the version line", "git: the plugin manifest of the parent commit"},
	{"internal/relay/dagsched/baserefresh_git.go", "its version %q does not keep the release %q its parent records", "git: the release the parent commit records"},
	{"internal/relay/dagsched/baserefresh_git.go", "%s has 1 parent(s) and changes %s: a single-parent commit is work of the child's own unless it only re-records the plugin manifest's version line", "git: a commit's parent count"},
	{"internal/relay/dagsched/baserefresh_git.go", "%s has %d parent(s): a refresh is a chain of merges of exactly two parents, and this commit is work of the child's own", "git: a merge's two parent commits"},
	{"internal/relay/dagsched/baserefresh_git.go", "the first parent %s of %s does not contain the accepted head %s (parents swapped, or a branch that is not the accepted one was merged in)", "git: a merge's parent commits"},
	{"internal/relay/dagsched/release.go", "the agent surface starts no managed child: the release request carries no parent, child or allowed_recipients", "request field names parent, child and allowed_recipients"},
	{"internal/relay/mergeturn/movement.go", ", which is not the parent of the commit before it", "git: a commit's parent"},
	{"internal/relay/mergeturn/movement.go", " on the branch has one parent, so it is not a merge commit and is not known to be a merged pull request", "git: a commit's parent count"},
	{"internal/relay/mergeturn/movement_reader.go", "parent ", "git: the commit-header prefix the reader cuts (a parse token)"},
	{"internal/relay/mergeturn/movement_reader.go", "git named %s as a parent of commit %s, which is not a full object name", "git: a commit's parent"},
	{"internal/relay/mergeturn/movement_reader.go", "the forge named %s as a parent of commit %s, which is not a full object name", "git: a commit's parent as the forge reports it"},
	{"internal/relay/mergeturn/train.go", "merge commit %s has parents %s and %s, and a bundle's are the base %s and the verified head %s in that order", "git: a merge commit's parents"},
	{"internal/relay/mergeturn/train.go", "the parents of %s were not read: %v", "git: a merge commit's parents"},
	{"internal/relay/mergeturn/train.go", "the merge commit %s is not what git merges from its parents: %v", "git: a merge commit's parents"},
	{"internal/relay/mergeturn/train.go", "the tree of merge commit %s is %s and git merges %s from its parents, so the merge holds a hand resolution", "git: a merge commit's parents"},
})

func roleWordsAllowed(rows []struct{ path, literal, reason string }) map[string]string {
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[roleWordsKey(r.path, r.literal)] = r.reason
	}
	return m
}

func roleWordsKey(path, literal string) string { return path + "\x00" + literal }

// roleWordHit is one bare role word in a scanned literal.
type roleWordHit struct {
	line    int
	literal string
	hit     layername.Hit
}

// roleWordsSkipped is whether a literal is no human text: no whitespace (an id, a key, a flag, a field
// path, "parent" as a value) or SQL.
func roleWordsSkipped(literal string) bool {
	if strings.IndexFunc(literal, unicode.IsSpace) < 0 {
		return true
	}
	return roleWordsSQLStatement.MatchString(literal) || roleWordsSQLFragment.MatchString(literal)
}

// roleWordHits is the bare role words in the string literals of one Go source file (path is
// repository-relative), and the nonRoleLiterals keys the file used. Import paths, struct tags and
// comments are not scanned.
func roleWordHits(path string, src []byte) ([]roleWordHit, map[string]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	skip := map[*ast.BasicLit]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			skip[n.Path] = true
		case *ast.Field:
			if n.Tag != nil {
				skip[n.Tag] = true
			}
		}
		return true
	})
	var hits []roleWordHit
	used := map[string]bool{}
	var unquoteErr error
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || skip[lit] {
			return true
		}
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			unquoteErr = fmt.Errorf("%s: %v", fset.Position(lit.Pos()), err)
			return true
		}
		if roleWordsSkipped(text) {
			return true
		}
		found := layername.Find(text)
		if len(found) == 0 {
			return true
		}
		if key := roleWordsKey(path, text); nonRoleLiterals[key] != "" {
			used[key] = true
			return true
		}
		line := fset.Position(lit.Pos()).Line
		for _, h := range found {
			hits = append(hits, roleWordHit{line: line, literal: text, hit: h})
		}
		return true
	})
	return hits, used, unquoteErr
}

// roleWordsScope is the repository-relative path prefixes of CRW_ROLE_WORDS_PATHS; nil is every path.
func roleWordsScope() []string {
	var scope []string
	for _, p := range strings.Split(os.Getenv("CRW_ROLE_WORDS_PATHS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			scope = append(scope, p)
		}
	}
	return scope
}

// inRoleWordsScope is whether path is under one of the scope's prefixes. A prefix ending in '/' is a
// directory (internal/relay/dag/ is not internal/relay/dagsched/); any other prefix is a whole path
// segment, a file or a directory (internal/b matches internal/b and internal/b/x.go, not internal/bridge).
func inRoleWordsScope(scope []string, path string) bool {
	if scope == nil {
		return true
	}
	for _, p := range scope {
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) || path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// roleWordsScan is the scanned files in scope with their hits, and the nonRoleLiterals keys they used.
type roleWordsScan struct {
	hits map[string][]roleWordHit // every scanned file in scope, also those without a hit
	used map[string]bool
}

func (s roleWordsScan) counts() map[string]int {
	counts := map[string]int{}
	for path, hits := range s.hits {
		if len(hits) > 0 {
			counts[path] = len(hits)
		}
	}
	return counts
}

// scanRoleWords parses every non-test Go file under root/internal (outside testdata directories) whose
// path is in scope. A scope with no such file fails the run, except in update mode (update), where it is
// the scope of a deleted or moved file and its baseline lines are dropped.
func scanRoleWords(t *testing.T, root string, scope []string, update bool) roleWordsScan {
	t.Helper()
	scan := roleWordsScan{hits: map[string][]roleWordHit{}, used: map[string]bool{}}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !inRoleWordsScope(scope, rel) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hits, used, err := roleWordHits(rel, src)
		if err != nil {
			return err
		}
		scan.hits[rel] = hits
		for k := range used {
			scan.used[k] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.hits) == 0 && !update {
		t.Fatalf("no Go file under internal/ is in scope %v (a deleted file's line is dropped with CRW_ROLE_WORDS=update)", scope)
	}
	return scan
}

// roleWordsLine is one line of the baseline: a header comment (path empty) or an entry.
type roleWordsLine struct {
	raw   string
	path  string
	count int
}

// parseRoleWordsBaseline reads the baseline text; problems are the lines that break its form (header
// comments first, then path<TAB>count sorted by path, unique, count > 0).
func parseRoleWordsBaseline(text string) (lines []roleWordsLine, entries map[string]int, problems []string) {
	entries = map[string]int{}
	prev := ""
	inHeader := true
	if text == "" {
		return nil, entries, nil
	}
	for i, raw := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(raw, "#") {
			if !inHeader {
				problems = append(problems, fmt.Sprintf("line %d: a comment after the entries", i+1))
			}
			lines = append(lines, roleWordsLine{raw: raw})
			continue
		}
		inHeader = false
		path, num, ok := strings.Cut(raw, "\t")
		count, err := strconv.Atoi(num)
		if !ok || err != nil || path == "" || strings.ContainsAny(path, " \t") {
			problems = append(problems, fmt.Sprintf("line %d: %q is not path<TAB>count", i+1, raw))
			continue
		}
		if count <= 0 {
			problems = append(problems, fmt.Sprintf("line %d: %s has count %d; a file with no hit has no line", i+1, path, count))
		}
		if _, dup := entries[path]; dup {
			problems = append(problems, fmt.Sprintf("line %d: %s is listed twice", i+1, path))
		} else if path < prev {
			problems = append(problems, fmt.Sprintf("line %d: %s is not sorted (after %s)", i+1, path, prev))
		}
		prev = path
		entries[path] = count
		lines = append(lines, roleWordsLine{raw: raw, path: path, count: count})
	}
	return lines, entries, problems
}

// updateRoleWordsBaseline rewrites the entries in scope from counts and keeps every other line
// byte-for-byte. With no file yet (exists false) it writes the header and every count in scope. An
// existing file is never raised: a count above its entry or a new path is a problem and nothing is
// written.
func updateRoleWordsBaseline(old string, exists bool, counts map[string]int, scope []string) (string, []string) {
	var out []string
	if !exists {
		out = strings.Split(strings.TrimSuffix(roleWordsHeader, "\n"), "\n")
		paths := make([]string, 0, len(counts))
		for path, n := range counts {
			if n > 0 && inRoleWordsScope(scope, path) {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		for _, path := range paths {
			out = append(out, path+"\t"+strconv.Itoa(counts[path]))
		}
		return strings.Join(out, "\n") + "\n", nil
	}
	lines, entries, problems := parseRoleWordsBaseline(old)
	if len(problems) > 0 {
		return "", append([]string{"the baseline is not well formed, fix it by hand first:"}, problems...)
	}
	for _, l := range lines {
		if l.path == "" || !inRoleWordsScope(scope, l.path) {
			out = append(out, l.raw)
			continue
		}
		switch n := counts[l.path]; {
		case n > l.count:
			problems = append(problems, fmt.Sprintf("%s has %d bare role words, above its entry %d; the baseline only shrinks", l.path, n, l.count))
		case n > 0:
			out = append(out, l.path+"\t"+strconv.Itoa(n))
		}
	}
	paths := make([]string, 0, len(counts))
	for path, n := range counts {
		if _, listed := entries[path]; !listed && n > 0 && inRoleWordsScope(scope, path) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		problems = append(problems, fmt.Sprintf("%s has %d bare role words and no entry; the baseline takes no new path", path, counts[path]))
	}
	if len(problems) > 0 {
		return "", problems
	}
	return strings.Join(out, "\n") + "\n", nil
}

// staleRoleWordEntries are the entries in scope that differ from the current count, and the files in
// scope with hits and no entry.
func staleRoleWordEntries(entries, counts map[string]int, scope []string) []string {
	var problems []string
	for path, want := range entries {
		if !inRoleWordsScope(scope, path) {
			continue
		}
		switch n := counts[path]; {
		case n < want && n == 0:
			problems = append(problems, fmt.Sprintf("%s has no bare role word left; remove its line (entry %d)", path, want))
		case n < want:
			problems = append(problems, fmt.Sprintf("lower %s to %d (entry %d)", path, n, want))
		case n > want:
			problems = append(problems, fmt.Sprintf("%s has %d bare role words, above its entry %d (see TestRoleWordsInHumanTextNameLayers)", path, n, want))
		}
	}
	for path, n := range counts {
		if _, listed := entries[path]; !listed && n > 0 && inRoleWordsScope(scope, path) {
			problems = append(problems, fmt.Sprintf("%s has %d bare role words and no entry (see TestRoleWordsInHumanTextNameLayers)", path, n))
		}
	}
	sort.Strings(problems)
	return problems
}

// risenRoleWordEntries are the entries of head that rose above base or are new paths.
func risenRoleWordEntries(base, head map[string]int) []string {
	var problems []string
	for path, n := range head {
		was, listed := base[path]
		switch {
		case !listed:
			problems = append(problems, fmt.Sprintf("%s is a new path (count %d) not in the baseline at the merge base", path, n))
		case n > was:
			problems = append(problems, fmt.Sprintf("%s rose from %d to %d against the merge base", path, was, n))
		}
	}
	sort.Strings(problems)
	return problems
}

// readRoleWordsBaseline is the baseline file of root and whether it exists.
func readRoleWordsBaseline(t *testing.T, root string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(roleWordsBaselineFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// roleWordsBaselineEntries is the entries of the baseline text, and the problems that make its counts
// unreliable: the file is missing (exists false) or a line breaks its form.
func roleWordsBaselineEntries(text string, exists bool) (map[string]int, []string) {
	if !exists {
		return map[string]int{}, []string{fmt.Sprintf("%s is missing; create it with CRW_ROLE_WORDS=update go test -tags dev ./internal/dev/ci/ -run TestRoleWordsBaselineOnlyShrinks", roleWordsBaselineFile)}
	}
	_, entries, problems := parseRoleWordsBaseline(text)
	for i, p := range problems {
		problems[i] = roleWordsBaselineFile + " " + p
	}
	return entries, problems
}

// missingRoleWordEntries are the entries in scope whose file does not exist. An entry outside the scope
// is another run's to check, so a scoped update is not failed by a file deleted elsewhere.
func missingRoleWordEntries(root string, entries map[string]int, scope []string) []string {
	var problems []string
	for path := range entries {
		if !inRoleWordsScope(scope, path) {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			problems = append(problems, fmt.Sprintf("lists %s, which does not exist; remove its line", path))
		}
	}
	sort.Strings(problems)
	return problems
}

// fixtureRoleWords is the number of bare role words the scan counts in a Go source fragment.
func fixtureRoleWords(t *testing.T, src string) int {
	t.Helper()
	hits, _, err := roleWordHits("internal/fixture/fixture.go", []byte("package fixture\n\n"+src+"\n"))
	if err != nil {
		t.Fatalf("fixture %q: %v", src, err)
	}
	return len(hits)
}

func TestRoleWordsInHumanTextNameLayers(t *testing.T) {
	t.Run("fixture: bare role words in human text are counted", func(t *testing.T) {
		for _, lit := range []string{
			"ask the parent",
			"the store-scope supervisor",
			"Parent sessions wait",
			"a parent-authored note",
			"a parent-made refresh",
			"is bound as 'supervisor' and",
			"role 'supervisor' cannot declare",
			"holds live bindings at ['child', 'parent']; one",
			"holds live bindings at ['child', 'parent'] (자식, issue child); one",
			"the parent's turn",
			"two supervisors above",
			"the parent (부모) is wrong",
			"UPDATE failed; ask the parent to retry",
			"ask the parent to UPDATE the record",
			"the record FROM the parent is stale",
			"no binding has role = 'parent' here, so ask the 부모",
		} {
			if n := fixtureRoleWords(t, "var _ = "+strconv.Quote(lit)); n == 0 {
				t.Errorf("%q: no hit, want the bare role word counted", lit)
			}
			if n := fixtureRoleWords(t, "var _ = `"+lit+"`"); n == 0 {
				t.Errorf("raw `%s`: no hit, want the bare role word counted", lit)
			}
		}
		if n := fixtureRoleWords(t, `var _ = "the parent and the supervisor"`); n != 2 {
			t.Errorf("two role words in one literal counted %d, want 2", n)
		}
	})
	t.Run("fixture: ids, flags, typed forms, git senses, tags, SQL and comments are not counted", func(t *testing.T) {
		for _, src := range []string{
			`var _ = "parent"`,
			`var _ = "--role supervisor"`,
			`var _ = "pass --authority parent=ref"`,
			`var _ = "write supervisor (부모) here"`,
			`var _ = "the role 'parent' (자식) is"`,
			`var _ = "holds live bindings at ['child', 'parent'] (issue child, 자식); one"`,
			`var _ = "holds live bindings at [\"child\",\"parent\"] (issue child, 자식); one"`,
			`var _ = "walk first-parent order"`,
			`var _ = "a two-parent merge"`,
			`var _ = "a parent-class decision"`,
			`var _ = "pass --parent-task T"`,
			`var _ = "parent_task_id is set"`,
			`var _ = "the first parent of the merge"`,
			`var _ = "the parent revision is"`,
			"type T struct {\n\tA string `help:\"ask the parent\"`\n}",
			`var _ = "SELECT task_id FROM scope_bindings WHERE role = 'parent'"`,
			`const liveParent = "role = 'parent' AND scope_kind = 'project' AND scope_key = ? AND task_id = ? AND status IN ('active','paused') AND superseded_by IS NULL"`,
			`var _ = " AND orb.role = 'parent' AND orb.scope_kind = 'project' AND orb.status IN ('active','paused') AND orb.superseded_by IS NULL)"`,
			`var _ = "  WHERE task_id = ? AND role = 'parent'"`,
			`var _ = " JOIN scope_bindings own ON own.task_id = d.decided_by AND own.role = 'parent'"`,
			`var _ = "SELECT r.relationship_id AS rid, r.parent_task_id AS parent FROM relationships r"`,
			`var _ = "UPDATE scope_bindings SET status = 'ended' WHERE role = 'parent'"`,
			`var _ = "DELETE FROM scope_bindings WHERE role = 'supervisor'"`,
			`var _ = "INSERT INTO scope_bindings (role, task_id) VALUES ('parent', ?)"`,
			"var _ = `\n\tWITH live AS (SELECT task_id FROM scope_bindings WHERE role = 'parent')\n\tSELECT task_id FROM live`",
			"// ask the parent\nvar _ = 1",
			`import _ "fmt"`,
		} {
			if n := fixtureRoleWords(t, src); n != 0 {
				t.Errorf("%s: %d hits, want none", src, n)
			}
		}
	})
	t.Run("fixture: a nonRoleLiterals entry is not counted", func(t *testing.T) {
		key := roleWordsKey("internal/fixture/fixture.go", "the parent of the tip")
		nonRoleLiterals[key] = "fixture"
		defer delete(nonRoleLiterals, key)
		if n := fixtureRoleWords(t, `var _ = "the parent of the tip"`); n != 0 {
			t.Errorf("an allowed literal counted %d hits", n)
		}
		if n := fixtureRoleWords(t, `var _ = "the parent of the base"`); n != 1 {
			t.Errorf("a literal not in nonRoleLiterals counted %d hits, want 1", n)
		}
	})

	root := repoRoot()
	scope := roleWordsScope()
	scan := scanRoleWords(t, root, scope, os.Getenv("CRW_ROLE_WORDS") == "update")
	text, exists := readRoleWordsBaseline(t, root)
	entries, problems := roleWordsBaselineEntries(text, exists)
	if len(problems) > 0 {
		t.Fatalf("the baseline cannot be read, so no count is checked against it:\n%s", strings.Join(problems, "\n"))
	}
	paths := make([]string, 0, len(scan.hits))
	for path := range scan.hits {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	over := false
	for _, path := range paths {
		hits := scan.hits[path]
		if len(hits) <= entries[path] {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %d bare role words, the baseline allows %d:", path, len(hits), entries[path])
		for _, h := range hits {
			fmt.Fprintf(&b, "\n\tline %d: %q in %q", h.line, h.hit.Word, h.literal)
		}
		t.Error(b.String())
		over = true
	}
	if over {
		t.Log("write 부모 for the relay supervisor and 자식 for the relay parent in running text; where the role id is a value, " +
			"write the typed form supervisor (부모) / parent (자식) (layername.Name, Gloss, Typed, ListGloss)")
	}
	for key, reason := range nonRoleLiterals {
		path, literal, _ := strings.Cut(key, "\x00")
		if inRoleWordsScope(scope, path) && !scan.used[key] {
			t.Errorf("nonRoleLiterals entry %s %q (%s) matches no scanned literal with a hit; remove it", path, literal, reason)
		}
	}
}

func TestRoleWordsBaselineOnlyShrinks(t *testing.T) {
	t.Run("fixture: the scope and update mode", func(t *testing.T) {
		old := "# header\n" +
			"internal/a/a.go\t3\n" +
			"internal/b/b.go\t5\n" +
			"internal/bb/b.go\t2\n"
		scope := []string{"internal/b/"}
		counts := map[string]int{"internal/a/a.go": 1, "internal/b/b.go": 4, "internal/bb/b.go": 9}
		got, problems := updateRoleWordsBaseline(old, true, counts, scope)
		if len(problems) > 0 {
			t.Fatalf("scoped update: %v", problems)
		}
		if want := "# header\ninternal/a/a.go\t3\ninternal/b/b.go\t4\ninternal/bb/b.go\t2\n"; got != want {
			t.Errorf("scoped update =\n%s\nwant (lines outside internal/b/ byte-equal)\n%s", got, want)
		}
		if stale := staleRoleWordEntries(map[string]int{"internal/a/a.go": 3, "internal/b/b.go": 4}, counts, scope); len(stale) > 0 {
			t.Errorf("a stale-high line outside the scope failed a scoped run: %v", stale)
		}
		for _, c := range []struct {
			prefix, path string
			in           bool
		}{
			{"internal/relay/dag/", "internal/relay/dag/epoch.go", true},
			{"internal/relay/dag/", "internal/relay/dagsched/epoch.go", false},
			{"internal/relay/dag", "internal/relay/dag/epoch.go", true},
			{"internal/relay/dag", "internal/relay/dagsched/epoch.go", false},
			{"internal/b", "internal/bridge/x.go", false},
			{"internal/relay/cli/decisions.go", "internal/relay/cli/decisions.go", true},
			{"internal/relay/cli/decisions.go", "internal/relay/cli/decisions_more.go", false},
		} {
			if got := inRoleWordsScope([]string{c.prefix}, c.path); got != c.in {
				t.Errorf("scope %q holds %s = %v, want %v", c.prefix, c.path, got, c.in)
			}
		}
		if stale := staleRoleWordEntries(map[string]int{"internal/a/a.go": 3}, counts, nil); len(stale) == 0 {
			t.Error("an unscoped run passed stale and missing entries")
		}
		if got, _ := updateRoleWordsBaseline(old, true, map[string]int{"internal/a/a.go": 3}, scope); got != "# header\ninternal/a/a.go\t3\ninternal/bb/b.go\t2\n" {
			t.Errorf("a file with no hit left keeps its line:\n%s", got)
		}
		if _, problems := updateRoleWordsBaseline(old, true, map[string]int{"internal/b/b.go": 6}, scope); len(problems) == 0 {
			t.Error("update raised an entry")
		}
		if _, problems := updateRoleWordsBaseline(old, true, map[string]int{"internal/b/c.go": 1}, scope); len(problems) == 0 {
			t.Error("update added a path")
		}
		created, _ := updateRoleWordsBaseline("", false, map[string]int{"internal/z.go": 2, "internal/a.go": 1, "internal/n.go": 0}, nil)
		if !strings.HasSuffix(created, "\ninternal/a.go\t1\ninternal/z.go\t2\n") || !strings.HasPrefix(created, "#") {
			t.Errorf("a new baseline =\n%s", created)
		}
		if _, _, problems := parseRoleWordsBaseline(created); len(problems) > 0 {
			t.Errorf("a new baseline is not well formed: %v", problems)
		}
		if _, _, problems := parseRoleWordsBaseline("internal/b.go\t1\ninternal/a.go\t1\ninternal/a.go\t2\ninternal/c.go\t0\nnot a line\n"); len(problems) != 4 {
			t.Errorf("malformed baseline problems = %v, want four", problems)
		}
		if risen := risenRoleWordEntries(map[string]int{"internal/a.go": 2}, map[string]int{"internal/a.go": 3, "internal/b.go": 1}); len(risen) != 2 {
			t.Errorf("rises against the merge base = %v, want two", risen)
		}
	})
	t.Run("fixture: a scoped update of a deleted file drops its line", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "internal", "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "internal", "a", "a.go"), []byte("package a\n\nvar _ = \"ask the parent\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := "# header\ninternal/a/a.go\t1\ninternal/gone/gone.go\t2\ninternal/old/old.go\t3\n"
		gone := []string{"internal/gone/gone.go"}
		scan := scanRoleWords(t, root, gone, true)
		if len(scan.hits) != 0 {
			t.Fatalf("a scope of a deleted file scanned %v", scan.hits)
		}
		updated, problems := updateRoleWordsBaseline(old, true, scan.counts(), gone)
		if len(problems) > 0 {
			t.Fatalf("update of a deleted file's scope: %v", problems)
		}
		if want := "# header\ninternal/a/a.go\t1\ninternal/old/old.go\t3\n"; updated != want {
			t.Errorf("update of a deleted file's scope =\n%s\nwant its line dropped and the rest byte-equal\n%s", updated, want)
		}
	})
	t.Run("fixture: a scoped run checks only the listed files in its scope exist", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "internal", "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "internal", "a", "a.go"), []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		entries := map[string]int{"internal/a/a.go": 1, "internal/old/old.go": 3}
		if missing := missingRoleWordEntries(root, entries, []string{"internal/a/"}); len(missing) != 0 {
			t.Errorf("a scoped run failed on a deleted file outside its scope: %v", missing)
		}
		if missing := missingRoleWordEntries(root, entries, nil); len(missing) != 1 || !strings.Contains(missing[0], "internal/old/old.go") {
			t.Errorf("an unscoped run reports missing files %v, want internal/old/old.go", missing)
		}
		if missing := missingRoleWordEntries(root, entries, []string{"internal/old/"}); len(missing) != 1 {
			t.Errorf("a scoped run reports missing files in its scope %v, want internal/old/old.go", missing)
		}
	})
	t.Run("fixture: a missing or malformed baseline is reported as such", func(t *testing.T) {
		if _, problems := roleWordsBaselineEntries("", false); len(problems) != 1 || !strings.Contains(problems[0], "missing") {
			t.Errorf("a missing baseline reads as %v, want one problem naming it missing", problems)
		}
		entries, problems := roleWordsBaselineEntries("# header\ninternal/a/a.go 3\ninternal/b/b.go\t2\n", true)
		if len(problems) != 1 || !strings.Contains(problems[0], "line 2") {
			t.Errorf("a malformed baseline reads as %v, want its line 2", problems)
		}
		if entries["internal/b/b.go"] != 2 {
			t.Errorf("the well-formed entries of a malformed baseline = %v", entries)
		}
		if _, problems := roleWordsBaselineEntries("# header\ninternal/a/a.go\t3\n", true); len(problems) != 0 {
			t.Errorf("a well-formed baseline reads as %v", problems)
		}
	})

	root := repoRoot()
	scope := roleWordsScope()
	update := os.Getenv("CRW_ROLE_WORDS") == "update"
	scan := scanRoleWords(t, root, scope, update)
	counts := scan.counts()
	text, exists := readRoleWordsBaseline(t, root)
	if update {
		updated, problems := updateRoleWordsBaseline(text, exists, counts, scope)
		if len(problems) > 0 {
			t.Fatalf("update refused:\n%s", strings.Join(problems, "\n"))
		}
		if updated != text {
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(roleWordsBaselineFile)), []byte(updated), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", roleWordsBaselineFile)
		}
		text, exists = updated, true
	}
	if !exists {
		t.Fatalf("%s is missing; create it with CRW_ROLE_WORDS=update go test -tags dev ./internal/dev/ci/ -run TestRoleWordsBaselineOnlyShrinks", roleWordsBaselineFile)
	}
	_, entries, problems := parseRoleWordsBaseline(text)
	for _, p := range problems {
		t.Errorf("%s %s", roleWordsBaselineFile, p)
	}
	for _, p := range missingRoleWordEntries(root, entries, scope) {
		t.Errorf("%s %s", roleWordsBaselineFile, p)
	}
	for _, p := range staleRoleWordEntries(entries, counts, scope) {
		t.Errorf("%s: %s", roleWordsBaselineFile, p)
	}

	base, err := exec.Command("git", "-C", root, "merge-base", "HEAD", "origin/dev").Output()
	if err != nil {
		t.Logf("no merge base with origin/dev (%v); the no-rise check is skipped", err)
		return
	}
	baseText, err := exec.Command("git", "-C", root, "show", strings.TrimSpace(string(base))+":"+roleWordsBaselineFile).Output()
	if err != nil {
		t.Logf("%s is not at the merge base %s (first introduction); the no-rise check is skipped", roleWordsBaselineFile, strings.TrimSpace(string(base)))
		return
	}
	_, baseEntries, _ := parseRoleWordsBaseline(string(baseText))
	for _, p := range risenRoleWordEntries(baseEntries, entries) {
		t.Errorf("%s: %s", roleWordsBaselineFile, p)
	}
}
