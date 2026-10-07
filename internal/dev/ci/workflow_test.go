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
		// workflowJobHeader is the one reader of a job key, so this walk and the Node-run
		// detector cannot disagree about where a job starts (CRW-939).
		if name, ok := workflowJobHeader(line); ok {
			current = name
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
// and nothing runs the Python implementation's suites, which left in todo 44. The screen-drift
// check (CRW-831) runs once too, in the one job whose subject is the screens.
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
	if !regexp.MustCompile(`(?m)^        run: .*crw-dev ci gui-drift --built .*$`).MatchString(jobs["gui"]) {
		t.Error("gui does not run crw-dev ci gui-drift")
	}
	for name, body := range jobs {
		if name != "gui" && strings.Contains(body, "ci gui-drift") {
			t.Errorf("%s runs crw-dev ci gui-drift again", name)
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

// The gui job decides whether to verify the screens from the changed files, before it installs
// Node, and every step that needs Node or the Go toolchain carries that answer. The build goes to
// a temporary tree, never to the committed internal/gui/assets, so the drift check compares the
// commit against a real rebuild rather than against itself (CRW-831).
func TestWorkflow_the_gui_job_gates_the_screen_verification(t *testing.T) {
	jobs, _ := workflowJobs(t)
	steps := workflowSteps(t, jobs["gui"])
	guard := "steps.paths.outputs.changed == 'true'"
	gate := -1
	for i, step := range steps {
		if step["run"] == "bash scripts/ci/gui_paths.sh" {
			gate = i
			if step["id"] != "paths" {
				t.Errorf("the path gate has id %q, want paths", step["id"])
			}
			if !strings.Contains(step["if"], "steps.mirror.outputs.mirrored != 'true'") {
				t.Errorf("the path gate runs if %q, which does not carry the mirror guard", step["if"])
			}
		}
	}
	if gate < 0 {
		t.Fatal("the gui job does not run bash scripts/ci/gui_paths.sh")
	}
	// Nothing before the gate may install Node or Go: the point of the gate is that an unrelated
	// change never pays for a runner's toolchain setup.
	for i, step := range steps[:gate] {
		if strings.Contains(step["uses"], "setup-node") || strings.Contains(step["uses"], "setup-go") {
			t.Errorf("step %d (%q) installs a toolchain before the path gate", i, step["name"])
		}
	}
	built, drift := "", ""
	for i, step := range steps[gate+1:] {
		if !strings.Contains(step["if"], guard) {
			t.Errorf("step %d (%q) after the gate runs if %q, which does not carry %q", gate+1+i, step["name"], step["if"], guard)
		}
		if strings.Contains(step["run"], "npm run build") {
			built = step["run"]
			if strings.Contains(step["run"], "internal/gui/assets") {
				t.Errorf("the build writes the committed tree: %q", step["run"])
			}
			if !strings.Contains(step["run"], "RUNNER_TEMP") {
				t.Errorf("the build does not write a temporary tree: %q", step["run"])
			}
		}
		if strings.Contains(step["run"], "ci gui-drift") {
			drift = step["run"]
		}
	}
	if built == "" {
		t.Error("the gui job never builds the screens")
	}
	// The drift check must read the very directory the build wrote: a typo in either the --outDir
	// or the --built value would otherwise compare something else and still pass this test.
	builtDir := workflowFlagValue(t, built, "--outDir")
	driftDir := workflowFlagValue(t, drift, "--built")
	if builtDir == "" || builtDir != driftDir {
		t.Errorf("the build writes %q but the drift check reads %q", builtDir, driftDir)
	}
}

// workflowFlagValue reads the value a shell command passes to a flag, quoted or bare, up to the
// next blank. It is how a test holds two steps to the same directory without matching prose.
func workflowFlagValue(t *testing.T, command, flag string) string {
	t.Helper()
	for _, field := range strings.Fields(command) {
		if rest, ok := strings.CutPrefix(field, flag+"="); ok {
			return strings.Trim(rest, `"'`)
		}
	}
	for i, field := range strings.Fields(command) {
		if field != flag {
			continue
		}
		if i+1 < len(strings.Fields(command)) {
			return strings.Trim(strings.Fields(command)[i+1], `"'`)
		}
	}
	return ""
}

var (
	// pythonStep matches what installs or runs Python in a workflow line: the setup action and its
	// version input, and a python or pip word (python3, python3.12, pip3, pipx, python3-minimal).
	pythonStep = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])(?:setup-python|python-version|python[0-9.]*|pipx?[0-9.]*)(?:$|[^A-Za-z0-9_])`)
	// skillStep matches a path below a skills directory (the roots of the one allow-list in
	// validate.go): a step cannot run a skill's script without naming where it lives, in a
	// working-directory or a cd as well as in the command.
	skillStep = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(?:` + alternation(skillAssetRoots) + `)/[^/\s]`)
	// skillRootValue matches an assignment whose value is exactly a skills root, quoted or not:
	// the root followed by the end of the line, a quote, or a blank. The roots alone are no skill
	// path (a listing names them), but a job that carries one as a value names where the skill
	// scripts live, so it is the other shape this detector has to see (CRW-353).
	skillRootValue = regexp.MustCompile(`^\s*[A-Za-z_][A-Za-z0-9_-]*:\s*["']?(?:` + alternation(skillAssetRoots) + `)/?["']?(?:\s|$)`)
)

// skillScriptsNodeJob is the one job whose subject is the staged skills' Node tests (the
// 2026-10-06 decision, CRW-353). A skill-path line is admitted only inside it.
const skillScriptsNodeJob = "skill-scripts-node"

// skillScriptsNodeFile is the workflow that job lives in. The exception is keyed by file and job
// together (CRW-939): a job name is not a place, so a workflow that copied the job would otherwise
// inherit the one allow-list's exception and run a skill script outside the subject that admits it.
const skillScriptsNodeFile = "ci.yml"

// jobKeyLine is a key at a job's own indentation: exactly two spaces, a YAML key, a colon, and then
// nothing, a blank or a comment. YAML accepts more than a lower-case hyphenated name -- a "_" or a
// "." in the key, an upper-case letter, and a single- or double-quoted key all open a job -- and
// every one of them has to reset the one Node-run exception. A header this reader missed would
// leave the exception switched on, and the job below it could run a skill script under a name that
// is not skill-scripts-node (CRW-939, the generation-1 pre-merge evaluation).
// The tail is deliberately unconstrained: a job key's value is whatever follows the colon, and
// `key:`, `key: # comment`, `key: value`, `key:#value` and `key :` all open the same job. Two
// spaces is a job's own indentation, so a step's key (eight) and a job's `runs-on:` (four) are not
// read as one. A matched line still has its own text checked (pythonInWorkflow does not skip it),
// because a workflow-level `env:` value and a one-line flow job are read here too and their content
// can name a skill path.
var jobKeyLine = regexp.MustCompile(`^  (?:"([^"]*)"|'([^']*)'|([A-Za-z0-9_.-]+))[ \t]*:.*$`)

// alternation is words as a regular expression alternative, each taken literally.
func alternation(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = regexp.QuoteMeta(word)
	}
	return strings.Join(quoted, "|")
}

// pythonInWorkflow is the lines of the workflow file, numbered, that install or run Python or name
// a path below a skills directory. A line that starts with # is a comment and is skipped; a
// trailing # is read as part of the line, because a # inside quotes hides nothing from the shell.
//
// The staged skills' Node tests run in one named job of one named workflow, and a skill-path line
// is admitted only inside that pair: the job names its root once and runs the tests it finds there,
// so neither the root value nor a path below it is a skill script running anywhere else. file is
// the workflow's path or base name; the job name alone does not carry the exception (CRW-939). The
// Python rule is unchanged in every job, that one included. Every line YAML reads as a key at a
// job's own indentation starts a new job and resets the exception, so a job cannot be added in a
// form this detector misses. Resetting is not skipping: the line is still read for a Python or
// skill-path token, because a workflow-level `env:` value and a one-line flow job share this shape
// and either can name a skill path or run one (CRW-939, the generation-2 evaluation).
func pythonInWorkflow(file, text string) []string {
	var found []string
	admitted := filepath.Base(file) == skillScriptsNodeFile
	inJobs, inSkillScriptsNode := false, false
	for number, line := range lines(text) {
		// A key at column 0 is a top-level key: it opens or closes the jobs block, and the job
		// exception lives only inside it. A workflow-level `env:` value whose key happens to be
		// skill-scripts-node shares the two-space key shape, so without this the exception would
		// switch on before `jobs:` and a job that reads the variable would run a skill test with no
		// finding (CRW-939, the generation-2 evaluation's d1).
		if line != "" && !strings.HasPrefix(line, " ") {
			inJobs, inSkillScriptsNode = strings.HasPrefix(line, "jobs:"), false
		} else if inJobs {
			if name, ok := workflowJobHeader(line); ok {
				inSkillScriptsNode = admitted && name == skillScriptsNodeJob
			}
		}
		code := strings.TrimSpace(line)
		skillPath := skillStep.MatchString(code) || skillRootValue.MatchString(code)
		if !strings.HasPrefix(code, "#") && (pythonStep.MatchString(code) || (skillPath && !inSkillScriptsNode)) {
			found = append(found, fmt.Sprintf("%d: %s", number+1, code))
		}
	}
	return found
}

// workflowJobHeader is the key when line opens a job: exactly two spaces, then a YAML key and a
// colon, then nothing, a blank or a comment. The key is a plain name or the contents of a quoted
// one; a key the reader cannot name still reports true, because the line starts a new job either
// way and the exception must reset. workflowJobs reads the same two-space shape, and
// TestWorkflow_the_job_key_reader_matches_workflowJobs holds the two readers to each other on the
// real ci.yml.
func workflowJobHeader(line string) (string, bool) {
	m := jobKeyLine.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	for _, key := range m[1:] {
		if key != "" {
			return key, true
		}
	}
	return "", true
}

// workflowPythonFindings reads every workflow file under dir and reports what pythonInWorkflow
// refuses, by file name. The caller passes a workflows directory, so a test can judge a synthetic
// one without writing a workflow into the repository.
func workflowPythonFindings(t *testing.T, dir string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	findings := map[string][]string{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if found := pythonInWorkflow(file, string(data)); len(found) > 0 {
			findings[filepath.Base(file)] = found
		}
	}
	return findings
}

// CI installs no Python (CRW-483), and the only skill scripts it runs are the staged skills' Node
// tests in the one skill-scripts-node job of ci.yml (the 2026-10-06 decision, CRW-353; CRW-939):
// the checks are Go only, so no workflow sets up an interpreter or calls python or pip, and a
// helper script in a skill's scripts/ or examples/ stays an original asset an agent runs. The job
// names its root once and runs the tests it finds below it, so pythonInWorkflow admits a skill-path
// line only inside that file and job — a skill path or a skills-root value in any other job, or in
// the same job name under another workflow, is still refused, which is what stops a job from
// reaching a skill script by moving the path into a variable or by copying the job's name.
func TestWorkflow_installs_no_python(t *testing.T) {
	dir := filepath.Join(repoRoot(), ".github", "workflows")
	files, err := filepath.Glob(filepath.Join(dir, "*.y*ml"))
	if err != nil || len(files) < 2 {
		t.Fatalf("workflow files = %v, %v", files, err)
	}
	for name, lines := range workflowPythonFindings(t, dir) {
		for _, line := range lines {
			t.Errorf("%s installs or runs Python, or runs a skill asset, at line %s", name, line)
		}
	}
}

// The exception is one file and one job. ci.yml's skill-scripts-node job may name a skill path;
// the same job name in another workflow is a finding, because a job name is not a place and a
// copied job would otherwise widen the allow-list's boundary (CRW-939). A workflow file that is
// not ci.yml, or another job of ci.yml, is refused for the same reason.
func TestWorkflow_the_skill_scripts_exception_is_one_file_and_one_job(t *testing.T) {
	job := "name: extra\n\njobs:\n  skill-scripts-node:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: node --test port/cxc/skills/crw-qa/tests/a.test.mjs\n"
	for _, file := range []string{"extra.yml", "release.yml", filepath.Join(".github", "workflows", "extra.yml")} {
		if got := pythonInWorkflow(file, job); len(got) == 0 {
			t.Errorf("%s admits a node --test run in a job named skill-scripts-node", file)
		}
	}
	if got := pythonInWorkflow("ci.yml", job); len(got) != 0 {
		t.Errorf("ci.yml's skill-scripts-node job is refused: %q", got)
	}
	if got := pythonInWorkflow(filepath.Join(".github", "workflows", "ci.yml"), job); len(got) != 0 {
		t.Errorf("ci.yml's skill-scripts-node job is refused when the path is given: %q", got)
	}
	// The file alone is not enough either: another job of ci.yml is still refused.
	other := "name: ci\n\njobs:\n  other:\n    steps:\n      SKILLS_ROOT: port/cxc/skills\n"
	if got := pythonInWorkflow("ci.yml", other); len(got) == 0 {
		t.Error("ci.yml admits a skills-root value outside the skill-scripts-node job")
	}
}

// Every line YAML reads as a key at a job's own indentation starts a new job, so the one Node-run
// exception cannot survive into the job below it. YAML accepts more job keys than the lower-case
// hyphenated shape the first reader knew: a key with a "_" or a ".", an upper-case key, a single-
// or double-quoted key, and a trailing comment after the colon are all one key. A header the
// detector does not read would leave the exception switched on, and the next job of ci.yml could
// then run a skill script under a name that is not skill-scripts-node (CRW-939, the generation-1
// pre-merge evaluation).
func TestWorkflow_every_yaml_job_key_resets_the_skill_scripts_exception(t *testing.T) {
	const head = "name: ci\n\njobs:\n  skill-scripts-node:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: node --test port/cxc/skills/crw-qa/tests/a.test.mjs\n"
	const run = "      - run: node --test port/cxc/skills/crw-qa/tests/b.test.mjs\n"
	// The real job stays clean: the exception is the point of the control.
	if got := pythonInWorkflow("ci.yml", head); len(got) != 0 {
		t.Fatalf("the real skill-scripts-node job is refused: %q", got)
	}
	for _, header := range []string{
		"  extra_job:\n",                      // a '_' in the key
		"  extra.job:\n",                      // a '.' in the key
		"  'quoted-job':\n",                   // a single-quoted key
		"  \"quoted-job\":\n",                 // a double-quoted key
		"  extra-job: # a trailing comment\n", // a comment after the colon
		"  extra-job:  # two spaces\n",
		"  ExtraJob:\n",                    // an upper-case key
		"  extra-job:\r\n",                 // a CRLF line ending
		"  extra-job: \n",                  // a trailing blank
		"  'quoted job':\n",                // a quoted key with a space
		"  extra-job :\n",                  // a blank before the colon
		"  extra-job:#no space after it\n", // no space after the colon
		"  extra-job: value\n",             // an inline value
		"  extra-job: {a: 1}\n",            // a flow mapping value
		"  extra-job: |\n",                 // a block scalar
		"  extra-job: &anchor\n",           // an anchored value
		"  \"quo#ted\":\n",                 // a '#' inside a quoted key
		"  job1:\n",                        // digits only
		"  JOB-2:\n",                       // upper case with a hyphen
		"  j.o_b-3:\n",                     // every allowed character at once
	} {
		body := head + header + "    runs-on: ubuntu-24.04\n    steps:\n" + run
		if got := pythonInWorkflow("ci.yml", body); len(got) == 0 {
			t.Errorf("a job opened by %q inherits the Node-run exception: the run is not refused", strings.TrimSpace(header))
		}
	}
	// Resetting the exception on a line is not skipping it. A workflow-level `env:` value and a
	// one-line flow job share the two-space key shape, and either can name a skill path or run one,
	// so the line is still read for a Python or skill-path token. A reader that reset the exception
	// by dropping the whole line let a skill script run under a variable (CRW-939, the generation-2
	// evaluation).
	for _, row := range []struct{ name, body string }{
		{"a workflow-level env value and a job that uses it",
			"name: extra\n\nenv:\n  SKILLS_ROOT: port/cxc/skills\n\njobs:\n  extra_job:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: node --test \"$SKILLS_ROOT\"/crw-qa/tests/a.test.mjs\n"},
		{"a one-line flow job running a staged skill test",
			"name: extra\n\njobs:\n  skill-scripts-node: {runs-on: ubuntu-24.04, steps: [{run: 'node --test port/cxc/skills/crw-qa/tests/a.test.mjs'}]}\n"},
	} {
		if got := pythonInWorkflow("extra.yml", row.body); len(got) == 0 {
			t.Errorf("%s: the line that reset the exception was skipped and no finding was raised", row.name)
		}
	}
	// The real ci.yml's own workflow-level env: and its jobs stay clean.
	for _, file := range []string{"ci.yml", "release.yml"} {
		data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", file))
		if err != nil {
			t.Fatal(err)
		}
		if got := pythonInWorkflow(file, string(data)); len(got) != 0 {
			t.Errorf("the real %s is refused: %q", file, got)
		}
	}
	// A key nested below a job, and a step's own key, are not job keys: the detector must not reset
	// the exception on every two-space line, or the job it admits would refuse its own root and its
	// own test run.
	for _, line := range []string{
		"    runs-on: ubuntu-24.04\n",
		"      - run: node --test port/cxc/skills/crw-qa/tests/a.test.mjs\n",
		"        working-directory: port/cxc/skills/crw-qa\n",
	} {
		body := "name: ci\n\njobs:\n  skill-scripts-node:\n" + line
		if got := pythonInWorkflow("ci.yml", body); len(got) != 0 {
			t.Errorf("%q is not a job key but reset the exception: %q", strings.TrimSpace(line), got)
		}
	}
	// The exception lives inside the jobs block. A workflow-level key whose name is
	// skill-scripts-node shares the two-space key shape, so a reader that reset the exception on
	// every two-space line switched it on before `jobs:` and let a job that reads the value run a
	// skill test with no finding (CRW-939, the generation-2 evaluation's d1).
	globalEnv := "name: ci\nenv:\n  skill-scripts-node: port/cxc/skills\njobs:\n  gui:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: node --test \"${{ env['skill-scripts-node'] }}\"/crw-qa/tests/a.test.mjs\n"
	if got := pythonInWorkflow("ci.yml", globalEnv); len(got) == 0 {
		t.Error("a workflow-level key named skill-scripts-node switched the exception on before jobs:")
	}
	// The control: the same value inside the real job is the one admitted place, and the real
	// ci.yml carries no workflow-level key of that name.
	insideJob := "name: ci\njobs:\n  skill-scripts-node:\n    runs-on: ubuntu-24.04\n    env:\n      skill-scripts-node: port/cxc/skills\n    steps:\n      - run: node --test port/cxc/skills/crw-qa/tests/a.test.mjs\n"
	if got := pythonInWorkflow("ci.yml", insideJob); len(got) != 0 {
		t.Errorf("the real job's own root is refused: %q", got)
	}
}

// The job-key reader finds exactly the jobs the real ci.yml declares, in order and nothing else: the
// whole file, not a fragment, so a shape the reader over- or under-reads shows up as a missing or an
// extra job. The expected list is written out here, so this is evidence about the reader and not a
// second reading of the same function.
func TestWorkflow_the_job_key_reader_finds_exactly_the_real_jobs(t *testing.T) {
	want := []string{"validate", "secrets", "skill-scripts-node", "gui", "go-product", "dev-gate"}
	data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	inside := false
	for _, line := range lines(string(data)) {
		if line != "" && !strings.HasPrefix(line, " ") {
			inside = strings.HasPrefix(line, "jobs:")
			continue
		}
		if !inside {
			continue
		}
		if name, ok := workflowJobHeader(line); ok {
			seen = append(seen, name)
		}
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("the job-key reader found %v, want the real jobs %v", seen, want)
	}
}

// A workflow that copies ci.yml's skill-scripts-node job is refused by the file check, driven
// through the same directory glob the repository check reads.
func TestWorkflow_a_copied_skill_scripts_node_job_in_another_workflow_is_a_finding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: extra\n\njobs:\n  skill-scripts-node:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: node --test port/cxc/skills/crw-qa/tests/a.test.mjs\n"
	if err := os.WriteFile(filepath.Join(dir, "extra.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if findings := workflowPythonFindings(t, dir); len(findings["extra.yml"]) == 0 {
		t.Error("extra.yml's skill-scripts-node job runs node --test and is not refused")
	}
	// The same body in ci.yml is the one admitted place.
	if err := os.Remove(filepath.Join(dir, "extra.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if findings := workflowPythonFindings(t, dir); len(findings) != 0 {
		t.Errorf("ci.yml's own skill-scripts-node job is refused: %v", findings)
	}
}

// The detector refuses what installs or runs Python in a workflow, and a step that names a skill
// asset script, and lets comments, other words that contain pip or python, and ordinary skill
// paths through.
func TestWorkflow_python_detector(t *testing.T) {
	// Every row is a line of a workflow that is not ci.yml. The one job whose skill-path lines are
	// admitted lives in ci.yml, so the same job name in another workflow is judged like any other
	// job: the exception is keyed by file and job together (CRW-939).
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
		{"      - run: sudo apt-get install -y python3-minimal", true}, // an interpreter package is an install too
		{"      - run: apt-get install python3-dev python3-pip", true},
		{"      - run: ./plugins/crw/skills/example/scripts/helper.py", true},
		{"      - run: \"$GITHUB_WORKSPACE/plugins/crw/skills/example/scripts/helper\"", true},
		{"      - run: sh port/cxc/skills/crw-example/examples/demo.sh", true},
		{"      - run: printf '%s\\n' ' # marker'; python3 -V", true},             // a # inside quotes hides nothing
		{"          working-directory: plugins/crw/skills/example/scripts", true}, // a step runs a script by its directory
		{"      - run: cd port/cxc/skills/crw-example && ./helper", true},
		{"      - run: cat plugins/crw/skills/example/SKILL.md", true}, // any path below a skills root
		{"      - run: go test ./... # no python here", true},          // a trailing comment is part of the line
		{"      - run: go test ./...", false},
		{"      # python is installed by nobody", false},
		{"          set -euo pipefail", false},
		{"      - run: echo pipeline cpython", false},
		{"      - run: ls plugins/crw/skills port/cxc/skills/ plugins/crw/skillset/x", false},  // the roots themselves are no skill path
		{"      SKILLS_ROOT: port/cxc/skills", true},                                           // a value that is exactly a skills root, outside the one job allowed to name it
		{"jobs:\n  skill-scripts-node:\n    steps:\n      SKILLS_ROOT: port/cxc/skills", true}, // the job's own name is not enough in another workflow
		{"jobs:\n  other:\n    steps:\n      SKILLS_ROOT: port/cxc/skills", true},              // the same value in another named job
		{"      SKILLS_ROOT: 'port/cxc/skills'", true},                                         // the quoted form is the same value
		{"      SKILLS_ROOT: port/cxc/skills # the staged skills", true},                       // and so is the form a trailing blank ends
		{"      - run: node --test port/cxc/skills/x/tests/a.test.mjs", true},                  // a skill path in any other job
		{"      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0", false},
	} {
		if got := pythonInWorkflow("release.yml", row.line+"\n"); (len(got) > 0) != row.found {
			t.Errorf("%q: found = %q, want found = %v", row.line, got, row.found)
		}
	}
	// Inside ci.yml's own skill-scripts-node job the same value is the job's own root.
	if got := pythonInWorkflow("ci.yml", "jobs:\n  skill-scripts-node:\n    steps:\n      SKILLS_ROOT: port/cxc/skills\n"); len(got) != 0 {
		t.Errorf("ci.yml's skill-scripts-node root is refused: %q", got)
	}
	expectEqual(t, "line number", pythonInWorkflow("ci.yml", "jobs:\n  a:\n    steps:\n      - run: |\n          make test\n          python3 x.py\n"),
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
		// A body-only edit that mirrors this leg stands these two steps down with the rest.
		// CRW-790 appends the light guard to every go-product step, so a light leg does no work.
		want := "matrix.part == 'dist' && steps.mirror.outputs.mirrored != 'true' && env.CRW_LIGHT_LEG != 'true'"
		if got := steps[index[name][0]]["if"]; got != want {
			t.Errorf("%q runs if %q, want %q", name, got, want)
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

// skillPathsStep is the skill-scripts-node job's changed-path step, dedented: the shell the runner
// executes when it decides whether the staged skills changed. It is the one step of that job whose
// name begins with "Decide from the changed files".
func skillPathsStep(t *testing.T) string {
	t.Helper()
	jobs, _ := workflowJobs(t)
	const name = "      - name: Decide from the changed files whether the staged skills changed\n"
	_, after, found := strings.Cut(jobs["skill-scripts-node"], name)
	if !found {
		t.Fatal("skill-scripts-node has no changed-path step")
	}
	_, block, found := strings.Cut(after, "\n        run: |\n")
	if !found {
		t.Fatal("the changed-path step has no literal shell step")
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

// skillPathsRun runs the job's changed-path shell as the runner does — from a script file with
// bash's -e and pipefail — and returns the changed= line it wrote, or "" when it wrote none. The
// variables the workflow passes are the only ones the step reads, so any inherited copy is dropped
// first: a test process running under Actions must not change the answer.
func skillPathsRun(t *testing.T, r *fixtureRepo, env ...string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "github-output")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "skill_paths.sh")
	if err := os.WriteFile(script, []byte(skillPathsStep(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	drop := []string{"PR_BASE_SHA=", "PR_HEAD_SHA=", "PUSH_BEFORE_SHA=", "GITHUB_SHA=", "GITHUB_OUTPUT=", "SKILLS_ROOT="}
	var base []string
	for _, entry := range os.Environ() {
		if !slices.ContainsFunc(drop, func(prefix string) bool { return strings.HasPrefix(entry, prefix) }) {
			base = append(base, entry)
		}
	}
	// The workflow always passes all four: the step reads them under set -u, so an event with no
	// base passes the empty string rather than nothing.
	base = append(base, "GITHUB_OUTPUT="+output, "SKILLS_ROOT=port/cxc/skills",
		"PR_BASE_SHA=", "PR_HEAD_SHA=", "PUSH_BEFORE_SHA=", "GITHUB_SHA=")
	got := runEnv(t, r.root, append(base, env...), "bash", script)
	if got.code != 0 {
		t.Fatalf("the changed-path step exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "changed=") {
			return line
		}
	}
	return ""
}

// skillPathsRepo is a repository whose dev branch moved a staged skill after the branch point: the
// first commit is the branch point, dev's tip carries the staged-skill change, and the feature
// branch starts from the branch point and then runs feature.
func skillPathsRepo(t *testing.T, feature func(*fixtureRepo)) (*fixtureRepo, string, string) {
	t.Helper()
	r := newRepo(t)
	r.write("README.md", "base\n")
	r.write("port/cxc/skills/crw-qa/SKILL.md", "a\n")
	r.commit()
	r.git("branch", "dev")
	r.git("checkout", "-q", "dev")
	r.write("port/cxc/skills/crw-qa/SKILL.md", "b\n")
	r.commit()
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.git("checkout", "-q", "-b", "feature", "dev~1")
	feature(r)
	r.commit()
	head := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	return r, base, head
}

// A pull request is compared from its merge base: the commits the branch adds to its base, not the
// base tip's own changes. dev moving the staged skills after the branch point must not select the
// job, and a branch carrying the same staged-skill change dev has must still select it (CRW-939).
func TestWorkflow_the_skill_paths_compare_a_pull_request_from_its_merge_base(t *testing.T) {
	// dev changed a staged skill; this branch changed nothing below the staged skills root.
	r, base, head := skillPathsRepo(t, func(r *fixtureRepo) {
		r.write("internal/relay/store/store.go", "changed\n")
	})
	if got := skillPathsRun(t, r, "PR_BASE_SHA="+base, "PR_HEAD_SHA="+head); got != "changed=false" {
		t.Errorf("a branch with no staged-skill change answers %q, want changed=false: dev's own change is not the pull request's", got)
	}
	// The branch carries the same staged-skill change dev has. Comparing the two tips directly sees
	// no difference, but the pull request still changed the skill relative to its merge base.
	r2, base2, head2 := skillPathsRepo(t, func(r *fixtureRepo) {
		r.write("port/cxc/skills/crw-qa/SKILL.md", "b\n")
		// An unrelated change beside it: two commits with the same tree, parent and message are one
		// object, and this branch's commit must be its own so the comparison is the question.
		r.write("docs/CI.md", "branch\n")
	})
	if got := skillPathsRun(t, r2, "PR_BASE_SHA="+base2, "PR_HEAD_SHA="+head2); got != "changed=true" {
		t.Errorf("a branch carrying the same staged-skill change as dev answers %q, want changed=true", got)
	}
}

// A push to dev compares the commit it replaced with the one it added, where the range is already
// the pushed commits; a manual dispatch has no base to compare with, so it runs the tests.
func TestWorkflow_the_skill_paths_keep_the_push_range_and_run_on_a_dispatch(t *testing.T) {
	r := newRepo(t)
	r.write("README.md", "base\n")
	r.commit()
	before := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.write("port/cxc/skills/crw-qa/SKILL.md", "a\n")
	r.commit()
	staged := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	if got := skillPathsRun(t, r, "PUSH_BEFORE_SHA="+before, "GITHUB_SHA="+staged); got != "changed=true" {
		t.Errorf("a push touching a staged skill answers %q, want changed=true", got)
	}
	r.write("internal/relay/store/store.go", "x\n")
	r.commit()
	next := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	if got := skillPathsRun(t, r, "PUSH_BEFORE_SHA="+staged, "GITHUB_SHA="+next); got != "changed=false" {
		t.Errorf("a push touching nothing staged answers %q, want changed=false", got)
	}
	for _, env := range [][]string{nil, {"PUSH_BEFORE_SHA=0000000000000000000000000000000000000000"}} {
		if got := skillPathsRun(t, r, env...); got != "changed=true" {
			t.Errorf("a manual dispatch (%v) answers %q, want changed=true", env, got)
		}
	}
}
