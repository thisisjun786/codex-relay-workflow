//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// contractCheck pairs a contract with the Go check that replays it. Every check is built into
// crw-dev, so a component is present exactly when its contract is: no checker script has to sit
// beside it (scripts/ci/contracts.py, the Python twin deleted in todo 44, paired each contract
// with its script).
type contractCheck struct {
	name, contract string
	check          func(root string, stdout, stderr io.Writer) int
}

const (
	skillRun         = "plugins/crw/skills/crw-run"
	runtimeContract  = "scripts/crw_runtime/components.json"
	bridgeToolSchema = "contract/schema/bridge-mcp-tools.json"
)

// contractChecks is what scripts/ci/contracts.py's CHECKS was until todo 44, in the same order.
var contractChecks = []contractCheck{
	{name: "hook", contract: skillRun + "/references/hook-contract.md", check: hookContractCheck},
	{name: "operations", contract: skillRun + "/references/operations.md", check: operationsCheck},
	// What the Go build takes from the one compatibility definition, checked against the checkout.
	{name: "runtime", contract: runtimeContract, check: runtimeCheck},
	// The two closed-vocabulary start-policy fields are checked against the table that declares them.
	{name: "start policy", contract: skillRun + "/references/start-policy.md", check: startPolicyCheck},
	// The parent title rule lives in the binding procedure, so the checker is paired with it.
	{name: "parent title", contract: "plugins/crw/skills/crw-plan/references/integrations.md", check: parentTitleCheck},
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Contracts is `crw-dev ci contracts`: run each known offline contract check whose contract is
// present in the checkout.
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
		if !isFile(filepath.Join(root, check.contract)) {
			fmt.Fprintf(stdout, "%s: component absent; no coverage claimed\n", check.name)
			continue
		}
		if code := check.check(root, stdout, stderr); code != 0 {
			return failf(stderr, "%s contract check failed with exit status %d", check.name, code)
		}
	}
	return 0
}

// hookContractCheck is `crw skill hook-probe replay` over the checkout's contract, decision
// fixtures and host observations rather than the copies built into crw.
func hookContractCheck(root string, stdout, stderr io.Writer) int {
	dir := filepath.Join(root, skillRun)
	return skill.Run([]string{"hook-probe", "replay",
		"--fixtures", filepath.Join(dir, "scripts", "fixtures", "decisions"),
		"--contract", filepath.Join(dir, "references", "hook-contract.md"),
		"--host-fixtures", filepath.Join(dir, "scripts", "fixtures", "host")}, strings.NewReader(""), stdout, stderr)
}

// startPolicyCheck is `crw skill start-policy selftest` over the checkout's start-policy.md.
func startPolicyCheck(root string, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(filepath.Join(root, skillRun, "references", "start-policy.md"))
	if err != nil {
		return failf(stderr, "start policy: %s", err)
	}
	return skill.StartPolicySelftest(raw, stdout, stderr)
}

// parentTitleCheck is `crw skill parent-title replay` over the checkout's title fixtures.
func parentTitleCheck(root string, stdout, stderr io.Writer) int {
	return skill.Run([]string{"parent-title", "replay",
		"--fixtures", filepath.Join(root, skillRun, "scripts", "fixtures", "titles")}, strings.NewReader(""), stdout, stderr)
}

// runtimeCheck judges scripts/crw_runtime/components.json by what the Go build relies on (see
// internal/runtime/definition): the fields the Go definition retains agree with it, each
// component's licence file is in the checkout, each identity tool is a tool the bridge's
// contract lists, and the console scripts, with the completion hook's entry point, are the links
// the installer places beside crw. The Python-era fields (trees, source digests, module
// locations) describe the Python packages and are not judged here.
func runtimeCheck(root string, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(filepath.Join(root, runtimeContract))
	if err != nil {
		return failf(stderr, "runtime: %s", err)
	}
	var committed struct {
		DefinitionVersion *int `json:"definitionVersion"`
		Components        []struct {
			Component, ConsoleScript, Version, LicencePath, IdentityTool, ExerciseCommand string
			Upstream                                                                      struct{ Remote, Revision, Licence string }
		}
	}
	if err := json.Unmarshal(raw, &committed); err != nil {
		return failf(stderr, "runtime: %s is not a component definition: %s", runtimeContract, err)
	}
	var findings []string
	finding := func(format string, args ...any) { findings = append(findings, fmt.Sprintf(format, args...)) }
	if committed.DefinitionVersion == nil || *committed.DefinitionVersion != definition.Version {
		finding("definitionVersion is not %d", definition.Version)
	}
	if len(committed.Components) != len(definition.Components) {
		finding("%d components, the Go definition has %d", len(committed.Components), len(definition.Components))
	}
	tools, err := bridgeTools(root)
	if err != nil {
		finding("%s: %s", bridgeToolSchema, err)
	}
	var scripts []string
	for i, c := range committed.Components {
		scripts = append(scripts, c.ConsoleScript)
		if i < len(definition.Components) {
			g := definition.Components[i]
			for _, field := range [][3]string{
				{"component", c.Component, g.Name}, {"consoleScript", c.ConsoleScript, g.ConsoleScript},
				{"version", c.Version, g.Version}, {"licencePath", c.LicencePath, g.LicencePath},
				{"identityTool", c.IdentityTool, g.IdentityTool}, {"exerciseCommand", c.ExerciseCommand, g.ExerciseCommand},
				{"upstream.remote", c.Upstream.Remote, g.Upstream.Remote}, {"upstream.revision", c.Upstream.Revision, g.Upstream.Revision},
				{"upstream.licence", c.Upstream.Licence, g.Upstream.Licence},
			} {
				if field[1] != field[2] {
					finding("component %d %s is %q, the Go definition's is %q", i, field[0], field[1], field[2])
				}
			}
		}
		if c.LicencePath == "" || !isFile(filepath.Join(root, c.LicencePath)) {
			finding("%s: licence %q is not a file in this checkout", c.Component, c.LicencePath)
		}
		if c.IdentityTool != "" && tools != nil && !slices.Contains(tools, c.IdentityTool) {
			finding("%s: identity tool %q is not a tool %s lists", c.Component, c.IdentityTool, bridgeToolSchema)
		}
	}
	links := append(scripts, definition.HookScript)
	want := definition.Links()
	if !sameNames(links, want) {
		finding("the console scripts and %s are %v, the links the installer places are %v", definition.HookScript, links, want)
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(stderr, "FAIL runtime: "+f)
		}
		fmt.Fprintf(stderr, "%d problem(s) in %s.\n", len(findings), runtimeContract)
		return 1
	}
	fmt.Fprintf(stdout, "runtime: %s agrees with the Go definition: %d components, licences present, identity tools listed in %s, links %s\n",
		runtimeContract, len(committed.Components), bridgeToolSchema, strings.Join(want, ", "))
	return 0
}

// bridgeTools is the tool names contract/schema/bridge-mcp-tools.json lists.
func bridgeTools(root string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, bridgeToolSchema))
	if err != nil {
		return nil, err
	}
	var schema struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	names := []string{}
	for _, tool := range schema.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// sameNames reports whether a and b hold the same names, each once.
func sameNames(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y) && len(slices.Compact(x)) == len(a)
}
