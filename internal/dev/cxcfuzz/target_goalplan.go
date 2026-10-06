//go:build dev

package cxcfuzz

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// goalplanSlug is the plan every generated case writes and rewrites.
const goalplanSlug = "rec-plan"

// goalplanTarget is the goalplan read and rewrite (CRW-709, absorbed into CRW-708):
// goalplan.ReadGoalplanDetailed and, under the write lock, goalplan.WriteGoalplan against the oracle
// readGoalplanDetailed and writeGoalplan at CXC v0.2.40 (pabcd-state/dist/goalplan.js:698, :921).
func goalplanTarget() Target {
	return Target{
		Name:     "goalplan",
		Generate: goalplanGenerate,
		Go:       goalplanGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("goalplan"), Root: DefaultOracleRoot},
		Compare:  goalplanCompare,
	}
}

// goalplanGo is the Go side: read the plan, then rewrite it under the write lock, then the rewritten
// bytes. A refused write is reported as writeError.
func goalplanGo(input any, env Env) (any, error) {
	read := goalplan.ReadGoalplanDetailed(env.Root, goalplanSlug)
	answer := goalplanAnswer(read)
	if read.Plan == nil {
		return maskTimestamps(answer, false), nil
	}
	locked, err := goalplan.WithGoalplanWriteLock(env.Root, goalplanSlug, func(plan *goalplan.Goalplan) (struct{}, error) {
		return struct{}{}, goalplan.WriteGoalplan(env.Root, plan)
	}, nil)
	if err != nil {
		return maskTimestamps(answer.Set("writeError", err.Error()), false), nil
	}
	// The lock is an ok/locked/unreadable union: a refusal never reaches the write, so its reason is
	// the answer rather than the original bytes left on disk.
	if locked.Kind != "ok" {
		return maskTimestamps(answer.Set("writeError", locked.Kind+": "+locked.Reason), false), nil
	}
	written, err := os.ReadFile(filepath.Join(env.Root, ".crw", "goalplans", goalplanSlug, goalplan.GoalplanFile))
	if err != nil {
		return maskTimestamps(answer.Set("writeError", err.Error()), false), nil
	}
	return maskTimestamps(answer.Set("written", string(written)), false), nil
}

// goalplanAnswer is the read half of both sides' answers: the diagnostic kind and field, and the
// revived plan as its text form or null. The diagnostic path and detail are excluded: the port uses
// Go's own OS and JSON-parser wording there by contract (internal/pabcd/goalplan/read.go), so they
// are a documented, deliberate difference rather than a divergence this target can compare.
func goalplanAnswer(read goalplan.GoalplanReadResult) pyjson.Object {
	kind, field := "ok", ""
	if read.Diagnostic != nil {
		kind, field = read.Diagnostic.Kind, read.Diagnostic.Field
	}
	answer := pyjson.Object{{Key: "kind", Value: kind}}
	if field != "" {
		answer = answer.Set("field", field)
	}
	if read.Plan == nil {
		return answer.Set("plan", nil)
	}
	// The plan's text form, indented two spaces as the oracle prints it with JSON.stringify(plan, null,
	// 2). jsonStringify does not escape <, > or & as encoding/json does, so a plan holding markup
	// compares equal instead of a false divergence. The comparison is canonical, so key order does not
	// matter.
	encoded, err := jsonStringify(read.Plan)
	if err != nil {
		return answer.Set("plan", nil)
	}
	return answer.Set("plan", string(encoded))
}

// goalplanCompare compares the read kind, field and revived plan, and the rewritten bytes.
// jsonStringify is JSON.stringify(value, null, 2) for a value encoding/json can carry: it leaves <, >
// and & literal and writes U+2028 and U+2029 as themselves, where encoding/json would escape them.
// The port's own JSON.stringify-compatible encoder (writeEncodeJSON) is private to internal/pabcd/
// goalplan, so this target keeps its own copy for the read-plan snapshot.
func jsonStringify(value any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	in := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out, i = append(out, "\u2028"...), i+5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out, i = append(out, "\u2029"...), i+5
		default:
			out, i = append(out, in[i], in[i+1]), i+1
		}
	}
	return out, nil
}

func goalplanCompare(goOut, oracleOut any) Verdict {
	if canonical(goOut) == canonical(oracleOut) {
		return Verdict{Kind: Same}
	}
	// A refused Go write leaves no written bytes; name that rather than a generic difference.
	if _, refused := field(goOut, "writeError"); refused {
		if _, published := field(oracleOut, "written"); published {
			return Verdict{Kind: Differ, Detail: "the Go write lock refused a rewrite the oracle published"}
		}
	}
	if goKind, ok := field(goOut, "written"); ok {
		if oracleKind, ok := field(oracleOut, "written"); ok {
			if goText, ok := goKind.(string); ok {
				if oracleText, ok := oracleKind.(string); ok {
					if lost := goalplanLostKeys(goText, oracleText); len(lost) > 0 {
						return Verdict{Kind: Differ, Detail: "data-loss: the Go rewrite drops " + strings.Join(lost, ", ")}
					}
				}
			}
		}
	}
	return Verdict{Kind: Differ, Detail: "the read result or the rewritten bytes differ"}
}

// goalplanLostKeys names the top-level keys of the oracle written plan that the Go written plan lacks.
func goalplanLostKeys(goText, oracleText string) []string {
	goValue, err := decode(goText)
	if err != nil {
		return nil
	}
	oracleValue, err := decode(oracleText)
	if err != nil {
		return nil
	}
	goKeys := map[string]bool{}
	for _, key := range pyjsonFields(goValue) {
		goKeys[key] = true
	}
	lost := []string{}
	for _, key := range pyjsonFields(oracleValue) {
		if !goKeys[key] {
			lost = append(lost, key)
		}
	}
	sort.Strings(lost)
	return lost
}

// goalplanPlans are the plan documents the issue names: schemaVersion 1 to 4 and 1.0 and 1e21,
// duplicate ids, null and unknown fields, a task without a title, an empty planFiles sha256,
// reviewRounds and their planFiles, a roundId near 2^53, a lone surrogate and a deep dependsOn.
func goalplanPlans(rng *rand.Rand) string {
	switch rng.Intn(16) {
	case 0:
		return `{`
	case 1:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "schemaVersion": 1}`
	case 2:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "schemaVersion": 1.0}`
	case 3:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "schemaVersion": 2}`
	case 4:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "schemaVersion": 3}`
	case 5:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "schemaVersion": 4}`
	case 6:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "schemaVersion": 4.5}`
	case 7:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "schemaVersion": 1e21}`
	case 8:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [{"id": "wp1", "title": "t", "id": "wp2"}], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}}`
	case 9:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [{"id": "wp1"}], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}}`
	case 10:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [{"id": "wp1", "title": "t", "tasks": [{"id": "t1"}]}], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}}`
	case 11:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "reviewRounds": [{"roundId": "r1", "purpose": "plan_audit", "planPath": "a", "planSha256": "", "status": "pending", "lane": {"launchId": "r1-20260101000000"}, "openedAt": "2026-01-01T00:00:00.000Z", "planFiles": [{"path": "a", "sha256": ""}]}]}`
	case 12:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "reviewRounds": [{"roundId": 9007199254740993, "purpose": "plan_audit", "planPath": "a", "planSha256": "b", "status": "pending", "lane": {"launchId": "l"}, "openedAt": "2026-01-01T00:00:00.000Z"}]}`
	case 13:
		return `{"objective": "o", "slug": "rec-plan", "workPhases": [{"id": "wp1", "title": "t", "dependsOn": ["wp2"]}], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}}`
	case 14:
		return `{"objective": "o\ud800", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}}`
	default:
		return goalplanPlan(rng)
	}
}

// goalplanPlan is a plausible plan with a random mix of phases, tasks, criteria and host link.
func goalplanPlan(rng *rand.Rand) string {
	phases := []string{
		`{"id": "wp1", "title": "first", "status": "pending", "tasks": [{"id": "t1", "title": "a", "status": "pending"}], "criteriaIds": ["c-1"]}`,
		`{"id": "wp2", "title": "second", "status": "done", "dependsOn": ["wp1"], "tasks": []}`,
	}
	parts := []string{`"objective": "o"`, `"slug": "rec-plan"`}
	count := rng.Intn(3)
	if count > 0 {
		parts = append(parts, `"workPhases": [`+strings.Join(phases[:count], ", ")+`]`)
	} else {
		parts = append(parts, `"workPhases": []`)
	}
	parts = append(parts, `"criteria": [{"id": "c-1", "scenario": "s", "surface": "logic", "expectedEvidence": "", "capturedEvidence": "", "status": "open"}]`)
	parts = append(parts, `"host": {"armed": `+strconv.FormatBool(rng.Intn(2) == 0)+`, "armedAt": null, "source": "none"}`)
	return "{" + strings.Join(parts, ", ") + "}"
}

// goalplanGenerate builds one case: the plan bytes at the Go side's own path,
// .crw/goalplans/rec-plan/goalplan.json. The document is stored once: the shim mirrors it to the
// oracle's .codexclaw path, so the shrinker cannot drop one copy and leave the two sides reading
// different documents.
func goalplanGenerate(rng *rand.Rand, size int) any {
	text := goalplanPlans(rng)
	return pyjson.Object{{Key: "fs", Value: []any{
		pyjson.Object{{Key: "path", Value: filepath.Join(".crw", "goalplans", goalplanSlug, "goalplan.json")}, {Key: "kind", Value: "file"}, {Key: "content", Value: text}},
	}}}
}
