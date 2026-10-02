//go:build dev

package ci

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

const opsReferences = "plugins/crw/skills/crw-run/references"

// opsFiles are the operations contract and the fixtures its checker reads.
var opsFiles = []string{"operations.md", "operations/check-result.example.json",
	"operations/compatibility-record.example.json", "operations/installation-plan.example.md",
	"operations/scenarios.md"}

// opsCopy copies this checkout's operations contract and the fixtures its checker reads into a
// scratch root. An edit of them changes what the golden rows answer: rewrite the goldens with
// CRW_GOLDEN=update and review their diff.
func opsCopy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range opsFiles {
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

// checkResult holds a command's result to the golden kept under key: root, the scratch directory
// the command ran in, is stored as $ROOT and the checkout as $REPO.
func checkResult(t *testing.T, key, root string, r result) {
	t.Helper()
	golden.CheckJSON(t, key, map[string]any{"code": r.code, "stdout": r.stdout, "stderr": r.stderr},
		golden.Substitute(root, "$ROOT"), golden.Substitute(repoRoot(), "$REPO"))
}

// failLines is stderr reduced to the checker's own FAIL lines.
func failLines(stderr string) string {
	var fails []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "FAIL ") {
			fails = append(fails, line)
		}
	}
	return strings.Join(fails, "\n")
}

// opsGolden requires `crw-dev ci operations` to answer the golden kept under label for root.
func opsGolden(t *testing.T, label, root string) result {
	t.Helper()
	got := goCheck(t, root, nil, "operations", "--root", root)
	checkResult(t, label, root, got)
	return got
}

// The operations checker has no property of its own in the todo-47 list: it runs as part of
// `crw-dev ci contracts` (GATE-8 places that check in validate). These tests pin its answer on
// the repository and on failing fixtures.
func Test47_OperationsContractGolden(t *testing.T) {
	if got := opsGolden(t, "repository", opsCopy(t)); got.code != 0 {
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
		// OPS-1.1: an install with no installMode is a Go install, and names its binary's digest
		// and target; requires-python is asked only of a component with a Python-era install.
		{"go install digest", "operations/compatibility-record.example.json", func(s string) string {
			return strings.Replace(s, `"installMode": "editable",`, `"binaryDigest": "short", "target": "linux/amd64",`, 1)
		}, "has a Go install without a 64 character binaryDigest"},
		{"go install target", "operations/compatibility-record.example.json", func(s string) string {
			return strings.Replace(s, `"installMode": "editable",`, `"binaryDigest": "`+strings.Repeat("a", 64)+`",`, 1)
		}, "has a Go install that does not name its target"},
		{"python-era requires-python", "operations/compatibility-record.example.json", func(s string) string {
			return strings.Replace(s, `"requiresPython": ">=3.11",`, `"requiresPython": "",`, 1)
		}, "is missing a non-empty requiresPython, which a Python-era install needs"},
		// OPS-1.3: a Go point names its install and installDigest; a Python-era one its interpreter.
		{"go point keys", "operations/compatibility-record.example.json", func(s string) string {
			return strings.Replace(s, `"measuredPoints": []`, `"measuredPoints": [{"install": "x", "codexCli": "c"}]`, 1)
		}, "has a measured point missing installDigest"},
		{"python-era point keys", "operations/compatibility-record.example.json", func(s string) string {
			return strings.Replace(s, `"measuredPoints": []`, `"measuredPoints": [{"interpreter": "3.13.0"}]`, 1)
		}, "has a measured point missing codexCli"},
		{"table field", "operations.md", func(s string) string {
			i := strings.Index(s, "### OPS-6.1")
			j := i + strings.Index(s[i:], "\n| `")
			return s[:j] + "\n| `extraField` | x |" + s[j:]
		}, "do not match OPS-6.1"},
	} {
		root := opsCopy(t)
		editOps(t, root, row.rel, row.edit)
		got := opsGolden(t, row.label, root)
		if got.code != 1 || (row.fragment != "" && !strings.Contains(got.stderr, row.fragment)) {
			t.Errorf("%s: %+v", row.label, got)
		}
	}
	root := t.TempDir()
	got := opsGolden(t, "missing", root)
	if got.code != 1 || !strings.HasPrefix(got.stderr, "MISSING ") {
		t.Errorf("missing: %+v", got)
	}
}

// goShapedRecord is a compatibility record whose only installs are Go installs (OPS-1.1): no
// requiresPython and no installMode, and its point names the install instead of an interpreter
// (OPS-1.3). point replaces that measured point's JSON when it is not empty.
func goShapedRecord(point string) string {
	runtimeDir := "/example/home/.local/share/crw-runtime/bin-0.5.0-aaaaaaaaaaaa"
	digest := strings.Repeat("c", 64)
	if point == "" {
		point = `{"install": "` + runtimeDir + `/bin", "installDigest": "` + digest + `",
        "codexCli": "codex-cli 0.154.0", "appServer": "{}", "host": "example-host", "date": "2026-01-01",
        "measuredBy": "<issue-id>", "method": "codex-session-relay doctor; an MCP session calling get_capabilities"}`
	}
	return `{
  "components": [
    {
      "component": "codex-session-relay",
      "source": {"checkout": "/example/checkouts/codex-relay-workflow", "remote": "none"},
      "revision": "` + strings.Repeat("a", 40) + `",
      "tree": "` + strings.Repeat("b", 40) + `",
      "workingTreeClean": true,
      "version": "0.5.0",
      "installs": [{"location": "` + runtimeDir + `/bin", "environment": "` + runtimeDir + `",
        "entryPoint": "` + runtimeDir + `/bin/codex-session-relay", "integrity": "` + digest + `",
        "binaryDigest": "` + digest + `", "target": "linux/amd64"}],
      "measuredPoints": [` + point + `]
    }
  ],
  "unmeasured": []
}
`
}

// A Go-shaped record is accepted.
func TestOperationsGoShapedCompatibilityRecord(t *testing.T) {
	root := opsCopy(t)
	editOps(t, root, "operations/compatibility-record.example.json", func(string) string { return goShapedRecord("") })
	if got := opsGolden(t, "go-shaped record", root); got.code != 0 {
		t.Fatalf("a Go-shaped compatibility record must pass: %+v", got)
	}
}

// OPS-1.3: a point takes the kind of the install it covers. Its own shape cannot choose the
// lighter schema, and a Go point has to name one of the component's Go installs and the digest
// of that binary; the rest is refused.
func TestOperationsMeasuredPointCoversAnInstallOfItsKind(t *testing.T) {
	bin := "/example/home/.local/share/crw-runtime/bin-0.5.0-aaaaaaaaaaaa/bin"
	rest := `"codexCli": "codex-cli 0.154.0", "appServer": "{}", "host": "example-host", "date": "2026-01-01", "measuredBy": "<issue-id>", "method": "doctor"`
	for _, row := range []struct {
		label, point, fragment string
	}{
		{"python-era point on a go install", `{"interpreter": "3.13.0", ` + rest + `}`,
			"has a Python-era measured point but no Python-era install for it to cover"},
		{"go point naming no install", `{"install": "/elsewhere/bin", "installDigest": "` + strings.Repeat("c", 64) + `", ` + rest + `}`,
			"has a measured point that names no Go install of this component"},
		{"go point naming no install at all", `{"install": 7, "installDigest": "` + strings.Repeat("c", 64) + `", ` + rest + `}`,
			"has a measured point that names no Go install of this component"},
		{"go point with other bytes", `{"install": "` + bin + `", "installDigest": "` + strings.Repeat("d", 64) + `", ` + rest + `}`,
			"has a measured point whose installDigest is not its install's binaryDigest"},
		{"go point with no digest", `{"install": "` + bin + `", ` + rest + `}`,
			"has a measured point whose installDigest is not its install's binaryDigest"},
	} {
		root := opsCopy(t)
		editOps(t, root, "operations/compatibility-record.example.json", func(string) string { return goShapedRecord(row.point) })
		got := opsGolden(t, row.label, root)
		if got.code != 1 || !strings.Contains(got.stderr, row.fragment) {
			t.Errorf("%s: %+v", row.label, got)
		}
	}
	// A Go-shaped point on a component whose only installs are Python-era ones covers nothing.
	root := opsCopy(t)
	editOps(t, root, "operations/compatibility-record.example.json", func(s string) string {
		location := "/example/checkouts/codex-session-relay/.venv/lib/python3.13/site-packages/codex_session_relay"
		return strings.Replace(s, `"measuredPoints": []`, `"measuredPoints": [{"install": "`+location+`", "installDigest": "`+strings.Repeat("9", 64)+`", `+rest+`}]`, 1)
	})
	if got := opsGolden(t, "go point on a python-era install", root); got.code != 1 || !strings.Contains(got.stderr, "has a measured point that names no Go install of this component") {
		t.Errorf("go point on a python-era install: %+v", got)
	}
}

// contractsRepo is a Git checkout holding the named files (empty).
func contractsRepo(t *testing.T, present ...string) *fixtureRepo {
	t.Helper()
	r := newRepo(t)
	r.write("README.md", "")
	for _, path := range present {
		r.write(path, "")
	}
	r.commit()
	return r
}

// contractInputs are the files the six checks read, as this checkout has them.
var contractInputs = []string{
	"plugins/crw/skills/crw-run/references/hook-contract.md",
	"plugins/crw/skills/crw-run/scripts/fixtures",
	"plugins/crw/skills/crw-run/references/operations.md",
	"plugins/crw/skills/crw-run/references/operations",
	"internal/runtime/definition/definition.go",
	".goreleaser.yaml",
	"contract/schema/bridge-mcp-tools.json",
	"LICENSE",
	"packages/codex-thread-bridge/LICENSE",
	"plugins/crw/skills/crw-run/references/start-policy.md",
	"plugins/crw/skills/crw-plan/references/integrations.md",
	"contract/schema/cxc",
	"contract/fixtures/cxc",
}

// copyTree copies the file or directory rel from this checkout into root.
func copyTree(t *testing.T, root, rel string) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(repoRoot(), rel), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, strings.TrimPrefix(path, repoRoot()))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every check is built into crw-dev: a component is present exactly when its contract is, and no
// checker script has to sit beside it. scripts/ci/contracts.py, the Python twin (deleted in todo
// 44), refused a contract whose checker script was missing (and the reverse); Go has no script to
// pair.
func Test47_ContractsPairsAndAbsentComponents(t *testing.T) {
	// No component present: every check reports it claims no coverage, as the golden holds.
	r := contractsRepo(t)
	got := goCheck(t, r.root, nil, "contracts")
	checkResult(t, "all absent", r.root, got)
	if got.code != 0 || strings.Count(got.stdout, "component absent; no coverage claimed") != len(contractChecks) {
		t.Errorf("all absent: %+v", got)
	}
	// A Python checker script without its contract is not a component: nothing runs it.
	r = contractsRepo(t, "plugins/crw/skills/crw-run/scripts/hook_probe.py", "scripts/check_operations_contract.py",
		"scripts/runtime_install.py", "plugins/crw/skills/crw-run/scripts/start_policy.py",
		"plugins/crw/skills/crw-run/scripts/parent_title.py")
	if got := goCheck(t, r.root, nil, "contracts"); got.code != 0 || strings.Count(got.stdout, "component absent; no coverage claimed") != len(contractChecks) {
		t.Errorf("scripts without contracts: %+v", got)
	}
	// A present contract is checked with the data it needs, and fails without it rather than
	// claim coverage.
	for _, contract := range []string{
		"plugins/crw/skills/crw-run/references/hook-contract.md",
		"plugins/crw/skills/crw-run/references/start-policy.md",
		"internal/runtime/definition/definition.go",
	} {
		r := contractsRepo(t, contract)
		if got := goCheck(t, r.root, nil, "contracts"); got.code != 1 || !strings.Contains(got.stderr, " contract check failed with exit status ") {
			t.Errorf("%s alone: %+v", contract, got)
		}
	}
	// The checkout's contracts and their data, without one Python script: every check runs and
	// passes, which is what the checkout is once the Python implementation leaves (todo 44).
	root := newRepo(t).root
	for _, rel := range contractInputs {
		copyTree(t, root, rel)
	}
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".py") {
			return errors.New("a Python file was copied: " + path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	checked := goCheck(t, root, nil, "contracts")
	if checked.code != 0 || strings.Contains(checked.stdout, "component absent") {
		t.Fatalf("contracts without Python checkers: %+v", checked)
	}
	// This repository: every check runs and passes.
	here := goCheck(t, repoRoot(), nil, "contracts")
	if here.code != 0 || strings.Contains(here.stdout, "component absent") {
		t.Fatalf("repository: %+v", here)
	}
	for _, line := range []string{"fixtures matched", "sites reached", "OK every clause cited by any fixture exists",
		"runtime: the component definition (internal/runtime/definition/definition.go) agrees with the checkout", "vocabulary: 3 run modes and 3 observation paths, as declared",
		"title fixtures against their recorded expectations", "cxc corpus: "} {
		if !strings.Contains(here.stdout, line) || !strings.Contains(checked.stdout, line) {
			t.Errorf("a check did not report %q\nrepository: %s\nwithout Python: %s", line, here.stdout, checked.stdout)
		}
	}
}

// The runtime check judges the one component definition against what it names outside itself,
// one property per row: the licences in the checkout and the release archives, the identity tool
// in the bridge's contract, and the links the release build makes.
func TestContractsRuntimeDefinition(t *testing.T) {
	base := func(t *testing.T) string {
		root := newRepo(t).root
		for _, rel := range []string{"internal/runtime/definition/definition.go", ".goreleaser.yaml", "contract/schema/bridge-mcp-tools.json", "LICENSE", "packages/codex-thread-bridge/LICENSE"} {
			copyTree(t, root, rel)
		}
		return root
	}
	if got := goCheck(t, base(t), nil, "contracts"); got.code != 0 || !strings.Contains(got.stdout, "links codex-session-relay, codex-thread-bridge as .goreleaser.yaml makes them") {
		t.Fatalf("clean: %+v", got)
	}
	edit := func(t *testing.T, root, rel, old, new string) {
		t.Helper()
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(string(data), old, new, 1)
		if changed == string(data) {
			t.Fatalf("%s: %q not found", rel, old)
		}
		if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		label, rel, old, new, fragment string
	}{
		{"identity tool", "contract/schema/bridge-mcp-tools.json", `"name": "get_capabilities"`, `"name": "get_abilities"`, `identity tool "get_capabilities" is not a tool contract/schema/bridge-mcp-tools.json lists`},
		{"link", ".goreleaser.yaml", "ln -sfn crw codex-thread-bridge", "ln -sfn crw crw-bridge", "links [codex-session-relay crw-bridge] beside crw, the installer places"},
		{"archived licence", ".goreleaser.yaml", "      - packages/codex-thread-bridge/LICENSE", "      - packages/LICENSE", `licence "packages/codex-thread-bridge/LICENSE" is not a file .goreleaser.yaml archives`},
	} {
		root := base(t)
		edit(t, root, row.rel, row.old, row.new)
		got := goCheck(t, root, nil, "contracts")
		if got.code != 1 || !strings.Contains(got.stderr, "FAIL runtime: ") || !strings.Contains(got.stderr, row.fragment) ||
			!strings.Contains(got.stderr, "runtime contract check failed with exit status 1") {
			t.Errorf("%s: %+v", row.label, got)
		}
	}
	for _, row := range []struct{ label, rel, fragment string }{
		{"licence", "packages/codex-thread-bridge/LICENSE", `FAIL runtime: codex-thread-bridge: licence "packages/codex-thread-bridge/LICENSE" is not a file in this checkout`},
		{"tool schema", "contract/schema/bridge-mcp-tools.json", "FAIL runtime: contract/schema/bridge-mcp-tools.json: "},
		{"release config", ".goreleaser.yaml", "FAIL runtime: .goreleaser.yaml: "},
	} {
		root := base(t)
		if err := os.Remove(filepath.Join(root, row.rel)); err != nil {
			t.Fatal(err)
		}
		if got := goCheck(t, root, nil, "contracts"); got.code != 1 || !strings.Contains(got.stderr, row.fragment) {
			t.Errorf("%s: %+v", row.label, got)
		}
	}
}

func Test47_ContractsOperationsPairAndResult(t *testing.T) {
	// The operations checker is built in: its Python script without the contract is no component.
	r := contractsRepo(t, "scripts/check_operations_contract.py")
	got := goCheck(t, r.root, nil, "contracts")
	if got.code != 0 || !strings.Contains(got.stdout, "operations: component absent; no coverage claimed") {
		t.Errorf("operations script without contract: %+v", got)
	}

	// A failing operations replay fails contracts: the golden holds the exit, stdout and the
	// checker's own FAIL lines.
	root := opsCopy(t)
	editOps(t, root, "operations/scenarios.md", func(s string) string { return strings.Replace(s, "OPS-2.3", "OPS-99.1", 1) })
	fr := &fixtureRepo{t, root}
	fr.git("init", "-q")
	goFail := goCheck(t, root, nil, "contracts")
	expectEqual(t, "failing replay exit", goFail.code, 1)
	const line = "FAIL fixtures cite OPS-99.1, which the contract does not define"
	if !strings.Contains(goFail.stderr, line) {
		t.Errorf("stderr lacks %q: %s", line, goFail.stderr)
	}
	checkResult(t, "failing operations replay", root, result{goFail.code, goFail.stdout, failLines(goFail.stderr)})
}

// The Action part needs at least 80 non-space characters: 79 fails and 80 passes.
func Test47_OperationsActionMinimum(t *testing.T) {
	for _, n := range []int{79, 80} {
		root := opsCopy(t)
		editOps(t, root, "operations/scenarios.md", func(s string) string {
			start := strings.Index(s, "## S1 ")
			action := start + strings.Index(s[start:], "Action:")
			end := action + strings.Index(s[action:], "Preserved:")
			return s[:action] + "Action: " + strings.Repeat("a", n) + "\n\n" + s[end:]
		})
		got := opsGolden(t, fmt.Sprintf("action %d", n), root)
		failed := strings.Contains(got.stderr, "scenario S1 states its Action part in fewer than 80 characters")
		if failed != (n < 80) || (got.code == 0) != (n >= 80) {
			t.Errorf("action of %d characters: %+v", n, got)
		}
	}
}
