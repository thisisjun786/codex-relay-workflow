package mergeturn

// The bundle's ci.yml job set (CRW-897, answer 6). The pin test that holds TrainExpectedJobs to
// this repository's .github/workflows/ci.yml reads the workflow as text, and verify reads the same
// text out of the head it is verifying, so a bundle whose members change the job set is refused
// with the names it lost and added rather than against the runtime's own list alone. This
// repository has no YAML dependency, so the reading is the text scan the pin test used.

import (
	"context"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// trainProductJob is the workflow's matrix job, whose leg names a run reports as
// "go-product (lint)" and the like.
const trainProductJob = "go-product"

// TrainJobsFromWorkflow is every job name a workflow declares, with the go-product matrix expanded
// into the leg names a run reports. A workflow it cannot read is an error, never an empty pass.
func TrainJobsFromWorkflow(workflow string) ([]string, error) {
	names, err := trainWorkflowJobNames(workflow)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != trainProductJob {
			out = append(out, name)
			continue
		}
		parts, err := trainWorkflowMatrixParts(workflow)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			out = append(out, trainProductJob+" ("+part+")")
		}
	}
	return out, nil
}

// trainWorkflowJobNames reads the two-space-indented keys under the workflow's jobs block.
func trainWorkflowJobNames(workflow string) ([]string, error) {
	_, after, found := strings.Cut(workflow, "\njobs:\n")
	if !found {
		return nil, errors.New("the workflow holds no jobs block")
	}
	var names []string
	for _, line := range strings.Split(after, "\n") {
		line = trainStripComment(line)
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":") {
			if name := strings.TrimSuffix(strings.TrimPrefix(line, "  "), ":"); name != "" {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// trainStripComment removes a trailing YAML comment (a '#' at the start of the line or preceded by
// whitespace, outside a quoted scalar) and the whitespace before it. A job header such as
// "  audit: # added gate" is a real job, and a scanner that misses it would let a head add a job
// without verify naming it; a '#' inside a quoted key is part of the key and stays.
func trainStripComment(line string) string {
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return line
}

// trainWorkflowMatrixParts reads the go-product job's strategy.matrix.part list as text. The list is
// looked for inside that job's own block, so a workflow that declares no job after it still reads.
func trainWorkflowMatrixParts(workflow string) ([]string, error) {
	body, found := trainJobBody(workflow, trainProductJob)
	if !found {
		return nil, errors.New("the workflow holds no " + trainProductJob + " job")
	}
	_, matrix, found := strings.Cut(body, "\n        part: [")
	if !found {
		return nil, errors.New("the workflow's " + trainProductJob + " job has no matrix part list")
	}
	list, _, found := strings.Cut(matrix, "]")
	if !found {
		return nil, errors.New("the workflow's matrix part list is unterminated")
	}
	parts := strings.Split(list, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, nil
}

// trainJobBody is the text of one job's block: from its header line to the next job's header, or to
// the end of the workflow. The header is matched with a trailing YAML comment stripped, so a
// workflow that comments its job keys still reads.
func trainJobBody(workflow, job string) (string, bool) {
	lines := strings.SplitAfter(workflow, "\n")
	start := -1
	offset := 0
	for i, line := range lines {
		bare := trainStripComment(strings.TrimSuffix(line, "\n"))
		if bare == "  "+job+":" {
			start = offset + len(line)
			lines = lines[i+1:]
			break
		}
		offset += len(line)
	}
	if start < 0 {
		return "", false
	}
	end := 0
	for _, line := range lines {
		bare := trainStripComment(strings.TrimSuffix(line, "\n"))
		if strings.HasPrefix(bare, "  ") && !strings.HasPrefix(bare, "   ") && strings.HasSuffix(bare, ":") {
			break
		}
		end += len(line)
	}
	return workflow[start : start+end], true
}

// trainWorkflowRefusal is answer 6's gate: the job set the head's .github/workflows/ci.yml declares
// must be the set this runtime verifies. A workflow that cannot be read or parsed is
// merge_target_unreadable, never a pass; a job set that differs is disposition_conflict naming the
// names the head lost and the names it added.
func trainWorkflowRefusal(ctx context.Context, proof TrainCheckout, checkout, head string) error {
	workflow, err := proof.File(ctx, checkout, head, TrainBundleWorkflow)
	if err != nil {
		return trainUnreadable("the job set of %s at %s was not read: %v", TrainBundleWorkflow, pyvalue.StrRepr(head), err)
	}
	jobs, err := TrainJobsFromWorkflow(workflow)
	if err != nil {
		return trainUnreadable("the job set of %s at %s was not read: %v", TrainBundleWorkflow, pyvalue.StrRepr(head), err)
	}
	have := map[string]bool{}
	for _, job := range jobs {
		have[job] = true
	}
	want := map[string]bool{}
	for _, job := range TrainExpectedJobs {
		want[job] = true
	}
	var missing, added []string
	for _, job := range TrainExpectedJobs {
		if !have[job] {
			missing = append(missing, job)
		}
	}
	for _, job := range jobs {
		if !want[job] {
			added = append(added, job)
		}
	}
	if len(missing) == 0 && len(added) == 0 {
		return nil
	}
	return trainConflict("the head's %s declares a job set this runtime does not verify: missing %s, added %s", TrainBundleWorkflow, trainNameList(missing), trainNameList(added))
}

// trainNameList renders job names for a refusal, "none" when there are none.
func trainNameList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, pyvalue.StrRepr(name))
	}
	return strings.Join(quoted, ", ")
}
