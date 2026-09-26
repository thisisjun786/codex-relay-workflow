//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const opsReferences = "plugins/crw/skills/crw-run/references"

// opsCopy copies this repository's operations contract and fixtures into a scratch root.
func opsCopy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"operations.md", "operations/check-result.example.json",
		"operations/compatibility-record.example.json", "operations/installation-plan.example.md",
		"operations/scenarios.md"} {
		data, err := os.ReadFile(filepath.Join(repoRoot(), opsReferences, rel))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, opsReferences, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func editOps(t *testing.T, root, rel string, edit func(string) string) {
	t.Helper()
	path := filepath.Join(root, opsReferences, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := edit(string(data))
	if changed == string(data) {
		t.Fatalf("edit of %s changed nothing", rel)
	}
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
}

func opsParity(t *testing.T, label, root string) result {
	t.Helper()
	py := python(t, root, nil, "scripts/check_operations_contract.py", "--root", root)
	got := goCheck(t, root, nil, "operations", "--root", root)
	sameResult(t, label, py, got)
	return got
}

// The operations checker has no property of its own in the todo-47 list: it is ported as part
// of `crw-dev ci contracts` (GATE-8 places that check in validate). These tests pin its
// parity on the repository and on failing fixtures.
func Test47_OperationsContractParity(t *testing.T) {
	if got := opsParity(t, "repository", opsCopy(t)); got.code != 0 {
		t.Fatalf("clean copy: %+v", got)
	}
	for _, row := range []struct {
		label, rel string
		edit       func(string) string
		fragment   string
	}{
		{"uncited clause", "operations/scenarios.md", func(s string) string { return strings.ReplaceAll(s, "OPS-2.3", "OPS-2.x") },
			"is never exercised by a fixture"},
		{"undefined citation", "operations/scenarios.md", func(s string) string { return strings.Replace(s, "OPS-2.3", "OPS-99.1", 1) },
			"fixtures cite OPS-99.1, which the contract does not define"},
		{"missing part", "operations/scenarios.md", func(s string) string { return strings.Replace(s, "Preserved:", "Kept:", 1) },
			"part"},
		{"short part", "operations/scenarios.md", func(s string) string {
			return strings.Replace(s, "Observed: no relay console script, no MCP registration, no state directory, and the current CRW skills not\nyet linked.", "Observed: none.", 1)
		}, "scenario S1 states its Observed part in fewer than 40 characters"},
		{"few scenarios", "operations/scenarios.md", func(s string) string { return strings.ReplaceAll(s, "\n## S", "\n## X") },
			"scenarios found"},
		{"bad json", "operations/check-result.example.json", func(s string) string { return s[:len(s)/2] },
			"an example record is not valid JSON: "},
		{"field value", "operations/check-result.example.json", func(s string) string { return strings.Replace(s, `"verified"`, `"maybe"`, 1) },
			"has an undeclared value"},
		{"revision length", "operations/compatibility-record.example.json", func(s string) string {
			return strings.Replace(s, `"revision": "`, `"revision": "x`, 1)
		}, "revision is not a full 40 character commit id"},
		{"no unmeasured", "operations/compatibility-record.example.json", func(s string) string {
			return strings.Replace(s, `"unmeasured":`, `"unmeasured_":`, 1)
		}, "does not say what is unmeasured"},
		{"table field", "operations.md", func(s string) string {
			i := strings.Index(s, "### OPS-6.1")
			j := i + strings.Index(s[i:], "\n| `")
			return s[:j] + "\n| `extraField` | x |" + s[j:]
		}, "do not match OPS-6.1"},
	} {
		root := opsCopy(t)
		editOps(t, root, row.rel, row.edit)
		got := opsParity(t, row.label, root)
		if got.code != 1 || (row.fragment != "" && !strings.Contains(got.stderr, row.fragment)) {
			t.Errorf("%s: %+v", row.label, got)
		}
	}
	root := t.TempDir()
	got := opsParity(t, "missing", root)
	if got.code != 1 || !strings.HasPrefix(got.stderr, "MISSING ") {
		t.Errorf("missing: %+v", got)
	}
}

func TestOperationsUnicodeClauseBoundaryParity(t *testing.T) {
	root := opsCopy(t)
	editOps(t, root, "operations.md", func(s string) string {
		return s + "\n### OPS-٢.٣ Unicode clause\n## OPS-٤ Unicode section\n### OPS-٩.١suffix Not a clause\n"
	})
	editOps(t, root, "operations/scenarios.md", func(s string) string {
		return s + "\nCitation: OPS-٢.٣ and OPS-٤\n"
	})
	got := opsParity(t, "unicode headings and citations", root)
	if got.code != 0 {
		t.Fatalf("Unicode clauses must be defined and cited: %+v", got)
	}
}

// contractsRepo is a Git checkout holding contracts.py and the named files (empty).
func contractsRepo(t *testing.T, present ...string) *fixtureRepo {
	t.Helper()
	r := newRepo(t)
	script, err := os.ReadFile(filepath.Join(repoRoot(), "scripts/ci/contracts.py"))
	if err != nil {
		t.Fatal(err)
	}
	r.write("scripts/ci/contracts.py", string(script))
	for _, path := range present {
		r.write(path, "")
	}
	r.commit()
	return r
}

func Test47_ContractsPairsAndAbsentComponents(t *testing.T) {
	// No component present: every check reports it claims no coverage.
	r := contractsRepo(t)
	py := runCommand(t, r.root, nil, "python3", "scripts/ci/contracts.py")
	got := goCheck(t, r.root, nil, "contracts")
	sameResult(t, "all absent", py, got)
	if got.code != 0 || strings.Count(got.stdout, "component absent; no coverage claimed") != len(contractChecks) {
		t.Errorf("all absent: %+v", got)
	}
	// A contract without its checker (or the reverse) is refused.
	for _, pair := range [][]string{
		{"plugins/crw/skills/crw-run/references/hook-contract.md"},
		{"plugins/crw/skills/crw-run/scripts/start_policy.py"},
		{"scripts/crw_runtime/components.json"},
	} {
		r := contractsRepo(t, pair...)
		py := runCommand(t, r.root, nil, "python3", "scripts/ci/contracts.py")
		got := goCheck(t, r.root, nil, "contracts")
		sameResult(t, pair[0], py, got)
		if got.code != 1 || !strings.HasPrefix(got.stderr, "Incomplete ") {
			t.Errorf("%s: %+v", pair[0], got)
		}
	}
	// This repository: every present pair runs and passes, output identical.
	root := repoRoot()
	sameResult(t, "repository", runCommand(t, root, nil, "python3", "scripts/ci/contracts.py"), goCheck(t, root, nil, "contracts"))
}

// contractsParityExit runs contracts.py and `crw-dev ci contracts` in root and requires the
// same exit status; stdout and stderr are returned for the caller's own checks.
func contractsParityExit(t *testing.T, label, root string) (result, result) {
	t.Helper()
	py := runCommand(t, root, nil, "python3", "scripts/ci/contracts.py")
	got := goCheck(t, root, nil, "contracts")
	if py.code != got.code {
		t.Errorf("%s: python exit %d, go exit %d\npython stderr: %s\ngo stderr: %s", label, py.code, got.code, py.stderr, got.stderr)
	}
	return py, got
}

func Test47_ContractsOperationsPairAndResult(t *testing.T) {
	// The operations checker is ported, but its pair is judged by the same rule contracts.py
	// uses: checker script present without its contract is an incomplete pair.
	r := contractsRepo(t, "scripts/check_operations_contract.py")
	py := runCommand(t, r.root, nil, "python3", "scripts/ci/contracts.py")
	got := goCheck(t, r.root, nil, "contracts")
	sameResult(t, "operations script without contract", py, got)
	expectEqual(t, "operations incomplete", got.code, 1)
	if !strings.Contains(got.stderr, "Incomplete operations contract/check pair") {
		t.Errorf("operations incomplete: %+v", got)
	}

	// A failing operations replay fails contracts in both implementations. Python ends in a
	// CalledProcessError traceback, so only the exit and the checker's own FAIL lines compare.
	root := opsCopy(t)
	editOps(t, root, "operations/scenarios.md", func(s string) string { return strings.Replace(s, "OPS-2.3", "OPS-99.1", 1) })
	scriptData, err := os.ReadFile(filepath.Join(repoRoot(), "scripts/check_operations_contract.py"))
	if err != nil {
		t.Fatal(err)
	}
	contractsData, err := os.ReadFile(filepath.Join(repoRoot(), "scripts/ci/contracts.py"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fixtureRepo{t, root}
	fr.git("init", "-q")
	fr.write("scripts/check_operations_contract.py", string(scriptData))
	fr.write("scripts/ci/contracts.py", string(contractsData))
	pyFail, goFail := contractsParityExit(t, "failing operations replay", root)
	expectEqual(t, "failing replay exit", goFail.code, 1)
	const line = "FAIL fixtures cite OPS-99.1, which the contract does not define"
	for label, r := range map[string]result{"python": pyFail, "go": goFail} {
		if !strings.Contains(r.stderr, line) {
			t.Errorf("%s stderr lacks %q: %s", label, line, r.stderr)
		}
	}
	expectEqual(t, "replay stdout", goFail.stdout, pyFail.stdout)
}

// The Action part needs at least 80 non-space characters: 79 fails and 80 passes, in both.
func Test47_OperationsActionMinimum(t *testing.T) {
	for _, n := range []int{79, 80} {
		root := opsCopy(t)
		editOps(t, root, "operations/scenarios.md", func(s string) string {
			start := strings.Index(s, "## S1 ")
			action := start + strings.Index(s[start:], "Action:")
			end := action + strings.Index(s[action:], "Preserved:")
			return s[:action] + "Action: " + strings.Repeat("a", n) + "\n\n" + s[end:]
		})
		got := opsParity(t, fmt.Sprintf("action %d", n), root)
		failed := strings.Contains(got.stderr, "scenario S1 states its Action part in fewer than 80 characters")
		if failed != (n < 80) || (got.code == 0) != (n >= 80) {
			t.Errorf("action of %d characters: %+v", n, got)
		}
	}
}
