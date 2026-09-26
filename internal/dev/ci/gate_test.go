//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// gateCase is one gate input: the job results plus the selection they were produced under.
type gateCase struct {
	selection jsonObject
	needs     map[string]any // job -> result object (jsonObject) or a raw value
	env       map[string]string
	raw       *string // NEEDS_JSON verbatim instead of needs
}

// gateEnv mirrors test_gate.py's GateTests.env.
func gateEnv(kind, event, base string) *gateCase {
	paths := map[string][]string{"full": {"scripts/ci/gate.py"}, "docs": {"README.md"},
		"skill": {"plugins/crw/skills/crw-run/SKILL.md"}}[kind]
	tests, packages := kind != "docs", kind == "full"
	if event == "workflow_dispatch" {
		tests, packages = true, true
	}
	ref := "refs/heads/dev"
	if event == "pull_request" {
		ref = "refs/pull/1/merge"
	}
	reason := "paths"
	if event == "workflow_dispatch" {
		reason = "dispatch"
	}
	c := &gateCase{
		selection: jsonObject{{"version", 1}, {"event", event}, {"base", strings.Repeat("a", 40)},
			{"head", strings.Repeat("b", 40)}, {"base_ref", base}, {"ref", ref}, {"changed", paths},
			{"unknown", []string{}}, {"unsafe", []string{}}, {"reason", reason},
			{"selected", jsonObject{{"tests", tests}, {"packages", packages}}}},
		needs: map[string]any{},
		env:   map[string]string{"EVENT_NAME": event, "BASE_REF": base, "REF": ref, "HEAD_SHA": strings.Repeat("b", 40)},
	}
	for _, name := range GateJobs {
		result := "success"
		if (name == "tests" && !tests) || (name == "packages" && !packages) {
			result = "skipped"
		}
		c.needs[name] = jsonObject{{"result", result}}
	}
	return c
}

func (c *gateCase) setSelection(key string, value any) {
	for i := range c.selection {
		if c.selection[i].Key == key {
			c.selection[i].Value = value
		}
	}
}

// environment renders the case as the workflow's env (json.dumps default separators).
func (c *gateCase) environment() []string {
	var env []string
	if c.raw != nil {
		env = append(env, "NEEDS_JSON="+*c.raw)
	} else {
		names := make([]string, 0, len(c.needs))
		for name := range c.needs {
			names = append(names, name)
		}
		sort.Strings(names)
		var needs jsonObject
		for _, name := range names {
			job := c.needs[name]
			if name == "selection" {
				if obj, ok := job.(jsonObject); ok {
					job = append(slices.Clone(obj), jsonKV{"outputs", jsonObject{{"scope", pyJSON(c.selection, false)}}})
				}
			}
			needs = append(needs, jsonKV{name, job})
		}
		env = append(env, "NEEDS_JSON="+pyJSON(needs, false))
	}
	for key, value := range c.env {
		env = append(env, key+"="+value)
	}
	return env
}

// gateParity runs gate.py and `crw-dev ci gate` with the case's environment (and nothing else
// of the gate's inputs inherited) and requires identical results; it returns the Go result.
func gateParity(t *testing.T, label string, c *gateCase) result {
	t.Helper()
	env := append([]string{"NEEDS_JSON=", "EVENT_NAME=", "BASE_REF=", "REF=", "HEAD_SHA="}, c.environment()...)
	dir := t.TempDir()
	py := python(t, dir, env, "scripts/ci/gate.py")
	got := goCheck(t, dir, env, "gate")
	sameResult(t, label, py, got)
	return got
}

const gatePass = "Every selected check succeeded; unselected checks were explicitly skipped.\n"

func expectRefused(t *testing.T, label string, got result, message string) {
	t.Helper()
	if got.code != 1 || got.stdout != "" || !strings.HasPrefix(got.stderr, "Gate failed: ") ||
		(message != "" && got.stderr != "Gate failed: "+message+"\n") {
		t.Errorf("%s: want refusal %q, got %+v", label, message, got)
	}
}

func Test47_GATE_1_NeedsMustNameExactlyTheRequiredJobs(t *testing.T) {
	const expected = "Expected exactly go-product, packages, secrets, selection, tests, validate results"
	for _, raw := range []string{"", "{", "[]", "null", "true", "{}"} {
		c := gateEnv("full", "pull_request", "dev")
		c.raw = &raw
		got := gateParity(t, "raw "+raw, c)
		expectRefused(t, "raw "+raw, got, "")
	}
	for _, job := range GateJobs {
		c := gateEnv("full", "pull_request", "dev")
		delete(c.needs, job)
		expectRefused(t, "missing "+job, gateParity(t, "missing "+job, c), expected)
	}
	c := gateEnv("full", "pull_request", "dev")
	c.needs["extra"] = jsonObject{{"result", "success"}}
	expectRefused(t, "extra", gateParity(t, "extra", c), expected)
}

func Test47_GATE_2_SelectedSucceedAndUnselectedSkip(t *testing.T) {
	for _, kind := range []string{"docs", "skill", "full"} {
		for _, event := range []string{"pull_request", "push", "workflow_dispatch"} {
			got := gateParity(t, kind+"/"+event, gateEnv(kind, event, "dev"))
			expectEqual(t, kind+"/"+event, got, result{0, gatePass, ""})
		}
	}
	expectEqual(t, "parent", gateParity(t, "parent", gateEnv("full", "pull_request", "codex/parent")), result{0, gatePass, ""})
	for _, job := range GateJobs {
		for _, state := range []any{"failure", "cancelled", "skipped", "pending", "neutral", "", nil, true} {
			c := gateEnv("full", "pull_request", "dev")
			c.needs[job] = jsonObject{{"result", state}}
			label := job + "/" + pyJSON(state, false)
			expectRefused(t, label, gateParity(t, label, c), "")
		}
	}
	for _, job := range []string{"tests", "packages"} {
		for _, state := range []any{"success", "failure", "cancelled", "neutral", nil} {
			c := gateEnv("docs", "pull_request", "dev")
			c.needs[job] = jsonObject{{"result", state}}
			label := "docs " + job + "/" + pyJSON(state, false)
			expectRefused(t, label, gateParity(t, label, c), job+" must report skipped")
		}
	}
}

func Test47_GATE_3_SelectionIsRevalidated(t *testing.T) {
	expectRefused(t, "main", gateParity(t, "main", gateEnv("full", "pull_request", "main")),
		"PRs must target a development branch; main is a release mirror")
	for _, row := range []struct {
		field   string
		value   any
		message string
	}{
		{"selected", jsonObject{{"tests", false}, {"packages", false}}, "Selected jobs disagree with path evidence"},
		{"unknown", []string{"new/component.py"}, "Unregistered paths require an explicit verification mapping"},
		{"head", "old", "Invalid candidate SHA"},
		{"version", true, "Invalid selection schema"},
		{"selected", jsonObject{{"tests", "true"}, {"packages", true}}, "Selection outputs must be booleans"},
	} {
		c := gateEnv("full", "pull_request", "dev")
		c.setSelection(row.field, row.value)
		expectRefused(t, row.field, gateParity(t, row.field, c), row.message)
	}
}

func Test47_GATE_4_SelectionIsBoundToTheCandidate(t *testing.T) {
	for _, key := range []string{"EVENT_NAME", "BASE_REF", "REF", "HEAD_SHA"} {
		c := gateEnv("full", "pull_request", "dev")
		c.env[key] = "different"
		expectRefused(t, key, gateParity(t, key, c), "Selection does not match "+key)
	}
}

func Test47_GATE_5_CLIMissingInputFails(t *testing.T) {
	var env []string
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); !slices.Contains([]string{"NEEDS_JSON", "EVENT_NAME", "BASE_REF", "REF", "HEAD_SHA"}, key) {
			env = append(env, kv)
		}
	}
	dir := t.TempDir()
	for _, bin := range []func() result{
		func() result { return runEnv(t, dir, env, "python3", filepath.Join(repoRoot(), "scripts/ci/gate.py")) },
		func() result { return runEnv(t, dir, env, crwDev, "ci", "gate") },
	} {
		expectEqual(t, "missing input", bin(), result{1, "", "Gate failed: 'NEEDS_JSON'\n"})
	}
}

// workflowJobs is test_gate.py's workflow_jobs: job name -> body from ci.yml's two-space
// headers under jobs:, deliberately not a YAML parser.
func workflowJobs(t *testing.T) (map[string]string, []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	header := regexp.MustCompile(`^  ([a-z][a-z0-9-]*):$`)
	jobs := map[string][]string{}
	var order []string
	current, inside := "", false
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			inside, current = strings.HasPrefix(line, "jobs:"), ""
			continue
		}
		if !inside {
			continue
		}
		if m := header.FindStringSubmatch(line); m != nil {
			current = m[1]
			jobs[current] = nil
			order = append(order, current)
		} else if current != "" {
			jobs[current] = append(jobs[current], line)
		}
	}
	bodies := map[string]string{}
	for name, body := range jobs {
		bodies[name] = strings.Join(body, "\n")
	}
	return bodies, order
}

func sortedCopy(items []string) []string {
	out := slices.Clone(items)
	sort.Strings(out)
	return out
}

func Test47_GATE_6_RequiredSetIsTheWorkflowJobSet(t *testing.T) {
	jobs, order := workflowJobs(t)
	var real []string
	for _, name := range order {
		if name != "dev-gate" {
			real = append(real, name)
		}
	}
	expectEqual(t, "jobs", sortedCopy(real), sortedCopy(GateJobs))
	if !slices.Contains(GateJobs, "packages") || jobs["packages"] == "" {
		t.Error("packages must be a required, real job")
	}
	declared := regexp.MustCompile(`(?m)^    needs: \[([^\]]+)\]$`).FindStringSubmatch(jobs["dev-gate"])
	if declared == nil {
		t.Fatal("dev-gate must declare its prerequisites")
	}
	var needs []string
	for _, part := range strings.Split(declared[1], ",") {
		needs = append(needs, strings.TrimSpace(part))
	}
	expectEqual(t, "dev-gate needs", sortedCopy(needs), sortedCopy(GateJobs))
	// The Go list is the Python list: the gate's data did not change in the port.
	py := runEnv(t, repoRoot(), nil, "python3", "-c",
		"import sys; sys.path.insert(0, 'scripts/ci'); import gate; print(','.join(sorted(gate.JOBS)))")
	expectEqual(t, "gate.py JOBS", py.stdout, strings.Join(sortedCopy(GateJobs), ",")+"\n")
}

func Test47_GATE_7_NoJobOptsOutOfItsResult(t *testing.T) {
	jobs, _ := workflowJobs(t)
	for name, body := range jobs {
		if strings.Contains(body, "continue-on-error") {
			t.Errorf("%s carries continue-on-error", name)
		}
	}
}

func Test47_GATE_8_ExpensiveJobsFollowTheSelection(t *testing.T) {
	jobs, _ := workflowJobs(t)
	for job, want := range map[string]string{
		"packages": "if: needs.selection.outputs.packages == 'true'",
		"tests":    "if: needs.selection.outputs.tests == 'true'",
	} {
		if !strings.Contains(jobs[job], want) {
			t.Errorf("%s job lacks %q", job, want)
		}
	}
	// The contract check runs the dev binary once, in validate, and never in the test matrix.
	contracts := regexp.MustCompile(`(?m)^      - run: .*crw-dev"? ci contracts'?$`)
	if !contracts.MatchString(jobs["validate"]) {
		t.Error("validate job does not run crw-dev ci contracts")
	}
	if strings.Contains(jobs["tests"], "ci contracts") || strings.Contains(jobs["tests"], "contracts.py") {
		t.Error("the contract check must run once, in validate, not in tests")
	}
}

func Test47_GATE_9_DownloadedToolingIsPinned(t *testing.T) {
	jobs, _ := workflowJobs(t)
	action := regexp.MustCompile(`uses: (\S+)`)
	for name, body := range jobs {
		for _, m := range action.FindAllStringSubmatch(body, -1) {
			if !regexp.MustCompile(`@[0-9a-f]{40}$`).MatchString(m[1]) {
				t.Errorf("%s: %s is not pinned by commit", name, m[1])
			}
		}
		for _, line := range regexp.MustCompile(`(?m)^.*uses: \S+.*$`).FindAllString(body, -1) {
			if !regexp.MustCompile(`@[0-9a-f]{40} # \S`).MatchString(line) {
				t.Errorf("%s: %q lacks its version comment", name, strings.TrimSpace(line))
			}
		}
	}
	// Downloaded toolchains that take a checksum carry one and an exact version.
	body := jobs["packages"]
	for _, pattern := range []string{`uses: astral-sh/setup-uv@[0-9a-f]{40} #`, `checksum: '[0-9a-f]{64}'`, `version: '\d+\.\d+\.\d+'`} {
		if !regexp.MustCompile(pattern).MatchString(body) {
			t.Errorf("packages job lacks %s", pattern)
		}
	}
}
