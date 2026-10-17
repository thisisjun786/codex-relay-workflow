//go:build dev

package ci

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1634: emitted skill references must resolve after CRW-1526 moves the folders.
// Inspect string literals, not historical comments or recorded test inputs.
func TestRuntimeSkillReferencesResolve(t *testing.T) {
	root := repoRoot()
	ref := regexp.MustCompile(`(?:\$crw:|crw:|\$)(crw-[a-z0-9]+(?:-[a-z0-9]+)*)`)
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" || rel == "internal/affordance" {
				// CRW-1528 owns affordance text; its explicit separate write scope
				// is excluded from CRW-1634, as are oracle fixtures.
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// These recognizers intentionally accept old installed names too.
		if rel == "internal/pabcd/hook/detect.go" || rel == "internal/relay/reception/packet.go" {
			return nil
		}
		// These development harnesses generate synthetic invalid skill inputs;
		// neither emits an instruction for a production agent to load a skill.
		if rel == "internal/dev/cxcfuzz/target_spawn.go" || rel == "internal/dev/laneparity/fire.go" {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range ref.FindAllStringSubmatch(value, -1) {
				seen++
				if _, err := os.Stat(filepath.Join(root, "plugins/crw/skills", match[1], "SKILL.md")); err != nil {
					t.Errorf("%s:%d references unavailable skill %s: %v", rel, fset.Position(lit.Pos()).Line, match[1], err)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("no runtime skill reference inspected")
	}
}
