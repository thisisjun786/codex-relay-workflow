//go:build dev

package ci

import (
	"fmt"
	"strconv"
	"strings"
)

// CRW-964: the local step table. `crw-dev ci local` runs every job and step of
// .github/workflows/ci.yml locally, in the same order and at the same pinned versions, and writes a
// verification-record/1. This file owns the table and the reader that holds it to the workflow, so a
// step ci.yml grows cannot go unverified.

// The kinds of a table entry.
const (
	// localRun runs the step's command in the clean worktree.
	localRun = "run"
	// localAction performs the step with Go code (a checkout, a toolchain, a build, the gate).
	localAction = "action"
	// localNotApplicable is a step this run never performs, with the reason in note.
	localNotApplicable = "not_applicable"
)

// The actions a localAction step names.
const (
	localCheckout    = "checkout"      // the clean worktree of the verified commit
	localGo          = "go"            // resolve and record the pinned Go
	localNode        = "node"          // resolve and record the pinned Node
	localBuildCrwDev = "build-crw-dev" // build the development binary into RUNNER_TEMP
	localAggregate   = "aggregate"     // dev-gate: every prerequisite succeeded
)

// localStep is one ci.yml step, as this run performs it. command and uses are the ci.yml step's own
// run text and pinned action, so the table can be compared with the workflow step by step.
type localStep struct {
	name     string   // the ci.yml step name, "" when the step is unnamed
	kind     string   // localRun, localAction or localNotApplicable
	action   string   // the Go action a localAction step performs
	command  string   // a run step's shell command, the ci.yml run text itself
	uses     string   // a uses step's pinned action, the ci.yml uses itself
	workdir  string   // the directory the command runs in, relative to the checkout root
	scope    string   // "range" for the steps that judge the commits a change adds, "full" otherwise
	tool     string   // the tool the step needs on PATH, "" when none
	env      []string // extra environment, as NAME=value
	legs     []string // the go-product matrix parts; empty means every leg
	heavy    bool     // runs through the heavy-check gate when one is configured
	ciDigest string   // the digest of the ci.yml step this step implements (local_workflow.go)
	note     string   // why a not_applicable step is not run, or what a decision step decided
}

// localJob is one ci.yml job. legs is the go-product matrix; nil elsewhere.
type localJob struct {
	name  string
	legs  []string
	steps []localStep
}

// localGoProductLegs is the go-product matrix, in ci.yml order.
func localGoProductLegs() []string {
	return []string{"lint", "test-1", "test-2", "test-3", "test-4", "test-rest", "dist"}
}

// localGoProductTestLegs is the go-product test legs, the ones `make test-part` runs.
func localGoProductTestLegs() []string {
	return []string{"test-1", "test-2", "test-3", "test-4", "test-rest"}
}

// The placeholders the runner replaces when it builds a step's environment: the range base and head
// the record names, the file the changed-path decision writes, and the matrix part.
const (
	localBaseEnv      = "@BASE@"
	localHeadEnv      = "@HEAD@"
	localGuiOutputEnv = "@GUI_OUTPUT@"
	localPartEnv      = "@PART@"
)

// The ci.yml block-scalar bodies the table runs, copied verbatim. A test holds each to the workflow.
const (
	localSkillDecision = `set -euo pipefail
# A pull request compares its base with its head, a push to dev the previous commit with
# this one. A manual dispatch has neither, so it runs the tests.
if [[ -n "${PR_BASE_SHA}" ]]; then
  base="${PR_BASE_SHA}"; head="${PR_HEAD_SHA}"
elif [[ -n "${PUSH_BEFORE_SHA}" && "${PUSH_BEFORE_SHA}" != "0000000000000000000000000000000000000000" ]]; then
  base="${PUSH_BEFORE_SHA}"; head="${GITHUB_SHA}"
else
  echo "changed=true" >> "$GITHUB_OUTPUT"
  echo 'no base to compare with; the staged skill-script tests run'
  exit 0
fi
changed="$(git diff --name-only "${base}" "${head}" -- "${SKILLS_ROOT}")"
if [[ -n "${changed}" ]]; then
  echo "changed=true" >> "$GITHUB_OUTPUT"
  printf 'staged skill paths changed:\n%s\n' "${changed}"
else
  echo "changed=false" >> "$GITHUB_OUTPUT"
  echo 'no staged skill path changed; the job ends without installing Node'
fi`
	localSkillTests = `set -euo pipefail
shopt -s nullglob
files=("${SKILLS_ROOT}"/*/tests/*.test.mjs)
if [[ ${#files[@]} -eq 0 ]]; then
  echo 'no staged skill-script test file'
  exit 0
fi
node --test "${files[@]}"`
	localDistBuilds = `for target in linux/amd64 linux/arm64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" make dist BINARY="dist/crw_${os}_${arch}/crw"
done
(cd dist && sha256sum crw_*/crw > SHA256SUMS && cat SHA256SUMS)`
	localGateScript = `set -euo pipefail
if [[ "$EVENT_NAME" == pull_request && "$BASE_REF" == main ]]; then
  echo '::error::PRs must target a development branch; main is a release mirror.'
  exit 1
fi
results="$RUNNER_TEMP/needs.json"
cat > "$results" <<'NEEDS'
${{ toJSON(needs) }}
NEEDS
jq -r 'to_entries[] | "\(.key): \(.value.result)"' "$results" || true
if ! jq -e 'type == "object" and length > 0 and all(.[]; type == "object" and .result == "success")' "$results" > /dev/null; then
  echo '::error::Every prerequisite job must succeed.'
  exit 1
fi
echo 'Every prerequisite job succeeded.'`
	localCorpusCommand = `make test-part TEST_PART="${PART#test-}"`
)

// localPlan is the table: every ci.yml job, every step of it, in workflow order. A job's steps are
// positional with the workflow's, so the reader compares them one by one.
func localPlan() []localJob {
	// The pair every job carries: the sparse checkout that fetches scripts/ci before the mirror can
	// decide, and the mirror itself. Both exist only for a body-only edit of a hosted run, so neither
	// is performed locally (CRW-790, the mirror).
	mirrorPair := func() []localStep {
		return []localStep{
			{name: "Check out scripts/ci for the body-only edit mirror", kind: localNotApplicable,
				note: "a sparse checkout that exists only so the hosted body-only edit mirror can decide; the local run checks out the whole commit"},
			{name: "Mirror the jobs this head already ran", kind: localNotApplicable,
				command: "bash scripts/ci/edit_mirror.sh",
				note:    "the body-only edit mirror reads this repository's hosted runs; a local run has none to mirror and always runs every step"},
		}
	}
	checkout := localStep{kind: localAction, action: localCheckout, scope: "full"}
	goToolchain := localStep{kind: localAction, action: localGo, tool: "go", scope: "full"}
	nodeToolchain := localStep{kind: localAction, action: localNode, tool: "node", scope: "full"}
	var plan []localJob
	validate := localJob{name: "validate", legs: nil}
	validate.steps = append(validate.steps, mirrorPair()...)
	validate.steps = append(validate.steps, checkout)
	validate.steps = append(validate.steps, goToolchain)
	validate.steps = append(validate.steps, localStep{name: "", kind: localRun, command: "go build -tags dev -o \"$RUNNER_TEMP/crw-dev\" ./cmd/crw-dev",
		tool: "go", scope: "full", heavy: true, legs: nil, env: nil})
	validate.steps = append(validate.steps, localStep{name: "", kind: localRun, command: "\"$RUNNER_TEMP/crw-dev\" ci validate",
		tool: "go", scope: "range", heavy: true, legs: nil, env: []string{"BLOB_RANGE_BASE=" + localBaseEnv}})
	validate.steps = append(validate.steps, localStep{name: "", kind: localRun, command: "\"$RUNNER_TEMP/crw-dev\" ci plugin",
		tool: "go", scope: "full", heavy: true, legs: nil, env: nil})
	validate.steps = append(validate.steps, localStep{name: "", kind: localRun, command: "\"$RUNNER_TEMP/crw-dev\" ci contracts",
		tool: "go", scope: "full", heavy: true, legs: nil, env: nil})
	plan = append(plan, validate)
	secrets := localJob{name: "secrets", legs: nil}
	secrets.steps = append(secrets.steps, mirrorPair()...)
	secrets.steps = append(secrets.steps, checkout)
	secrets.steps = append(secrets.steps, localStep{name: "", kind: localRun, command: "bash scripts/ci/secrets.sh",
		tool: "", scope: "range", heavy: true, legs: nil, env: []string{"GITHUB_EVENT_NAME=pull_request", "PR_BASE_SHA=" + localBaseEnv}})
	plan = append(plan, secrets)
	skill_scripts_node := localJob{name: "skill-scripts-node", legs: nil}
	skill_scripts_node.steps = append(skill_scripts_node.steps, mirrorPair()...)
	skill_scripts_node.steps = append(skill_scripts_node.steps, checkout)
	skill_scripts_node.steps = append(skill_scripts_node.steps, localStep{name: "Decide from the changed files whether the staged skills changed", kind: localRun, command: "set -euo pipefail\n# A pull request compares from its merge base: the commits the branch adds to its base,\n# never the base tip's own changes. A push to dev compares the previous commit with this\n# one, where the range is already the pushed commits. A manual dispatch has neither, so\n# it runs the tests.\nif [[ -n \"${PR_BASE_SHA}\" ]]; then\n  range=\"${PR_BASE_SHA}...${PR_HEAD_SHA}\"\nelif [[ -n \"${PUSH_BEFORE_SHA}\" && \"${PUSH_BEFORE_SHA}\" != \"0000000000000000000000000000000000000000\" ]]; then\n  range=\"${PUSH_BEFORE_SHA}..${GITHUB_SHA}\"\nelse\n  echo \"changed=true\" >> \"$GITHUB_OUTPUT\"\n  echo 'no base to compare with; the staged skill-script tests run'\n  exit 0\nfi\nchanged=\"$(git diff --name-only \"${range}\" -- \"${SKILLS_ROOT}\")\"\nif [[ -n \"${changed}\" ]]; then\n  echo \"changed=true\" >> \"$GITHUB_OUTPUT\"\n  printf 'staged skill paths changed:\\n%s\\n' \"${changed}\"\nelse\n  echo \"changed=false\" >> \"$GITHUB_OUTPUT\"\n  echo 'no staged skill path changed; the job ends without installing Node'\nfi",
		tool: "", scope: "range", heavy: false, legs: nil, env: []string{"PR_BASE_SHA=" + localBaseEnv, "PR_HEAD_SHA=" + localHeadEnv, "GITHUB_OUTPUT=" + localGuiOutputEnv, "SKILLS_ROOT=plugins/crw/skills"},
		note: "the same changed-path decision ci.yml runs, recorded; the local run performs every step, so the answer skips nothing"})
	skill_scripts_node.steps = append(skill_scripts_node.steps, nodeToolchain)
	skill_scripts_node.steps = append(skill_scripts_node.steps, localStep{name: "Run the staged skill-script tests", kind: localRun, command: localSkillTests,
		tool: "node", scope: "full", heavy: true, legs: nil, env: []string{"SKILLS_ROOT=plugins/crw/skills"}})
	plan = append(plan, skill_scripts_node)
	gui := localJob{name: "gui", legs: nil}
	gui.steps = append(gui.steps, mirrorPair()...)
	gui.steps = append(gui.steps, checkout)
	gui.steps = append(gui.steps, localStep{name: "Decide from the changed files whether the screens changed", kind: localRun, command: "bash scripts/ci/gui_paths.sh",
		tool: "", scope: "range", heavy: false, legs: nil, env: []string{"PR_BASE_SHA=" + localBaseEnv, "PR_HEAD_SHA=" + localHeadEnv, "GITHUB_OUTPUT=" + localGuiOutputEnv},
		note: "the same changed-path decision ci.yml runs, recorded; the local run performs every step, so the answer skips nothing"})
	gui.steps = append(gui.steps, goToolchain)
	gui.steps = append(gui.steps, nodeToolchain)
	gui.steps = append(gui.steps, localStep{name: "Install the screen dependencies from the committed lockfile", kind: localRun, command: "npm ci", workdir: "web",
		tool: "node", scope: "full", heavy: true, legs: nil, env: nil})
	gui.steps = append(gui.steps, localStep{name: "Run the screen tests", kind: localRun, command: "npm test", workdir: "web",
		tool: "node", scope: "full", heavy: true, legs: nil, env: nil})
	gui.steps = append(gui.steps, localStep{name: "Build the screens into a fresh tree", kind: localRun, command: "npm run build -- --outDir \"$RUNNER_TEMP/gui-built\" --emptyOutDir", workdir: "web",
		tool: "node", scope: "full", heavy: true, legs: nil, env: nil})
	gui.steps = append(gui.steps, localStep{name: "Refuse a committed tree that is not a fresh build", kind: localRun, command: "go run -tags dev ./cmd/crw-dev ci gui-drift --built \"$RUNNER_TEMP/gui-built\"",
		tool: "go", scope: "full", heavy: true, legs: nil, env: nil})
	plan = append(plan, gui)
	go_product := localJob{name: "go-product", legs: localGoProductLegs()}
	go_product.steps = append(go_product.steps, localStep{name: "Light mode notice", kind: localNotApplicable,
		command: "printf '%s\\n' \"light mode: this leg's tests run in full when the merge lane labels the pull request crw-lane\" >> \"$GITHUB_STEP_SUMMARY\"",
		note:    "light mode is never applied: the local run always runs every test leg in full (CRW-790)"})
	go_product.steps = append(go_product.steps, mirrorPair()...)
	go_product.steps = append(go_product.steps, checkout)
	go_product.steps = append(go_product.steps, goToolchain)
	go_product.steps = append(go_product.steps, localStep{name: "Lint", kind: localRun, command: "make lint",
		tool: "go", scope: "full", heavy: true, legs: []string{"lint"}, env: nil})
	go_product.steps = append(go_product.steps, localStep{name: "Test and replay the contract corpus (${{ matrix.part }})", kind: localRun, command: localCorpusCommand,
		tool: "go", scope: "full", heavy: true, legs: localGoProductTestLegs(), env: []string{"PART=" + localPartEnv}})
	go_product.steps = append(go_product.steps, localStep{name: "Build the static binaries for every published target", kind: localRun, command: localDistBuilds,
		tool: "go", scope: "full", heavy: true, legs: []string{"dist"}, env: nil})
	go_product.steps = append(go_product.steps, localStep{name: "Install and wire the release binary in an isolated home", kind: localRun, command: "CRW_TEST_BINARY=\"$PWD/dist/crw_linux_amd64/crw\" go test -trimpath -tags integration -count=1 ./internal/runtime/integration/...",
		tool: "go", scope: "full", heavy: true, legs: []string{"dist"}, env: []string{"CGO_ENABLED=0"},
		note: "CGO_ENABLED=0 and -trimpath are the dist build's, so the test reuses its compiled packages and only relinks"})
	go_product.steps = append(go_product.steps, localStep{kind: localNotApplicable, legs: []string{"dist"},
		uses: "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		note: "upload-artifact publishes the built artifact to the hosted run; a local run leaves it in dist/"})
	go_product.steps = append(go_product.steps, localStep{kind: localNotApplicable, legs: []string{"dist"},
		uses: "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		note: "upload-artifact publishes the built artifact to the hosted run; a local run leaves it in dist/"})
	go_product.steps = append(go_product.steps, localStep{kind: localNotApplicable, legs: []string{"dist"},
		uses: "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		note: "upload-artifact publishes the built artifact to the hosted run; a local run leaves it in dist/"})
	go_product.steps = append(go_product.steps, localStep{kind: localNotApplicable, legs: []string{"dist"},
		uses: "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		note: "upload-artifact publishes the built artifact to the hosted run; a local run leaves it in dist/"})
	plan = append(plan, go_product)
	dev_gate := localJob{name: "dev-gate", legs: nil}
	dev_gate.steps = append(dev_gate.steps, localStep{name: "Require every prerequisite to succeed", kind: localAction, action: localAggregate,
		scope: "full", command: localGateScript, tool: "go",
		note: "every prerequisite job must have succeeded, as the gate's own script requires"})
	plan = append(plan, dev_gate)
	applyStepDigests(plan)
	return plan
}

// localYAMLScalar is a run value as the workflow's YAML reader hands it to the runner: a quoted
// scalar loses its quotes (single-quoted text keeps everything else, with ” for one quote), and
// a plain scalar is itself. The table carries the shell text, so the comparison is like for like.
func localYAMLScalar(value string) string {
	if len(value) >= 2 && strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	if len(value) >= 2 && strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return value
}

// workflowJobNamed is the parsed job called name, or nil.
func workflowJobNamed(jobs []workflowJob, name string) *workflowJob {
	for i := range jobs {
		if jobs[i].name == name {
			return &jobs[i]
		}
	}
	return nil
}

// checkWorkflowAt reads ci.yml as the named commit has it and refuses a workflow the table does not
// cover. Reading it from the verified commit rather than the working tree is what keeps a dirty
// edit to ci.yml from changing what this run is held to.
func checkWorkflowAt(root, commit string, plan []localJob) error {
	data, err := runGit(root, "show", commit+":.github/workflows/ci.yml")
	if err != nil {
		return err
	}
	workflow, err := parseWorkflow(string(data))
	if err != nil {
		return err
	}
	if problems := localPlanProblems(plan, workflow); len(problems) > 0 {
		return fmt.Errorf("the local plan does not cover .github/workflows/ci.yml:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}
