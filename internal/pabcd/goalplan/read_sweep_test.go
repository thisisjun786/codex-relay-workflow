package goalplan

// CRW-1109 in the goalplan reader (docs/port-cxc/known-defects.md :234 and :346). Red on dev: a schemaVersion of 0, a
// negative number, a fraction or -1e999 revived (as written, floored, or dropped to version 1), and the shape diagnostic
// answered (unknown) - or blamed the host - for a slug mismatch, a criterion with no id and a malformed steering entry.

import (
	"os"
	"path/filepath"
	"testing"
)

const readSweepPlan = `{"objective":"o","slug":"demo","workPhases":[],"criteria":[],"host":{"armed":false,"armedAt":null,"source":"none"}`

// A version that is not a whole number from 1 is refused with its own diagnostic, the reader and the write lock leave the
// bytes as they are (the lock refuses the writer: the plan is there, so a gate that reads it cannot go on as if it were absent), and a plan with no version (legacy) or a whole version still reads.
func TestSchemaVersionMustBeAWholeNumberFromOne(t *testing.T) {
	for _, version := range []string{"0", "-7", "2.9", "-0", "1e-400", "-1e999"} {
		cwd, dir := readWorkspace(t)
		path := filepath.Join(dir, GoalplanFile)
		raw := readSweepPlan + `,"schemaVersion":` + version + `}`
		writeReadFile(t, path, raw)
		read := ReadGoalplanDetailed(cwd, "demo")
		if read.Plan != nil || read.Diagnostic == nil || read.Diagnostic.Kind != "invalid-shape" || read.Diagnostic.Field != "schemaVersion (a whole number from 1)" {
			t.Errorf("schemaVersion %s: %+v %+v", version, read.Plan, read.Diagnostic)
		}
		lock, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (string, error) { return "entered", nil }, &GoalplanWriteLockOptions{RetryDelaysMs: []int{}})
		if err != nil || lock.Kind != "unreadable" || !lock.Refused {
			t.Errorf("schemaVersion %s: the lock answered %+v %v", version, lock, err)
		}
		if b, _ := os.ReadFile(path); string(b) != raw {
			t.Errorf("schemaVersion %s: the bytes changed", version)
		}
	}
	for raw, want := range map[string]float64{readSweepPlan + `}`: 0, readSweepPlan + `,"schemaVersion":1}`: 1, readSweepPlan + `,"schemaVersion":"2"}`: 0} {
		cwd, dir := readWorkspace(t)
		writeReadFile(t, filepath.Join(dir, GoalplanFile), raw)
		plan := ReadGoalplan(cwd, "demo")
		if plan == nil || want == 0 && plan.SchemaVersion != nil || want != 0 && (plan.SchemaVersion == nil || *plan.SchemaVersion != want) {
			t.Errorf("%s: %+v", raw, plan)
		}
	}
}

// The shape diagnostic names the field a refusal is about.
func TestShapeDiagnosticNamesTheField(t *testing.T) {
	for raw, want := range map[string]string{
		`{"objective":"o","slug":"other","workPhases":[],"criteria":[]}`:                                       "slug (the stored slug is not the requested one)",
		`{"objective":"o","slug":"demo","workPhases":[],"criteria":[{"scenario":"s"}]}`:                        "criteria[].id",
		`{"objective":"o","slug":"demo","workPhases":[],"criteria":[],"steeringLog":[{}]}`:                     "steeringLog[] entries (each needs idempotencyKey/rationale/evidence/appliedAt/summary)",
		`{"objective":"o","slug":"demo","workPhases":[],"criteria":[],"steeringLog":[{"idempotencyKey":"k"}]}`: "steeringLog[] entries (each needs idempotencyKey/rationale/evidence/appliedAt/summary)",
		`{"objective":"o","slug":"demo","workPhases":[],"criteria":[],"schemaVersion":0}`:                      "schemaVersion (a whole number from 1)",
	} {
		cwd, dir := readWorkspace(t)
		writeReadFile(t, filepath.Join(dir, GoalplanFile), raw)
		read := ReadGoalplanDetailed(cwd, "demo")
		if read.Plan != nil || read.Diagnostic == nil || read.Diagnostic.Field != want {
			t.Errorf("%s: field %+v, want %q", raw, read.Diagnostic, want)
		}
	}
}
