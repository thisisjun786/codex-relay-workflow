//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

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
	runtimeContract  = "internal/runtime/definition/definition.go"
	bridgeToolSchema = "contract/schema/bridge-mcp-tools.json"
	releaseConfig    = ".goreleaser.yaml"
)

// contractChecks is what scripts/ci/contracts.py's CHECKS was until todo 44, in the same order.
var contractChecks = []contractCheck{
	{name: "hook", contract: skillRun + "/references/hook-contract.md", check: hookContractCheck},
	{name: "operations", contract: skillRun + "/references/operations.md", check: operationsCheck},
	// The one compatibility definition (internal/runtime/definition), checked against the
	// checkout and the release build (decision 47).
	{name: "runtime", contract: runtimeContract, check: runtimeCheck},
	// The two closed-vocabulary start-policy fields are checked against the table that declares them.
	{name: "start policy", contract: skillRun + "/references/start-policy.md", check: startPolicyCheck},
	// The parent title rule lives in the binding procedure, so the checker is paired with it.
	{name: "parent title", contract: "plugins/crw/skills/crw-plan/references/integrations.md", check: parentTitleCheck},
	// The CXC v0.2.40 behaviour corpus (CRW-279): its specs, fixtures, rules, rename table and
	// coverage index agree. Recording needs the Node oracle and stays a manual `crw-dev cxc
	// record`; this check needs none.
	{name: "cxc corpus", contract: cxccorpus.Coverage, check: cxccorpus.LintReport},
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Contracts is `crw-dev ci contracts`: run each known offline contract check whose contract is
// present in the checkout.
func Contracts(args []string, stdout, stderr io.Writer) int {
	if code := parseFlags(newFlags("contracts"), "Run known offline contract checks when their owning component is present.",
		args, stdout, stderr); code >= 0 {
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

// runtimeCheck judges the one compatibility definition, internal/runtime/definition (decision
// 47), against what it names outside itself: each component's licence file is in the checkout
// and among the files the release archives carry, the bridge's identity tool is a tool the
// bridge's contract lists, and the links the installer places beside crw are the ones the
// release build makes. Until todo 44 it compared scripts/crw_runtime/components.json with the
// Go definition field by field; that file left with its last Python reader.
func runtimeCheck(root string, stdout, stderr io.Writer) int {
	var findings []string
	finding := func(format string, args ...any) { findings = append(findings, fmt.Sprintf(format, args...)) }
	tools, err := bridgeTools(root)
	if err != nil {
		finding("%s: %s", bridgeToolSchema, err)
	}
	release, err := os.ReadFile(filepath.Join(root, releaseConfig))
	if err != nil {
		finding("%s: %s", releaseConfig, err)
	}
	archived := releaseArchiveFiles(release)
	for _, c := range definition.Components {
		switch {
		case c.LicencePath == "" || !isFile(filepath.Join(root, c.LicencePath)):
			finding("%s: licence %q is not a file in this checkout", c.Name, c.LicencePath)
		case release != nil && !slices.Contains(archived, c.LicencePath):
			finding("%s: licence %q is not a file %s archives", c.Name, c.LicencePath, releaseConfig)
		}
		if c.IdentityTool != "" && tools != nil && !slices.Contains(tools, c.IdentityTool) {
			finding("%s: identity tool %q is not a tool %s lists", c.Name, c.IdentityTool, bridgeToolSchema)
		}
	}
	want := definition.Links()
	if release != nil {
		if made := releaseLinks(release); !sameNames(made, want) {
			finding("the release build (%s) links %v beside crw, the installer places %v", releaseConfig, made, want)
		}
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(stderr, "FAIL runtime: "+f)
		}
		fmt.Fprintf(stderr, "%d problem(s) in the component definition (%s).\n", len(findings), runtimeContract)
		return 1
	}
	fmt.Fprintf(stdout, "runtime: the component definition (%s) agrees with the checkout: %d components, licences present and archived, identity tools listed in %s, links %s as %s makes them\n",
		runtimeContract, len(definition.Components), bridgeToolSchema, strings.Join(want, ", "), releaseConfig)
	return 0
}

var (
	releaseLink = regexp.MustCompile(`(?m)^\s*- cmd: ln -sfn crw (\S+)$`)
	releaseFile = regexp.MustCompile(`(?m)^\s*- ([^\s:{}]+)$`)
)

// releaseLinks is the compatibility names the release build's post hooks link to crw
// (`ln -sfn crw <name>`), which the archives carry beside it.
func releaseLinks(config []byte) []string {
	names := []string{}
	for _, m := range releaseLink.FindAllSubmatch(config, -1) {
		names = append(names, string(m[1]))
	}
	return names
}

// releaseArchiveFiles is every plain list entry of the release configuration, the archives'
// extra files among them (LICENSE and the bridge's licence); it is not a YAML reader, and only
// membership is asked of it.
func releaseArchiveFiles(config []byte) []string {
	var files []string
	for _, m := range releaseFile.FindAllSubmatch(config, -1) {
		files = append(files, string(m[1]))
	}
	return files
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
