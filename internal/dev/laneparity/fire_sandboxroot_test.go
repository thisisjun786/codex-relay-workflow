//go:build dev

package laneparity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contracttest"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// CRW-1265: crw-dev parity fire and latency replay the CXC hook fixtures in a process that need not have
// isolated (cmd/crw-dev runs no testsupport.Main), so every case of a firing reads a crw configuration that
// names the firing's scratch directory as paths.sandbox_root, whatever configuration the firing process has.
// Before, a case read the test process's isolated configuration, or none at all under crw-dev, and the
// managed-dispatch fixtures were refused as outside_dedicated_root when --scratch lay outside the default
// sandbox base.
func TestFireHooks_everyCaseNamesTheScratchAsItsSandboxRoot(t *testing.T) {
	// Given: a generated plugin root and a scratch directory of the test's own
	o := fireFixture(t)
	want, err := ExpectedLegs(o.Root)
	if err != nil {
		t.Fatal(err)
	}
	_, registered, err := ReadRegistered(o.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	declared := Declared(registered, want)
	scratch, err := filepath.EvalSymlinks(o.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	plan := newSeedPlan(SwitchOn, o.CRW, declared)
	in := contracttest.HookFireInput{
		Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Declared: declared, Scratch: o.Scratch,
		Only: regexp.MustCompile(`^hook__pre-tool-use-attaching-skills__(managed_fallback_candidate_null_effort|managed_dispatch_marker_issues_once)$`),
		Seed: func(c *cxccorpus.Case, env []string) (func() error, error) {
			seen = append(seen, sandboxRootOf(t, env))
			return plan.seed(c, env)
		},
	}

	// When: the managed-dispatch fixtures and a probe of CRW's own are fired
	results, err := contracttest.FireHooks(in)
	if err != nil {
		t.Fatal(err)
	}
	probe := OwnProbes()[0]
	probes, err := contracttest.FireProbes(in, []contracttest.Probe{{ID: probe.ID, Scenario: probe.Scenario, Check: probe.Check}})
	if err != nil {
		t.Fatal(err)
	}

	// Then: every process of every case read the scratch directory as its sandbox root, the fixtures equal
	// their expectation, and the firing left nothing in the scratch directory
	if len(results) != 2 {
		t.Fatalf("%d fixtures fired, want 2", len(results))
	}
	for _, res := range results {
		if !res.Run || res.Err != nil {
			t.Errorf("%s: run %v: %v", res.ID, res.Run, res.Err)
		}
	}
	if len(probes) != 1 || probes[0].Err != nil {
		t.Errorf("probe %s: %+v", probe.ID, probes)
	}
	if len(seen) < 3 {
		t.Fatalf("%d processes seeded, want one per step of each case", len(seen))
	}
	for i, root := range seen {
		if root != scratch {
			t.Errorf("process %d of the firing read sandbox_root %s, want the scratch directory %s", i, root, scratch)
		}
	}
	if left, err := os.ReadDir(o.Scratch); err != nil || len(left) != 0 {
		t.Errorf("the scratch directory holds %v after the firing (%v)", left, err)
	}
}

// sandboxRootOf is the paths.sandbox_root of the crw configuration env names as CRW_CONFIG.
func sandboxRootOf(t *testing.T, env []string) string {
	t.Helper()
	path := ""
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "CRW_CONFIG="); ok {
			path = value
		}
	}
	if path == "" {
		return "(no CRW_CONFIG)"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "(unreadable CRW_CONFIG: " + err.Error() + ")"
	}
	var doc struct {
		Paths struct {
			SandboxRoot string `json:"sandbox_root"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "(CRW_CONFIG is not JSON: " + err.Error() + ")"
	}
	return doc.Paths.SandboxRoot
}
