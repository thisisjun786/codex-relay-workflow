//go:build dev

package ci

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// CRW-1555 (N3-7): the words "issue child" are gone from the Go text a person reads, as CRW-1553 took them out of docs/
// and plugins/ (TestDocsAndPluginsNameNoIssueChild, the same words and the same spellings: issueChildWord). The relay
// role id `child` is named by its id ("a `child` session"), never by a layer name, and no new short name is made for it.
//
// What is read. Every Go file under internal/ and cmd/ that is not a test file and is not under a testdata directory is
// parsed. Two kinds of text are read: each string literal (an interpreted or a raw one, read as its value, so an escaped
// "\n" between the words counts) and each comment (a comment group is read as one text, the comment markers dropped, so
// the words broken across two comment lines count). Go identifiers (IssueChild, legacyIssueChild) are no text and join no
// words, so they are not read. Test files are not read: they pin the text and carry the removed words as fixtures.
//
// The exception list below is the only place the words may stay. A line is excepted by its file and its whole trimmed
// source line (not by a line number, so a moved line keeps its entry), and every entry carries the reason the line is
// not rewritten. An entry whose line is gone, or whose line no longer holds the words, is stale and fails, so the list
// cannot outlive the text it covers.

// goIssueChildException is one source line of a Go file that keeps the words.
type goIssueChildException struct{ file, line, reason string }

// goIssueChildExceptions are the lines that keep the words, each with its reason.
var goIssueChildExceptions = []goIssueChildException{
	{"internal/flow/cli/schedrelay.go",
		`request.Instructions = fmt.Sprintf("You are the issue child of node %s (issue %s) of DAG plan %s, released by flow run %s under design revision %s (%s; sized for a child: %s). "+`,
		"stored record: these instructions become the prompt of the managed-start request that dagsched.assemble freezes into the release intent (the request, its sha256 and its bytes are stored in the dag release row), so a release that is continued after an upgrade must rebuild the same bytes; the text is the retired path's and stays as it was written"},
}

// goTextHit is one place in a Go file where the words stand in text a person reads.
type goTextHit struct {
	line int    // 1-based source line of the words
	kind string // "string" or "comment"
}

// goIssueChildHits is each place of src (a Go source file) where a string literal or a comment holds the words.
func goIssueChildHits(name string, src []byte) ([]goTextHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var hits []goTextHit
	seen := map[string]bool{}
	add := func(line int, kind string) {
		key := fmt.Sprintf("%d %s", line, kind)
		if !seen[key] {
			seen[key] = true
			hits = append(hits, goTextHit{line: line, kind: kind})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		first := fset.Position(lit.Pos()).Line
		for _, m := range issueChildWord.FindAllStringIndex(value, -1) {
			// A raw literal keeps its line breaks, so the words stand on the line they start on; an interpreted
			// literal is one source line.
			if strings.HasPrefix(lit.Value, "`") {
				add(first+strings.Count(value[:m[0]], "\n"), "string")
			} else {
				add(first, "string")
			}
		}
		return true
	})
	for _, group := range file.Comments {
		var lines []string
		for _, c := range group.List {
			text := c.Text
			switch {
			case strings.HasPrefix(text, "//"):
				lines = append(lines, strings.TrimPrefix(text, "//"))
			default:
				body := strings.TrimSuffix(strings.TrimPrefix(text, "/*"), "*/")
				lines = append(lines, strings.Split(body, "\n")...)
			}
		}
		joined := strings.Join(lines, "\n")
		first := fset.Position(group.Pos()).Line
		for _, m := range issueChildWord.FindAllStringIndex(joined, -1) {
			add(first+strings.Count(joined[:m[0]], "\n"), "comment")
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].line < hits[j].line || hits[i].line == hits[j].line && hits[i].kind < hits[j].kind
	})
	return hits, nil
}

func TestGoTextNamesNoIssueChild(t *testing.T) {
	root := repoRoot()
	excepted := map[string]map[string]string{}
	for _, e := range goIssueChildExceptions {
		if strings.TrimSpace(e.reason) == "" {
			t.Errorf("exception %s %q has no reason", e.file, e.line)
		}
		if excepted[e.file] == nil {
			excepted[e.file] = map[string]string{}
		}
		excepted[e.file][e.line] = e.reason
	}
	used := map[string]map[string]bool{}
	var problems []string
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
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
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			hits, err := goIssueChildHits(rel, data)
			if err != nil {
				return err
			}
			lines := strings.Split(string(data), "\n")
			var left []string
			for _, h := range hits {
				line := strings.TrimSpace(lines[h.line-1])
				if _, ok := excepted[rel][line]; ok {
					if used[rel] == nil {
						used[rel] = map[string]bool{}
					}
					used[rel][line] = true
					continue
				}
				left = append(left, fmt.Sprintf("line %d (%s): %s", h.line, h.kind, line))
			}
			if len(left) > 0 {
				shown := left
				if len(shown) > 3 {
					shown = shown[:3]
				}
				problems = append(problems, fmt.Sprintf("%s: %d place(s) still name an issue child, as in\n    %s", rel, len(left), strings.Join(shown, "\n    ")))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range goIssueChildExceptions {
		if !used[e.file][e.line] {
			problems = append(problems, fmt.Sprintf("stale exception: %s no longer holds the line %q with the words", e.file, e.line))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("Go text under internal/ and cmd/ still names an \"issue child\" (%d file(s) and exceptions); name the relay role id `child` by its id (\"a `child` session\") and make no layer name of it, or list the line in goIssueChildExceptions with the reason:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}

	t.Run("fixture: strings and comments are read and identifiers are not", func(t *testing.T) {
		src := "package p\n\n" +
			"// a legacy issue-child run\n" +
			"var a = \"an issue child\"\n" +
			"var b = \"issue\\nchild\"\n" +
			"var c = `x\nIssue Child`\n" +
			"// issue\n// children\n" +
			"/* block\nissue child */\n" +
			"var legacyIssueChild = \"the `child` session\" // a comment about a child issue\n" +
			"func IssueChild() {}\n"
		hits, err := goIssueChildHits("p.go", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range hits {
			got = append(got, fmt.Sprintf("%d %s", h.line, h.kind))
		}
		want := []string{"3 comment", "4 string", "5 string", "7 string", "8 comment", "11 comment"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("hits = %v, want %v", got, want)
		}
	})
}
