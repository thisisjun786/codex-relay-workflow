//go:build dev

package ci

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"unicode"
)

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// contractCheck pairs an offline checker with the contract it replays. builtin, when set, is
// the Go port of the checker, so the pair needs only its contract; otherwise the checker is
// still a Python script owned by another component and is run with python3.
type contractCheck struct {
	name, script, contract string
	args                   []string
	builtin                func(root string, stdout, stderr io.Writer) int
}

// contractChecks is scripts/ci/contracts.py's CHECKS, in the same order.
var contractChecks = []contractCheck{
	{name: "hook", script: "plugins/crw/skills/crw-run/scripts/hook_probe.py",
		contract: "plugins/crw/skills/crw-run/references/hook-contract.md", args: []string{"replay"}},
	{name: "operations", script: "scripts/check_operations_contract.py",
		contract: "plugins/crw/skills/crw-run/references/operations.md", builtin: operationsCheck},
	// Re-derives the committed component identity from this checkout, so the one compatibility
	// definition cannot drift away from the source it describes.
	{name: "runtime", script: "scripts/runtime_install.py",
		contract: "scripts/crw_runtime/components.json", args: []string{"verify-definition"}},
	// The two closed-vocabulary start-policy fields are checked against the table that declares them.
	{name: "start policy", script: "plugins/crw/skills/crw-run/scripts/start_policy.py",
		contract: "plugins/crw/skills/crw-run/references/start-policy.md", args: []string{"selftest"}},
	// The parent title rule lives in the binding procedure, so the checker is paired with it.
	{name: "parent title", script: "plugins/crw/skills/crw-run/scripts/parent_title.py",
		contract: "plugins/crw/skills/crw-plan/references/integrations.md", args: []string{"replay"}},
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Contracts is `crw-dev ci contracts`: run each known offline contract check whose owning
// component is present, and refuse a half-present script/contract pair.
func Contracts(args []string, stdout, stderr io.Writer) int {
	if _, code := parseFlags("contracts", "Run known offline contract checks when their owning component is present.",
		nil, args, stdout, stderr); code >= 0 {
		return code
	}
	root, err := repositoryRoot()
	if err != nil {
		return failf(stderr, "contracts: %s", err)
	}
	for _, check := range contractChecks {
		// The same pair rule as scripts/ci/contracts.py, builtins included: while the Python
		// checker exists it marks its component as present, and the Go port runs in its place.
		// The step that deletes check_operations_contract.py has to decide this rule anew.
		contractPresent := isFile(filepath.Join(root, check.contract))
		scriptPresent := isFile(filepath.Join(root, check.script))
		if contractPresent != scriptPresent {
			return failf(stderr, "Incomplete %s contract/check pair", check.name)
		}
		if !contractPresent {
			fmt.Fprintf(stdout, "%s: component absent; no coverage claimed\n", check.name)
			continue
		}
		if check.builtin != nil {
			if code := check.builtin(root, stdout, stderr); code != 0 {
				return failf(stderr, "%s contract check failed with exit status %d", check.name, code)
			}
			continue
		}
		cmd := exec.Command("python3", append([]string{filepath.Join(root, check.script)}, check.args...)...)
		cmd.Dir = root
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return failf(stderr, "%s contract check failed: %s", check.name, err)
		}
	}
	return 0
}
