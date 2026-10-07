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

// trainWorkflowJobNames reads the two-space-indented keys under the workflow's jobs block. Inside
// that block a two-space-indented key is a job whatever its value looks like: a block that follows on
// the next lines, a flow mapping on the same line ("  audit: {runs-on: ubuntu, steps: [...]}"), a
// whole-job anchor ("  audit: &base_job") or an alias ("  audit-copy: *base_job"). Missing any of
// them would let a head add a job this runtime never checks. A jobs block this reader cannot read key
// by key — a value on the jobs: line itself, or a job-level line whose key cannot be read — is an
// error, never an empty pass.
func trainWorkflowJobNames(workflow string) ([]string, error) {
	body, inline, found := trainJobsBlock(workflow)
	if !found {
		return nil, errors.New("the workflow holds no jobs block")
	}
	if inline {
		return nil, errors.New("the workflow's jobs block carries its value on the jobs: line, which this reader cannot read key by key")
	}
	var names []string
	for _, line := range strings.Split(body, "\n") {
		line = trainStripComment(line)
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		// a blank or comment-only line is nothing to read; a line this reader cannot read the key of
		// is an error, never a job silently skipped.
		if line == "" {
			continue
		}
		// the two-space gate is what keeps a nested key ("    runs-on: ubuntu") from being read as a
		// job: trainJobKey reads a key, not an indentation level. A job's own block scalar cannot sit
		// at exactly this indent, so every other two-space line here is a job key.
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		name, ok := trainJobKey(strings.TrimPrefix(line, "  "))
		if !ok {
			return nil, errors.New("the workflow's jobs block holds a line whose key this reader cannot read")
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("the workflow's jobs block holds no job key")
	}
	return names, nil
}

// trainJobsBlock splits the workflow at its top-level jobs: line and answers the text that follows the
// key's own line, plus whether the key carried its value on that same line (a flow mapping or any
// other same-line value, which this reader cannot read key by key).
func trainJobsBlock(workflow string) (body string, inline bool, found bool) {
	_, after, found := strings.Cut(workflow, "\njobs:")
	if !found {
		return "", false, false
	}
	head, rest, hasLine := strings.Cut(after, "\n")
	if value := strings.TrimSpace(head); value != "" {
		return value, true, true
	}
	if !hasLine {
		return "", false, true
	}
	return rest, false, true
}

// trainJobKey reads one line's key. Within a jobs block a two-space-indented key is a job whatever
// its value looks like — a block on the following lines, a flow mapping on the same line
// ("audit: {runs-on: ubuntu, steps: [...]}"), a whole-job YAML anchor ("audit: &base_job") or an
// alias ("audit-copy: *base_job"), all of which GitHub Actions accepts — so the value is deliberately
// not inspected. The key names the job; a reader that judged the value could let an added job through
// unnoticed, which is the fail-open this reader exists to close. A quoted key is cut at its closing
// quote, so a key that itself contains a colon is read whole rather than split at the wrong colon. It
// answers ok=false only for a line whose key cannot be read at all (no colon, an empty key, a quoted
// key with no closing quote), which the caller answers as unreadable.
func trainJobKey(line string) (string, bool) {
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	var key string
	if quote := line[0]; quote == '"' || quote == '\'' {
		end := strings.IndexByte(line[1:], quote)
		if end < 0 {
			return "", false
		}
		key = line[1 : 1+end]
		if !strings.HasPrefix(strings.TrimSpace(line[2+end:]), ":") {
			return "", false
		}
	} else {
		cut, _, found := strings.Cut(line, ":")
		if !found {
			return "", false
		}
		key = strings.TrimSpace(cut)
	}
	if key == "" || strings.ContainsAny(key, "{}[]") {
		return "", false
	}
	return key, true
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
// looked for inside that job's own block, so a workflow that declares no job after it still reads. A
// matrix that also carries an include or exclude key is refused: those keys add or remove legs whose
// names this text scan cannot compute, and reading only the part list would report an unchanged job
// set for a head that added a leg — the fail-open this reader exists to close (CRW-897, answer 2).
func trainWorkflowMatrixParts(workflow string) ([]string, error) {
	body, found := trainJobBody(workflow, trainProductJob)
	if !found {
		return nil, errors.New("the workflow holds no " + trainProductJob + " job")
	}
	if trainMatrixHasUnreadableKey(body) {
		return nil, errors.New("the workflow's " + trainProductJob + " matrix carries an include or exclude list, whose legs this reader cannot read key by key")
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

// trainMatrixHasUnreadableKey reports whether the go-product job's strategy.matrix carries a key this
// reader cannot compute legs from. The leg names a run reports come from the matrix, and this text
// scan reads them only from the part list: an include or exclude list, or any other axis, recombines
// or adds legs, so it is refused rather than read as the part list alone — the fail-open this reader
// exists to close (CRW-897, answer 2). Only the matrix block's own keys are judged, so an "include:"
// in a step input or an env entry elsewhere in the job is not a matrix key.
func trainMatrixHasUnreadableKey(body string) bool {
	_, after, found := strings.Cut(body, "\n      matrix:\n")
	if !found {
		return false
	}
	for _, line := range strings.Split(after, "\n") {
		bare := trainStripComment(line)
		if bare == "" {
			continue
		}
		if !strings.HasPrefix(bare, "        ") || strings.HasPrefix(bare, "         ") {
			// the matrix block ends at the first line at or above its own key indent
			return false
		}
		if key, ok := trainJobKey(strings.TrimPrefix(bare, "        ")); ok && key != "part" {
			return true
		}
	}
	return false
}

// trainJobBody is the text of one job's block: from its header line to the next job's header, or to
// the end of the workflow. The search runs inside the jobs block only, so a two-space key that sits
// elsewhere in the workflow — an "env:" entry, say — is never mistaken for a job header. The header is
// matched with a trailing YAML comment stripped, so a workflow that comments its job keys still reads.
func trainJobBody(workflow, job string) (string, bool) {
	body, inline, found := trainJobsBlock(workflow)
	if !found || inline {
		return "", false
	}
	lines := strings.SplitAfter(body, "\n")
	start := -1
	offset := 0
	for i, line := range lines {
		bare := trainStripComment(strings.TrimSuffix(line, "\n"))
		if name, ok := trainJobKey(strings.TrimPrefix(bare, "  ")); ok && name == job && strings.HasPrefix(bare, "  ") && !strings.HasPrefix(bare, "   ") {
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
		if _, ok := trainJobKey(strings.TrimPrefix(bare, "  ")); ok && strings.HasPrefix(bare, "  ") && !strings.HasPrefix(bare, "   ") {
			break
		}
		end += len(line)
	}
	return body[start : start+end], true
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
