package goalplan

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"golang.org/x/sys/unix"
)

// GoalplanReadDiagnostic and GoalplanReadResult are goalplan.ts:679-688.
// Diagnostic kinds are absent, unreadable, invalid-json and invalid-shape.
type GoalplanReadDiagnostic struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Field  string `json:"field,omitempty"`
	Detail string `json:"detail,omitempty"`
}
type GoalplanReadResult struct {
	Plan       *Goalplan               `json:"plan"`
	Diagnostic *GoalplanReadDiagnostic `json:"diagnostic"`
}

func readFailure(kind, path, detail, field string) GoalplanReadResult {
	if kind == "absent" {
		detail = ""
	}
	return GoalplanReadResult{Diagnostic: &GoalplanReadDiagnostic{Kind: kind, Path: path, Detail: detail, Field: field}}
}

// ReadGoalplanDetailed is readGoalplanDetailed (:698-742). It never throws;
// OS/JSON parser errors use Go's wording. Open/fstat binding fixes linked paths.
func ReadGoalplanDetailed(cwd, slug string) GoalplanReadResult {
	path, err := goalplanPath(cwd, slug)
	if err != nil {
		return readFailure("unreadable", slug, err.Error(), "")
	}
	dir, real, err := openPlanDir(cwd, slug)
	if err != nil {
		kind := "unreadable"
		if pathAbsent(err) {
			kind = "absent"
		}
		return readFailure(kind, path, err.Error(), "")
	}
	defer dir.Close()
	return readPlanAt(dir, real, path, slug)
}

// ReadGoalplan is the null-on-failure compatibility wrapper (:746-748).
func ReadGoalplan(cwd, slug string) *Goalplan { return ReadGoalplanDetailed(cwd, slug).Plan }

func readPlanAt(dir *os.File, real, path, slug string) GoalplanReadResult {
	result, _ := revivalLossReadPlan(dir, real, path, slug)
	return result
}

// revivalLossReadPlan is readPlanAt that also returns what the write lock judges besides the plan (see revivalLossFile).
func revivalLossReadPlan(dir *os.File, real, path, slug string) (GoalplanReadResult, revivalLossFile) {
	err := boundFile(dir, real, true)
	var file *os.File
	if err == nil {
		file, err = openAt(dir, GoalplanFile, filepath.Join(real, GoalplanFile), unix.O_RDONLY, false, 0)
	}
	if err != nil {
		kind := "unreadable"
		if pathAbsent(err) {
			kind = "absent"
		}
		return readFailure(kind, path, err.Error(), ""), revivalLossFile{openErr: err}
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return readFailure("unreadable", path, err.Error(), ""), revivalLossFile{}
	}
	decoded := source.DecodeUTF8(raw)
	dec := json.NewDecoder(strings.NewReader(decoded))
	dec.UseNumber()
	var parsed any
	err = dec.Decode(&parsed)
	if err == nil {
		_, err = dec.Token()
		if err == io.EOF {
			err = nil
		} else if err == nil {
			err = fmt.Errorf("unexpected trailing JSON value")
		}
	}
	surrogate := ""
	if at := unpairedSurrogate(decoded); at >= 0 {
		surrogate = fmt.Sprintf("unpaired JSON surrogate at byte %d would lose stored text", at)
	}
	if err != nil {
		// Text that does not parse keeps what the write lock judges: it is the oracle's unparseable plan, which the
		// orchestrate gate goes on from, only when nothing in it would be lost by a write (CRW-975).
		return readFailure("invalid-json", path, err.Error(), ""), revivalLossFile{text: decoded, badByte: revivalLossBadByte(raw), refuse: surrogate}
	}
	if surrogate != "" {
		return readFailure("unreadable", path, surrogate, ""), revivalLossFile{refuse: surrogate}
	}
	plan := reviveGoalplan(parsed, &slug)
	if plan == nil {
		// No plan, but the text is kept for the write lock: stored data a write would lose (bytes that are not UTF-8, a
		// repeated key) makes the file a refusal there, not an absent plan (CRW-975).
		field := firstInvalidField(parsed, &slug)
		return readFailure("invalid-shape", path, "the goalplan parsed as JSON but field '"+field+"' did not satisfy the schema", field), revivalLossFile{text: decoded, badByte: revivalLossBadByte(raw)}
	}
	return GoalplanReadResult{Plan: plan}, revivalLossFile{parsed: parsed, text: decoded, badByte: revivalLossBadByte(raw)}
}

// UnpairedJSONSurrogate is the offset of the first unpaired \u surrogate escape in JSON text s, -1 when there is none: the
// goalplan reader's refusal, for a caller that decodes JSON of its own (the loop CLI's steering batch).
func UnpairedJSONSurrogate(s string) int { return unpairedSurrogate(s) }

// Refuse valid JSON that encoding/json would decode lossily (data-loss exception).
// Escaped backslashes and complete surrogate pairs keep their original meaning. s
// may also be text that does not parse (the write lock judges it too).
func unpairedSurrogate(s string) int {
	unit := func(at int) uint64 {
		if at+6 > len(s) || s[at:at+2] != `\u` {
			return 0
		}
		n, _ := strconv.ParseUint(s[at+2:at+6], 16, 16)
		return n
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		n := unit(i)
		if n >= 0xd800 && n <= 0xdbff {
			m := unit(i + 6)
			if m < 0xdc00 || m > 0xdfff {
				return i
			}
			i += 11
		} else if n >= 0xdc00 && n <= 0xdfff {
			return i
		} else if i+1 < len(s) && s[i+1] == 'u' {
			i += 5
		} else {
			i++
		}
	}
	return -1
}

// firstInvalidField follows the oracle's diagnostic order (:875-913). The oracle answers (unknown) for a
// slug that is not a valid one or not the requested one, a criterion with no id and a malformed steering
// entry, and blames a missing host - which revival defaults and never refuses - for whatever failed after it;
// CRW-1109 names the field each of those refusals is about instead. What is refused is unchanged.
func firstInvalidField(parsed any, expectedSlug *string) string {
	o, ok := parsed.(map[string]any)
	if !ok {
		return "(root: not an object)"
	}
	if _, ok := text(o, "objective"); !ok {
		return "objective"
	}
	slug, ok := text(o, "slug")
	if !ok {
		return "slug"
	}
	if _, err := ValidateGoalplanSlug(slug); err != nil {
		return "slug (not a valid goalplan slug)"
	}
	if expectedSlug != nil && slug != *expectedSlug {
		return "slug (the stored slug is not the requested one)"
	}
	if declaredSchemaVersion(o) > SupportedMaxSchemaVersion {
		return "schemaVersion"
	}
	if !validSchemaVersion(o) {
		return "schemaVersion (a whole number from 1)"
	}
	phases, ok := o["workPhases"].([]any)
	if !ok {
		return "workPhases"
	}
	for _, raw := range phases {
		w, ok := raw.(map[string]any)
		_, id := text(w, "id")
		_, title := text(w, "title")
		if !ok || !id || !title {
			return "workPhases[] entries (each needs id/title)"
		}
	}
	for _, raw := range phases {
		w := raw.(map[string]any)
		if _, ok := reviveDependsOn(w, "dependsOn"); !ok {
			return "workPhases[].dependsOn"
		}
		if _, ok := reviveDependsOn(w, "awaitsDecision"); !ok {
			return "workPhases[].awaitsDecision"
		}
		tasks, _ := w["tasks"].([]any)
		for _, raw := range tasks {
			task, ok := raw.(map[string]any)
			_, id := text(task, "id")
			_, title := text(task, "title")
			if !ok || !id || !title {
				continue
			}
			if _, ok := reviveDependsOn(task, "dependsOn"); !ok {
				return "workPhases[].tasks[].dependsOn"
			}
		}
	}
	criteria, ok := o["criteria"].([]any)
	if !ok {
		return "criteria"
	}
	for _, raw := range criteria {
		c, ok := raw.(map[string]any)
		if _, scenario := text(c, "scenario"); !ok || !scenario {
			return "criteria[] entries (each needs scenario/expectedEvidence/status)"
		}
		if _, id := text(c, "id"); !id {
			return "criteria[].id"
		}
	}
	if _, ok := reviveDecisions(o, "decisions"); !ok {
		return "decisions"
	}
	if v, present := o["steeringLog"]; present {
		list, ok := v.([]any)
		if !ok {
			return "steeringLog"
		}
		for _, entry := range list {
			e, ok := entry.(map[string]any)
			for _, k := range [...]string{"idempotencyKey", "rationale", "evidence", "appliedAt", "summary"} {
				if f, _ := text(e, k); !ok || f == "" {
					return "steeringLog[] entries (each needs idempotencyKey/rationale/evidence/appliedAt/summary)"
				}
			}
			if _, ok := reviveSteeringOps(e["ops"]); !ok {
				return "steeringLog[].ops"
			}
			if _, ok := reviveSteeringEvents(e["events"]); !ok {
				return "steeringLog[].events"
			}
		}
	}
	return "(unknown)"
}
