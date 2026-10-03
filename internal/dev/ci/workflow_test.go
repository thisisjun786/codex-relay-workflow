//go:build dev

package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The CI workflow's structure (.github/workflows/ci.yml) and its one required check, dev-gate.
// The workflow is read as text, by its two-space job headers and six-space step items,
// deliberately without a YAML parser.

// workflowJobs is each job's body by name, and the jobs in workflow order.
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

// workflowSteps is a job's steps in order, each as its own top-level keys (key -> the rest of
// the line). A step starts at `      - ` and its keys sit eight spaces in.
func workflowSteps(t *testing.T, body string) []map[string]string {
	t.Helper()
	_, rest, found := strings.Cut(body, "\n    steps:\n")
	if !found {
		t.Fatal("the job has no steps")
	}
	key := regexp.MustCompile(`^        ([a-z][a-z-]*):(?: (.*))?$`)
	var steps []map[string]string
	for _, block := range regexp.MustCompile(`(?m)^      - `).Split(rest, -1)[1:] {
		keys := map[string]string{}
		for i, line := range strings.Split(block, "\n") {
			if i == 0 {
				line = "        " + line
			}
			if m := key.FindStringSubmatch(line); m != nil {
				keys[m[1]] = m[2]
			}
		}
		steps = append(steps, keys)
	}
	return steps
}

func sortedCopy(items []string) []string {
	out := slices.Clone(items)
	sort.Strings(out)
	return out
}

// dev-gate needs every other job, so a job cannot run outside the required check, and nothing
// else waits on anything: every check starts at once.
func TestWorkflow_the_gate_needs_every_other_job_and_nothing_else_waits(t *testing.T) {
	jobs, order := workflowJobs(t)
	var others []string
	for _, name := range order {
		if name == "dev-gate" {
			continue
		}
		others = append(others, name)
		if regexp.MustCompile(`(?m)^    needs:`).MatchString(jobs[name]) {
			t.Errorf("%s waits on another job", name)
		}
	}
	declared := regexp.MustCompile(`(?m)^    needs: \[([^\]]+)\]$`).FindStringSubmatch(jobs["dev-gate"])
	if declared == nil {
		t.Fatal("dev-gate must declare its prerequisites")
	}
	var needs []string
	for _, part := range strings.Split(declared[1], ",") {
		needs = append(needs, strings.TrimSpace(part))
	}
	expectEqual(t, "dev-gate needs", sortedCopy(needs), sortedCopy(others))
	if !regexp.MustCompile(`(?m)^    if: always\(\)$`).MatchString(jobs["dev-gate"]) {
		t.Error("dev-gate must run after a failed or cancelled prerequisite, not be skipped")
	}
}

func TestWorkflow_no_job_opts_out_of_its_result(t *testing.T) {
	jobs, _ := workflowJobs(t)
	for name, body := range jobs {
		if strings.Contains(body, "continue-on-error") {
			t.Errorf("%s carries continue-on-error", name)
		}
	}
}

func TestWorkflow_downloaded_tooling_is_pinned(t *testing.T) {
	jobs, _ := workflowJobs(t)
	for name, body := range jobs {
		for _, line := range regexp.MustCompile(`(?m)^.*uses: \S+.*$`).FindAllString(body, -1) {
			if !regexp.MustCompile(`uses: \S+@[0-9a-f]{40} # \S`).MatchString(line) {
				t.Errorf("%s: %q is not pinned by commit with its version comment", name, strings.TrimSpace(line))
			}
		}
	}
}

// Each check runs once: the contract and plugin checks in validate, never again in a Go leg,
// and nothing runs the Python implementation's suites, which left in todo 44.
func TestWorkflow_each_check_runs_once(t *testing.T) {
	jobs, _ := workflowJobs(t)
	for _, check := range []string{"ci validate", "ci plugin", "ci contracts"} {
		pattern := regexp.MustCompile(`(?m)^      - run: .*crw-dev"? ` + check + `'?$`)
		if !pattern.MatchString(jobs["validate"]) {
			t.Errorf("validate does not run crw-dev %s", check)
		}
		for name, body := range jobs {
			if name != "validate" && strings.Contains(body, check) {
				t.Errorf("%s runs crw-dev %s again", name, check)
			}
		}
	}
	for name, body := range jobs {
		for _, word := range []string{"setup-uv", "uv sync", "uv run", "pytest", "unittest", "ci scope", "ci gate"} {
			if strings.Contains(body, word) {
				t.Errorf("%s job still names %q", name, word)
			}
		}
	}
}

var (
	// pythonStep matches what installs or runs Python in a workflow line: the setup action and its
	// version input, and a python or pip command word (python3, python3.12, pip3, pipx).
	pythonStep = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])(?:setup-python|python-version|python[0-9.]*|pipx?[0-9.]*)(?:$|[^A-Za-z0-9_-])`)
	// assetStep matches a path below a skill's asset directory (the one allow-list in validate.go).
	assetStep = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(?:` + alternation(skillAssetRoots) + `)/[^/\s]+/(?:` + alternation(skillAssetDirs) + `)/`)
	// workflowComment is a YAML or shell comment: a # at the start of a line or after a space.
	workflowComment = regexp.MustCompile(`(?:^|\s)#.*$`)
)

// alternation is words as a regular expression alternative, each taken literally.
func alternation(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = regexp.QuoteMeta(word)
	}
	return strings.Join(quoted, "|")
}

// pythonInWorkflow is the lines of a workflow, numbered, that install or run Python or name a
// script below a skill's asset directories; a comment is not a line of the workflow.
func pythonInWorkflow(text string) []string {
	var found []string
	for number, line := range lines(text) {
		code := strings.TrimSpace(workflowComment.ReplaceAllString(line, ""))
		if pythonStep.MatchString(code) || assetStep.MatchString(code) {
			found = append(found, fmt.Sprintf("%d: %s", number+1, code))
		}
	}
	return found
}

// CI installs no Python and runs no skill asset (CRW-483): the checks are Go only, so no workflow
// sets up an interpreter, calls python or pip, or names a script under a skill's asset
// directories, which are original assets an agent runs and no CI step does. Whether CI should
// test skill scripts is a decision of its own.
func TestWorkflow_installs_no_python(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(), ".github", "workflows", "*.y*ml"))
	if err != nil || len(files) < 2 {
		t.Fatalf("workflow files = %v, %v", files, err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range pythonInWorkflow(string(data)) {
			t.Errorf("%s installs or runs Python, or runs a skill asset, at line %s", filepath.Base(file), line)
		}
	}
}

// The detector refuses what installs or runs Python in a workflow, and a step that names a skill
// asset script, and lets comments, other words that contain pip or python, and ordinary skill
// paths through.
func TestWorkflow_python_detector(t *testing.T) {
	for _, row := range []struct {
		line  string
		found bool
	}{
		{"      - uses: actions/setup-python@0123456789abcdef0123456789abcdef01234567 # v5", true},
		{"          python-version: '3.12'", true},
		{"      - run: python3 -m pytest", true},
		{"      - run: python tools/check.py", true},
		{"      - run: /usr/bin/python3.12 tools/check", true},
		{"      - run: sh -c 'python -V'", true},
		{"      - run: pip install -r requirements.txt", true},
		{"      - run: pip3 install build", true},
		{"      - run: pipx run build", true},
		{"      - run: ./plugins/crw/skills/example/scripts/helper.py", true},
		{"      - run: \"$GITHUB_WORKSPACE/plugins/crw/skills/example/scripts/helper\"", true},
		{"      - run: sh port/cxc/skills/crw-example/examples/demo.sh", true},
		{"      - run: go test ./...", false},
		{"      - run: go test ./... # no python here", false},
		{"      # python is installed by nobody", false},
		{"          set -euo pipefail", false},
		{"      - run: echo pipeline cpython", false},
		{"      - run: cat plugins/crw/skills/example/SKILL.md", false},
		{"      - run: ls plugins/crw/skills/example/references port/cxc/skills", false},
		{"      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0", false},
	} {
		if got := pythonInWorkflow(row.line + "\n"); (len(got) > 0) != row.found {
			t.Errorf("%q: found = %q, want found = %v", row.line, got, row.found)
		}
	}
	expectEqual(t, "line number", pythonInWorkflow("jobs:\n  a:\n    steps:\n      - run: |\n          make test\n          python3 x.py\n"),
		[]string{"6: python3 x.py"})
}

// matrixValues reads a one-line `key: [a, b]` matrix entry from a job body.
func matrixValues(t *testing.T, body, key string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^        ` + key + `: \[([^\]]+)\]$`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s matrix", key)
	}
	var values []string
	for _, part := range strings.Split(m[1], ",") {
		values = append(values, strings.Trim(strings.TrimSpace(part), "'"))
	}
	return values
}

// The Go suite runs in CI as `make test-part` legs. A Makefile part without a leg would never
// run, and the isolated-home integration test runs in the dist leg, after the build, against the
// binary it built.
func TestWorkflow_parallel_legs_cover_the_whole_run(t *testing.T) {
	jobs, _ := workflowJobs(t)
	data, err := os.ReadFile(filepath.Join(repoRoot(), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	var numbered []string
	for _, m := range regexp.MustCompile(`(?m)^TEST_PART_(\d+) :=`).FindAllStringSubmatch(string(data), -1) {
		numbered = append(numbered, m[1])
	}
	filtered := regexp.MustCompile(`\$\(filter \$\(TEST_PART\),([^)]*)\)`).FindStringSubmatch(string(data))
	if filtered == nil {
		t.Fatal("Makefile does not validate TEST_PART")
	}
	expectEqual(t, "validated parts", strings.Fields(filtered[1]), numbered)
	want := []string{"lint", "dist", "test-rest"}
	for _, n := range numbered {
		want = append(want, "test-"+n)
	}
	expectEqual(t, "go-product legs", sortedCopy(matrixValues(t, jobs["go-product"], "part")), sortedCopy(want))
	for _, step := range []string{"run: make lint", "run: make test-part TEST_PART="} {
		if !strings.Contains(jobs["go-product"], step) {
			t.Errorf("go-product lacks %q", step)
		}
	}
	steps := workflowSteps(t, jobs["go-product"])
	index := map[string][]int{}
	for i, step := range steps {
		index[step["name"]] = append(index[step["name"]], i)
	}
	build, wired := "Build the static binaries for every published target", "Install and wire the release binary in an isolated home"
	for _, name := range []string{build, wired} {
		if len(index[name]) != 1 {
			t.Fatalf("go-product has %d steps named %q, want one", len(index[name]), name)
		}
		if got := steps[index[name][0]]["if"]; got != "matrix.part == 'dist'" {
			t.Errorf("%q runs if %q, want matrix.part == 'dist'", name, got)
		}
	}
	if index[wired][0] < index[build][0] {
		t.Errorf("%q runs before %q", wired, build)
	}
	if want := `CRW_TEST_BINARY="$PWD/dist/crw_linux_amd64/crw" go test -trimpath -tags integration -count=1 ./internal/runtime/integration/...`; steps[index[wired][0]]["run"] != want {
		t.Errorf("%q runs %q, want %q", wired, steps[index[wired][0]]["run"], want)
	}
}

// legProblems lists what would make a package run in two legs, or a leg run nothing. The numbered
// parts name their packages and `rest` is every other package (`go list ./...` less TEST_PARTS), so
// a package runs twice when two parts name it or TEST_PARTS misses a part, and a named path without
// tests runs nothing. The numbered parts run without the dev tag and `rest` runs the dev-tagged
// tests in its second pass, so a part cannot name one of those packages. A package is the cleaned
// path of its directory, so `./internal/relay/cli/` and `./internal/relay/cli` are one package.
func legProblems(makefile, root string) []string {
	fields := func(name string) []string {
		m := regexp.MustCompile(`(?m)^` + name + ` :=(.*)$`).FindStringSubmatch(makefile)
		if m == nil {
			return nil
		}
		return strings.Fields(m[1])
	}
	var problems []string
	var parts []string
	owner := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^TEST_PART_(\d+) :=`).FindAllStringSubmatch(makefile, -1) {
		n := m[1]
		parts = append(parts, "$(TEST_PART_"+n+")")
		for _, pkg := range fields("TEST_PART_" + n) {
			dir := path.Clean(pkg)
			if other, named := owner[dir]; named {
				problems = append(problems, fmt.Sprintf("%s is named by part %s and by part %s", pkg, other, n))
				continue
			}
			owner[dir] = n
			tests, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), "*_test.go"))
			switch {
			case !strings.HasPrefix(pkg, "./") || strings.Contains(pkg, "...") || dir == ".." || strings.HasPrefix(dir, "../"):
				problems = append(problems, fmt.Sprintf("part %s names %q, which is not one package directory", n, pkg))
			case strings.HasPrefix(dir, "cmd/crw-dev") || dir == "internal/dev" || strings.HasPrefix(dir, "internal/dev/"):
				problems = append(problems, fmt.Sprintf("part %s names %s, whose tests need the dev tag and run in rest", n, pkg))
			case len(tests) == 0:
				problems = append(problems, fmt.Sprintf("part %s names %s, which has no tests", n, pkg))
			}
		}
	}
	if got := fields("TEST_PARTS"); !slices.Equal(got, parts) {
		problems = append(problems, fmt.Sprintf("TEST_PARTS is %v, want the numbered parts %v", got, parts))
	}
	return problems
}

// Every package runs in exactly one leg: the one part that names it, or `rest`.
func TestMakefile_every_package_runs_in_exactly_one_leg(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if problems := legProblems(string(data), repoRoot()); len(problems) > 0 {
		t.Errorf("the legs do not run every package exactly once:\n%s", strings.Join(problems, "\n"))
	}
}

// The check refuses each way a package would run twice or a leg would run nothing.
func TestMakefile_leg_check_refuses_a_package_in_two_legs_or_none(t *testing.T) {
	makefile := "TEST_PART_1 := ./internal/relay/cli ./internal/relay/hook\nTEST_PART_2 := ./internal/relay/store\nTEST_PARTS := $(TEST_PART_1) $(TEST_PART_2)\n"
	if problems := legProblems(makefile, repoRoot()); len(problems) != 0 {
		t.Fatalf("a sound Makefile is refused: %v", problems)
	}
	for name, mutated := range map[string]string{
		"a package named by two parts":                  strings.Replace(makefile, "./internal/relay/store", "./internal/relay/cli", 1),
		"a package named twice by equivalent spellings": strings.Replace(makefile, "./internal/relay/store", "./internal/relay/cli/", 1),
		"a package named twice through a parent":        strings.Replace(makefile, "./internal/relay/store", "./internal/relay/../relay/cli", 1),
		"a part missing from TEST_PARTS":                strings.Replace(makefile, " $(TEST_PART_2)", "", 1),
		"a path with no tests":                          strings.Replace(makefile, "./internal/relay/store", "./internal/relay/nowhere", 1),
		"a dev-tagged package":                          strings.Replace(makefile, "./internal/relay/store", "./internal/dev/ci", 1),
		"a pattern instead of one package":              strings.Replace(makefile, "./internal/relay/store", "./internal/relay/...", 1),
	} {
		if len(legProblems(mutated, repoRoot())) == 0 {
			t.Errorf("%s passes", name)
		}
	}
}

// gateScript is dev-gate's one shell step, dedented.
func gateScript(t *testing.T) string {
	t.Helper()
	jobs, _ := workflowJobs(t)
	_, block, found := strings.Cut(jobs["dev-gate"], "\n        run: |\n")
	if !found {
		t.Fatal("dev-gate has no literal shell step")
	}
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "          "))
	}
	return strings.Join(lines, "\n")
}

// gate runs dev-gate's script as the runner does, from a script file with bash's -e and
// pipefail, with needs as the text GitHub substitutes for toJSON(needs), and returns its exit
// status and output.
func gate(t *testing.T, needs, event, base string) (int, string) {
	t.Helper()
	const expression = "${{ toJSON(needs) }}"
	script := gateScript(t)
	if strings.Count(script, expression) != 1 {
		t.Fatalf("dev-gate's script must write %s once:\n%s", expression, script)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("dev-gate's script needs jq, which the hosted runner has")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "gate.sh")
	if err := os.WriteFile(file, []byte(strings.Replace(script, expression, needs, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", file)
	cmd.Env = append(os.Environ(), "RUNNER_TEMP="+dir, "EVENT_NAME="+event, "BASE_REF="+base)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// results is toJSON(needs) as GitHub renders it: each job's result and (empty) outputs.
func results(states map[string]string) string {
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		parts = append(parts, "  \""+name+"\": {\n    \"result\": \""+states[name]+"\",\n    \"outputs\": {}\n  }")
	}
	return "{\n" + strings.Join(parts, ",\n") + "\n}"
}

// The gate passes only an all-success run: a failed, cancelled or skipped prerequisite, an empty
// or unreadable result and a pull request into main each fail it. The results reach it through a
// file written by its own script, so their size is not bounded by the environment's.
func TestWorkflow_the_gate_passes_only_when_every_prerequisite_succeeded(t *testing.T) {
	jobs, _ := workflowJobs(t)
	if regexp.MustCompile(`(?m)^ +[A-Z_]+: \$\{\{ toJSON\(needs\)`).MatchString(jobs["dev-gate"]) {
		t.Error("dev-gate passes the results in the environment")
	}
	all := map[string]string{"validate": "success", "secrets": "success", "go-product": "success"}
	for _, event := range [][2]string{{"pull_request", "dev"}, {"pull_request", "codex/parent"}, {"push", ""}, {"workflow_dispatch", ""}} {
		if code, out := gate(t, results(all), event[0], event[1]); code != 0 {
			t.Errorf("%v: an all-success run is refused (%d)\n%s", event, code, out)
		}
	}
	if code, _ := gate(t, results(all), "pull_request", "main"); code == 0 {
		t.Error("a pull request into main passes")
	}
	for _, job := range []string{"validate", "secrets", "go-product"} {
		for _, state := range []string{"failure", "cancelled", "skipped", ""} {
			one := map[string]string{"validate": "success", "secrets": "success", "go-product": "success"}
			one[job] = state
			if code, _ := gate(t, results(one), "pull_request", "dev"); code == 0 {
				t.Errorf("%s %q passes", job, state)
			}
		}
	}
	for _, raw := range []string{"{}", "[]", "null", "{", `{"validate": "success"}`, `{"validate": {}}`} {
		if code, _ := gate(t, raw, "pull_request", "dev"); code == 0 {
			t.Errorf("results %q pass", raw)
		}
	}
	// Larger than one environment string may be (128 KiB on Linux): the results are a file.
	many := map[string]string{}
	for i := range 4000 {
		many[fmt.Sprintf("job-%04d", i)] = "success"
	}
	if raw := results(many); len(raw) <= 128<<10 {
		t.Fatalf("the large result is only %d bytes", len(raw))
	}
	if code, out := gate(t, results(many), "push", ""); code != 0 {
		t.Errorf("a large all-success result is refused (%d)\n%.400s", code, out)
	}
}
