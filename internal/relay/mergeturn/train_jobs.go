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
		// a job with a strategy.matrix reports one leg per combination ("go-product (lint)"), so its
		// legs are expanded here rather than the job id alone; a matrix this reader cannot compute
		// legs from is an error, never a plain job id that would hide the added legs
		// (CRW-897, answer 2).
		parts, hasMatrix, err := trainJobMatrixParts(workflow, name)
		if err != nil {
			return nil, err
		}
		if !hasMatrix {
			out = append(out, name)
			continue
		}
		for _, part := range parts {
			out = append(out, name+" ("+part+")")
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
	key, _, ok := trainJobKeyValue(line)
	return key, ok
}

// trainJobKeyValue reads one line's key and the rest of the line after the key's colon: "audit:" gives
// ("audit", ""), "audit: {runs-on: x}" gives ("audit", "{runs-on: x}") and a quoted key gives its own
// text and the rest. The caller that must judge a key's value uses the second result rather than
// trimming a literal prefix, so a quoted key is read the same as an unquoted one.
func trainJobKeyValue(line string) (string, string, bool) {
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	var key, value string
	if quote := line[0]; quote == '"' || quote == '\'' {
		end := strings.IndexByte(line[1:], quote)
		if end < 0 {
			return "", "", false
		}
		key = line[1 : 1+end]
		rest := strings.TrimSpace(line[2+end:])
		if !strings.HasPrefix(rest, ":") {
			return "", "", false
		}
		value = strings.TrimSpace(rest[1:])
	} else {
		cut, rest, found := strings.Cut(line, ":")
		if !found {
			return "", "", false
		}
		key, value = strings.TrimSpace(cut), strings.TrimSpace(rest)
	}
	if key == "" || strings.ContainsAny(key, "{}[]") {
		return "", "", false
	}
	return key, value, true
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

// trainWorkflowMatrixParts reads the go-product job's strategy.matrix.part list. The keys are found
// by walking the job's own block at the indentation its mapping uses — the job header sits at two
// spaces, so its direct children sit at four, the strategy's at six and the matrix's keys at eight —
// rather than by searching the job text for a key name. A name inside a scalar (a run script or an
// env value) is therefore never taken for a mapping key, and a part: elsewhere in the job is never
// read as the matrix's list (CRW-897, answer 2; pre-merge evaluation d1, d2).
func trainJobMatrixParts(workflow, job string) ([]string, bool, error) {
	body, found := trainJobBody(workflow, job)
	if !found {
		return nil, false, errors.New("the workflow holds no " + job + " job")
	}
	matrix, hasMatrix, err := trainMatrixBlock(body)
	if err != nil {
		return nil, false, err
	}
	if !hasMatrix {
		// the product job reports one leg per matrix combination, so a product job with no matrix
		// has no leg names this reader can compute; another job without a matrix is just its id
		if job == trainProductJob {
			return nil, false, errors.New("the workflow's " + trainProductJob + " job has no matrix")
		}
		return nil, false, nil
	}
	var part string
	for _, line := range matrix {
		bare := trainStripComment(line)
		if strings.TrimSpace(bare) == "" {
			continue
		}
		key, value, ok := trainJobKeyValue(strings.TrimSpace(bare))
		if !ok {
			return nil, false, errors.New("the workflow's " + job + " matrix holds a line whose key this reader cannot read")
		}
		if key != "part" {
			return nil, false, errors.New("the workflow's " + job + " matrix carries the key " + pyvalue.StrRepr(key) + ", and this reader can compute leg names only from a part list")
		}
		part = value
	}
	if part == "" {
		return nil, false, errors.New("the workflow's " + job + " job has no matrix part list")
	}
	if !strings.HasPrefix(part, "[") {
		return nil, false, errors.New("the workflow's " + job + " matrix part list is not an inline list")
	}
	list, _, found := strings.Cut(part[1:], "]")
	if !found {
		return nil, false, errors.New("the workflow's " + job + " matrix part list is unterminated")
	}
	parts := strings.Split(list, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, true, nil
}

// trainMatrixBlock answers the go-product job body's strategy.matrix lines: the keys at the matrix's
// own indentation, stopping at the first line at or above the matrix key's indent. The job's children
// are at four spaces (the header is at two), so strategy must be there and matrix at six; a
// strategy: or matrix: inside a scalar is at another indent and is not read. A matrix key whose value
// sits on the matrix: line itself ("matrix: &anchor" or "matrix: {part: [...]}") carries no readable
// key block, so the block is reported as absent and the caller refuses it.
func trainMatrixBlock(body string) ([]string, bool, error) {
	lines := strings.Split(body, "\n")
	// the job header sits at two spaces, so its own keys (strategy among them) sit at four and the
	// matrix at six. Requiring those exact indents is what keeps a key name inside a scalar — a run
	// script or an env value — from being read as the job's strategy or matrix.
	strategyAt, matrixAt := -1, -1
	for i, line := range lines {
		bare := trainStripComment(line)
		if strings.TrimSpace(bare) == "" {
			continue
		}
		indent := len(bare) - len(strings.TrimLeft(bare, " "))
		key, value, ok := trainJobKeyValue(strings.TrimSpace(bare))
		if !ok {
			continue
		}
		if strategyAt < 0 {
			if key == "strategy" && indent == 4 {
				strategyAt = i
			}
			continue
		}
		if indent <= 4 {
			// the strategy block ended without a matrix key
			break
		}
		if key == "matrix" && indent == 6 {
			// a matrix key whose value sits on the matrix: line itself ("matrix: &anchor" or
			// "matrix: {part: [...]}") carries no readable key block
			if value != "" {
				return nil, false, errors.New("the workflow's job carries its matrix on the matrix: line, whose legs this reader cannot read key by key")
			}
			matrixAt = i
			break
		}
	}
	if matrixAt < 0 {
		return nil, false, nil
	}
	var block []string
	for _, line := range lines[matrixAt+1:] {
		bare := trainStripComment(line)
		if strings.TrimSpace(bare) == "" {
			continue
		}
		if len(bare)-len(strings.TrimLeft(bare, " ")) <= 6 {
			break
		}
		block = append(block, line)
	}
	return block, true, nil
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
