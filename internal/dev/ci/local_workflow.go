//go:build dev

package ci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CRW-964: the reader of .github/workflows/ci.yml that the local plan is held to. It reads each job's
// matrix, condition and env, and each step's name, uses, with, env, working-directory, if and run
// text, so a plan step can carry the digest of the ci.yml step it implements (parent ruling 2). A
// change to any of those is plan drift, and the run is refused rather than run partially.

// workflowStep is one step of a parsed ci.yml.
type workflowStep struct {
	name    string
	uses    string
	run     string
	workdir string
	ifExpr  string
	// nodeVersion is the with: node-version a setup-node step names, "" otherwise.
	nodeVersion string
	env         map[string]string
	with        map[string]string
}

// workflowJob is one job of a parsed ci.yml. matrix holds the strategy.matrix entries as written.
type workflowJob struct {
	name   string
	ifExpr string
	env    map[string]string
	matrix map[string]string
	steps  []workflowStep
}

var (
	localWorkflowJobHeader = regexp.MustCompile(`^  ([a-z][a-z0-9-]*):$`)
	localWorkflowJobKey    = regexp.MustCompile(`^    ([a-z][a-z-]*):(?: (.*))?$`)
	localWorkflowEntry6    = regexp.MustCompile(`^      ([A-Za-z_][A-Za-z0-9_.-]*): (.*)$`)
	localWorkflowEntry8    = regexp.MustCompile(`^        ([A-Za-z_][A-Za-z0-9_.-]*): (.*)$`)
	localWorkflowStepKey   = regexp.MustCompile(`^        ([a-z][a-z-]*):(?: (.*))?$`)
	localWorkflowEntry10   = regexp.MustCompile(`^          ([A-Za-z_][A-Za-z0-9_.-]*):(?: (.*))?$`)
)

// localWorkflowTopKey is a top-level key of ci.yml. The reader reads only the keys the plan can compare: a key
// it does not read could change how a step runs, so it is refused rather than ignored (pre-merge finding d2).
var localWorkflowTopKey = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):`)

// localWorkflowTopKeys, localWorkflowJobKeys are the keys the reader accepts at each level. concurrency, runs-on,
// timeout-minutes and permissions do not change which commands a step runs.
var localWorkflowTopKeys = map[string]bool{"name": true, "on": true, "permissions": true, "concurrency": true, "jobs": true}
var localWorkflowJobKeys = map[string]bool{"runs-on": true, "timeout-minutes": true, "permissions": true, "env": true, "strategy": true, "steps": true, "if": true, "needs": true}

// parseWorkflow reads the jobs of .github/workflows/ci.yml with their steps. It is deliberately not a
// YAML parser: the file is this repository's own, and the reader only has to see the keys the plan
// compares. A block it does not read is refused by the plan, never skipped.
func parseWorkflow(text string) ([]workflowJob, error) {
	raw := lines(strings.TrimSuffix(text, "\n"))
	var jobs []workflowJob
	inside := false
	// section is the job's block at four spaces (steps, env, strategy, ...); block is the step's open
	// env or with block.
	section, block := "", ""
	for i := 0; i < len(raw); i++ {
		line := raw[i]
		// A blank or comment-only line carries no key: it neither ends a block nor a section (a comment inside a step's with
		// block must not drop the node-version after it, CRW-1191).
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			if m := localWorkflowTopKey.FindStringSubmatch(line); m != nil && !localWorkflowTopKeys[m[1]] {
				return nil, fmt.Errorf("ci.yml sets %s at the top level, which the local plan does not read", m[1])
			}
			inside = strings.HasPrefix(line, "jobs:")
			continue
		}
		if !inside {
			continue
		}
		if m := localWorkflowJobHeader.FindStringSubmatch(line); m != nil {
			jobs = append(jobs, workflowJob{name: m[1], env: map[string]string{}, matrix: map[string]string{}})
			section, block = "", ""
			continue
		}
		if len(jobs) == 0 {
			continue
		}
		job := &jobs[len(jobs)-1]
		if m := localWorkflowJobKey.FindStringSubmatch(line); m != nil {
			if !localWorkflowJobKeys[m[1]] {
				return nil, fmt.Errorf("ci.yml job %s sets %s, which the local plan does not read", job.name, m[1])
			}
			section, block = m[1], ""
			if m[1] == "if" {
				job.ifExpr = localYAMLScalar(m[2])
			}
			continue
		}
		switch section {
		case "env":
			if m := localWorkflowEntry6.FindStringSubmatch(line); m != nil {
				job.env[m[1]] = localYAMLScalar(m[2])
			}
			continue
		case "strategy":
			if m := localWorkflowEntry8.FindStringSubmatch(line); m != nil {
				job.matrix[m[1]] = localYAMLScalar(m[2])
			}
			continue
		case "steps":
		default:
			continue
		}
		if strings.HasPrefix(line, "      - ") {
			job.steps = append(job.steps, workflowStep{env: map[string]string{}, with: map[string]string{}})
			block = ""
			line = "        " + line[8:]
		}
		if len(job.steps) == 0 {
			continue
		}
		step := &job.steps[len(job.steps)-1]
		if block != "" {
			if m := localWorkflowEntry10.FindStringSubmatch(line); m != nil {
				value := localYAMLScalar(m[2])
				if block == "env" {
					step.env[m[1]] = value
				} else {
					step.with[m[1]] = value
					if m[1] == "node-version" {
						step.nodeVersion = value
					}
				}
				continue
			}
		}
		m := localWorkflowStepKey.FindStringSubmatch(line)
		if m == nil {
			block = ""
			continue
		}
		block = ""
		switch m[1] {
		case "name":
			step.name = strings.Trim(m[2], `\"'`)
		case "uses":
			if fields := strings.Fields(m[2]); len(fields) > 0 {
				step.uses = fields[0]
			}
		case "id":
			// An id names the step for the workflow's own outputs; it does not change the work.
		case "if":
			step.ifExpr = localYAMLScalar(m[2])
		case "working-directory":
			step.workdir = localYAMLScalar(m[2])
		case "env", "with":
			if m[2] == "" {
				block = m[1]
			}
		case "run":
			if m[2] != "|" && m[2] != "|-" && m[2] != ">" && m[2] != ">-" {
				step.run = localYAMLScalar(m[2])
				break
			}
			// A literal block scalar: its body is the run text. The body sits deeper than the key
			// that opened it, so the first body line fixes the indent and a shallower line ends the
			// block (the step's own keys and the next step sit shallower still).
			var body []string
			indent := -1
			for j := i + 1; j < len(raw); j++ {
				rest := raw[j]
				if strings.TrimSpace(rest) == "" {
					body = append(body, "")
					continue
				}
				width := len(rest) - len(strings.TrimLeft(rest, " "))
				if indent < 0 {
					indent = width
				}
				if width < indent {
					break
				}
				body = append(body, rest[indent:])
				i = j
			}
			for len(body) > 0 && body[len(body)-1] == "" {
				body = body[:len(body)-1]
			}
			step.run = strings.Join(body, "\n")
		default:
			return nil, fmt.Errorf("ci.yml sets %s on a step, which the local plan does not read", m[1])
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("ci.yml has no jobs")
	}
	return jobs, nil
}

// workflowStepDigest is the sha256 of one ci.yml step as the plan implements it: its job's name,
// condition and matrix, its position, and the step's name, uses, with, run, working-directory, if
// and env (the job's env merged with the step's). The fields are serialized as one JSON document, so
// a value that contains a delimiter cannot blur two steps together.
func workflowStepDigest(job workflowJob, n int) string {
	step := job.steps[n]
	env := map[string]string{}
	for k, v := range job.env {
		env[k] = v
	}
	for k, v := range step.env {
		env[k] = v
	}
	data, _ := json.Marshal(struct {
		Job     string            `json:"job"`
		JobIf   string            `json:"jobIf"`
		Matrix  map[string]string `json:"matrix"`
		Index   int               `json:"index"`
		Name    string            `json:"name"`
		Uses    string            `json:"uses"`
		With    map[string]string `json:"with"`
		Run     string            `json:"run"`
		WorkDir string            `json:"workingDirectory"`
		If      string            `json:"if"`
		Env     map[string]string `json:"env"`
	}{job.name, job.ifExpr, job.matrix, n, step.name, step.uses, step.with, step.run, step.workdir, step.ifExpr, env})
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// localPlanProblems is what would let a ci.yml step run unverified: a job out of order, a job or a
// step the table does not have, a step that moved, or a step whose ci.yml digest is not the one the
// plan carries. It is the C1 check, and the runner refuses the workflow when it is not empty.
func localPlanProblems(plan []localJob, workflow []workflowJob) []string {
	var problems []string
	for i := range workflow {
		job := &workflow[i]
		if i >= len(plan) {
			problems = append(problems, fmt.Sprintf("ci.yml job %q has no entry in the local plan", job.name))
			continue
		}
		entry := &plan[i]
		if entry.name != job.name {
			problems = append(problems, fmt.Sprintf("job %d is %q in the plan and %q in ci.yml: the job order differs", i, entry.name, job.name))
			continue
		}
		if len(entry.steps) != len(job.steps) {
			problems = append(problems, fmt.Sprintf("%s: the plan has %d steps, ci.yml has %d", job.name, len(entry.steps), len(job.steps)))
		}
		for n := 0; n < len(entry.steps) && n < len(job.steps); n++ {
			want, got := job.steps[n].name, entry.steps[n].name
			if want != got {
				problems = append(problems, fmt.Sprintf("%s step %d: the plan names %q, ci.yml names %q", job.name, n, got, want))
			}
			// A step kept under its name but changed underneath would run other work locally, so its run
			// text and its action are compared too.
			if entry.steps[n].kind == localRun && strings.TrimSpace(entry.steps[n].command) != strings.TrimSpace(job.steps[n].run) {
				problems = append(problems, fmt.Sprintf("%s step %d (%s): the plan's command is not the ci.yml run text", job.name, n, want))
			}
			if entry.steps[n].uses != "" && entry.steps[n].uses != job.steps[n].uses {
				problems = append(problems, fmt.Sprintf("%s step %d (%s): the plan's action is not the ci.yml uses", job.name, n, want))
			}
			if digest := workflowStepDigest(*job, n); entry.steps[n].ciDigest != digest {
				problems = append(problems, fmt.Sprintf("%s step %d (%s): the ci.yml step digest is %s, the plan carries %q", job.name, n, want, digest, entry.steps[n].ciDigest))
			}
		}
	}
	for i := len(workflow); i < len(plan); i++ {
		problems = append(problems, fmt.Sprintf("the plan names job %q, which ci.yml does not have", plan[i].name))
	}
	return problems
}
