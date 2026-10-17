//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

const dispositionModule = "github.com/thisisjun786/codex-relay-workflow/"

type dispositionRow struct {
	kind, target, action string
}

// The table is the decision input; a missing, incomplete or malformed table cannot
// become a vacuous "nothing imports a deleted package" pass.
func parseDisposition(text string) ([]dispositionRow, error) {
	var rows []dispositionRow
	seen := map[string]bool{}
	header := []string{"Kind", "Target", "Disposition", "Current use and evidence", "Destination or removal condition"}
	for _, table := range markdownTables(text) {
		if len(table) == 0 || len(table[0]) == 0 || table[0][0] != "Kind" {
			continue
		}
		if !slices.Equal(table[0], header) {
			return nil, fmt.Errorf("invalid disposition table header: %q", table[0])
		}
		for _, cells := range table[1:] {
			if len(cells) != len(header) {
				return nil, fmt.Errorf("invalid disposition row: %q", cells)
			}
			row := dispositionRow{cells[0], strings.Trim(cells[1], "`"), cells[2]}
			if row.kind != "package" && row.kind != "command" {
				return nil, fmt.Errorf("unknown disposition kind %q", row.kind)
			}
			if row.action != "keep" && row.action != "merge" && row.action != "delete" {
				return nil, fmt.Errorf("unknown disposition %q for %s", row.action, row.target)
			}
			key := row.kind + ":" + row.target
			if row.target == "" || seen[key] || cells[3] == "" || cells[4] == "" {
				return nil, fmt.Errorf("missing or duplicate disposition evidence: %q", cells)
			}
			seen[key] = true
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no disposition rows")
	}
	return rows, nil
}

type dispositionPackage struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// Read the compiler's package metadata, not textual mentions in comments,
// fixtures or prompts. Test imports are deliberately separate from Imports.
func dispositionPackages(t *testing.T) []dispositionPackage {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", "-tags=dev,integration", "./...")
	cmd.Dir = repoRoot()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var packages []dispositionPackage
	for {
		var p dispositionPackage
		if err := decoder.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("go list JSON: %v", err)
		}
		packages = append(packages, p)
	}
	if len(packages) == 0 {
		t.Fatal("go list returned no packages")
	}
	return packages
}

func dispositionProblems(rows []dispositionRow, packages []dispositionPackage, verbs []string) []string {
	known := map[string]bool{}
	for _, p := range packages {
		if strings.HasPrefix(p.ImportPath, dispositionModule+"internal/pabcd/") {
			known["package:"+strings.TrimPrefix(p.ImportPath, dispositionModule)] = true
		}
	}
	for _, verb := range verbs {
		known["command:crw pabcd "+verb] = true
	}
	var problems []string
	for _, row := range rows {
		key := row.kind + ":" + row.target
		if !known[key] {
			problems = append(problems, "unknown or duplicate disposition target "+key)
			continue
		}
		delete(known, key)
		if row.action != "delete" {
			continue
		}
		if row.kind == "command" {
			problems = append(problems, row.target+" is still registered in harness.Verbs")
			continue
		}
		target := dispositionModule + row.target
		for _, p := range packages {
			// Conservatively include every non-test importer, including the old
			// CLI, hooks and developer tools. Deleting a parent must also account
			// for product imports of its nested packages.
			for _, imp := range p.Imports {
				if imp == target || strings.HasPrefix(imp, target+"/") {
					problems = append(problems, strings.TrimPrefix(p.ImportPath, dispositionModule)+" imports "+strings.TrimPrefix(imp, dispositionModule)+" marked delete")
				}
			}
		}
	}
	for missing := range known {
		problems = append(problems, "missing disposition for "+missing)
	}
	slices.Sort(problems)
	return problems
}

func TestRebrandDispositionMatchesProductImports(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "docs/rebrand/disposition.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseDisposition(string(data))
	if err != nil {
		t.Fatal(err)
	}
	packages := dispositionPackages(t)
	var verbs []string
	for _, verb := range harness.Verbs() {
		verbs = append(verbs, verb.Name)
	}
	if problems := dispositionProblems(rows, packages, verbs); len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
	// A table with no delete decisions is valid, but must not hide a broken
	// import guard. Use the real graph to reject deletion of flow's source owner.
	mutated := slices.Clone(rows)
	found := false
	for i := range mutated {
		if mutated[i].kind == "package" && mutated[i].target == "internal/pabcd/source" {
			mutated[i].action = "delete"
			found = true
		}
	}
	if !found {
		t.Fatal("the shared source package has no disposition")
	}
	problems := dispositionProblems(mutated, packages, verbs)
	if !slices.Contains(problems, "internal/flow/engine imports internal/pabcd/source marked delete") {
		t.Fatalf("deleting flow's source dependency was not rejected: %v", problems)
	}
}

func TestRebrandDispositionGuardSeparatesTestImports(t *testing.T) {
	const target = "internal/pabcd/example"
	rows := []dispositionRow{{"package", target, "delete"}}
	packages := []dispositionPackage{
		{ImportPath: dispositionModule + target},
		{
			ImportPath:   dispositionModule + "internal/consumer",
			TestImports:  []string{dispositionModule + target},
			XTestImports: []string{dispositionModule + target},
		},
	}
	if problems := dispositionProblems(rows, packages, nil); len(problems) != 0 {
		t.Fatalf("test-only imports prevent deletion: %v", problems)
	}
	packages[1].Imports = []string{dispositionModule + target}
	if problems := dispositionProblems(rows, packages, nil); len(problems) != 1 {
		t.Fatalf("product importer was not rejected: %v", problems)
	}
	packages[1].Imports = []string{dispositionModule + target + "/nested"}
	if problems := dispositionProblems(rows, packages, nil); len(problems) != 1 {
		t.Fatalf("nested product importer was not rejected: %v", problems)
	}
	if problems := dispositionProblems([]dispositionRow{{"command", "crw pabcd example", "delete"}}, nil, []string{"example"}); len(problems) != 1 {
		t.Fatalf("registered command was not rejected: %v", problems)
	}
}

func TestRebrandDispositionRejectsMissingOrMalformedRows(t *testing.T) {
	header := "| Kind | Target | Disposition | Current use and evidence | Destination or removal condition |\n| --- | --- | --- | --- | --- |\n"
	row := "| package | `internal/pabcd/example` | keep | caller | boundary |\n"
	for _, text := range []string{"", header, header + strings.Replace(row, "keep", "retire", 1), header + row + row, header + strings.Replace(row, "caller", "", 1)} {
		if _, err := parseDisposition(text); err == nil {
			t.Errorf("accepted missing or malformed rows: %q", text)
		}
	}
	rows, err := parseDisposition(header + row)
	if err != nil || len(rows) != 1 {
		t.Fatalf("valid table: %v, %v", rows, err)
	}
	packages := []dispositionPackage{{ImportPath: dispositionModule + "internal/pabcd/example"}}
	if problems := dispositionProblems(nil, packages, []string{"example"}); len(problems) != 2 {
		t.Fatalf("missing package and command were not rejected: %v", problems)
	}
}
