package goalplan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

type obj = map[string]any

func ptr(s string) *string { return &s }

func with(base obj, extra obj) obj {
	m := obj{}
	for k, v := range base {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func lane(extra obj) obj { return with(obj{"launchId": "r1-x"}, extra) }

func round(extra obj) obj {
	return with(obj{"roundId": "r1", "purpose": "plan_audit", "planPath": "p", "planSha256": "h", "status": "pending", "lane": lane(nil)}, extra)
}

func TestReviveLane(t *testing.T) {
	for name, c := range map[string]struct {
		raw  any
		want *ReviewLane
	}{
		"not an object":              {"x", nil},
		"null":                       {nil, nil},
		"a list":                     {[]any{}, nil},
		"no launch id":               {obj{}, nil},
		"empty launch id":            {obj{"launchId": ""}, nil},
		"launch id not text":         {obj{"launchId": 5}, nil},
		"launch id only":             {obj{"launchId": "l"}, &ReviewLane{LaunchID: "l"}},
		"empty text kept":            {lane(obj{"reviewerSession": "", "workspaceRoot": "", "artifactSha256": ""}), &ReviewLane{LaunchID: "r1-x", ReviewerSession: ptr(""), WorkspaceRoot: ptr(""), ArtifactSha256: ptr("")}},
		"text of another type":       {lane(obj{"reviewerSession": 5, "workspaceRoot": nil}), &ReviewLane{LaunchID: "r1-x"}},
		"verdict kept":               {lane(obj{"verdict": "near-pass"}), &ReviewLane{LaunchID: "r1-x", Verdict: VerdictNearPass}},
		"verdict of another case":    {lane(obj{"verdict": "PASS"}), &ReviewLane{LaunchID: "r1-x"}},
		"bad identity drops only it": {lane(obj{"verdict": "fail", "sourceIdentity": obj{"kind": "other"}}), &ReviewLane{LaunchID: "r1-x", Verdict: VerdictFail}},
		"identity kept":              {lane(obj{"sourceIdentity": obj{"kind": "unavailable"}}), &ReviewLane{LaunchID: "r1-x", SourceIdentity: &SourceIdentity{Kind: source.KindUnavailable, CapturedAt: epoch}}},
		"other keys dropped":         {lane(obj{"evil": 1}), &ReviewLane{LaunchID: "r1-x"}},
	} {
		if got := reviveLane(c.raw); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
	}
}

func TestReviveReviewRounds(t *testing.T) {
	for _, raw := range []any{nil, "x", obj{}, 5, true} {
		if got := reviveReviewRounds(raw); got != nil {
			t.Errorf("%v is not a list: got %v, want absent", raw, got)
		}
	}
	// A list that held nothing valid is not absent: it is stored as [].
	for _, raw := range []any{[]any{}, []any{nil, 5, "x", []any{}, obj{}, round(obj{"status": "done"})}} {
		if got := reviveReviewRounds(raw); got == nil || len(got) != 0 {
			t.Errorf("%v: got %#v, want a non-nil empty list", raw, got)
		}
	}
	for name, bad := range map[string]obj{
		"no round id": round(obj{"roundId": ""}), "purpose": round(obj{"purpose": "audit"}), "no plan path": round(obj{"planPath": nil}),
		"no plan hash": round(obj{"planSha256": 5}), "status of a gate": round(obj{"status": "approved_gate"}), "no lane": round(obj{"lane": nil}),
		"lane without a launch id": round(obj{"lane": obj{}}),
	} {
		got := reviveReviewRounds([]any{bad, round(obj{"roundId": "ok"})})
		if len(got) != 1 || got[0].RoundID != "ok" {
			t.Errorf("%s: got %+v, want the invalid round dropped and the valid one kept", name, got)
		}
	}
	want := ReviewRoundState{RoundID: "r1", Purpose: PurposeFinalGate, Status: ReviewInFlight, Lane: ReviewLane{LaunchID: "r1-x"}, OpenedAt: epoch}
	if got := reviveReviewRounds([]any{round(obj{"purpose": "final_gate", "planPath": "", "planSha256": "", "status": "in_flight", "openedAt": 7})}); !reflect.DeepEqual(got, []ReviewRoundState{want}) {
		t.Errorf("empty plan path and hash are valid and a non-text openedAt is the epoch: got %+v", got)
	}
	files := []any{obj{"path": "a", "sha256": "b"}}
	kept := reviveReviewRounds([]any{round(obj{"closedAt": "", "ownerSessionId": "s", "workPhaseId": "", "planUnit": 5, "planEpoch": "e", "planFiles": files})})
	if len(kept) != 1 {
		t.Fatalf("a round with binding fields: got %+v", kept)
	}
	if bound := kept[0]; bound.ClosedAt == nil || *bound.ClosedAt != "" || bound.OwnerSessionID != "s" || bound.WorkPhaseID != "" || bound.PlanUnit != "" || bound.PlanEpoch != "e" || len(bound.PlanFiles) != 1 {
		t.Errorf("binding fields are kept only when non-empty text, closedAt when text: got %+v", bound)
	}
}

func TestRevivePlanFiles(t *testing.T) {
	ok := obj{"path": "a/000.md", "sha256": "aa"}
	if got := revivePlanFiles([]any{ok, obj{"path": "./a/../b/010.md", "sha256": "bb", "size": 3}}); !reflect.DeepEqual(got, []PlanFileHash{{"a/000.md", "aa"}, {"./a/../b/010.md", "bb"}}) {
		t.Errorf("valid list: got %+v", got)
	}
	// A path stays as written, and may name a dotted entry or come back to the working directory without leaving it.
	for _, inside := range []string{"..hidden/000.md", "a/..", ".", "a/../b", "b/.../c"} {
		if got := revivePlanFiles([]any{obj{"path": inside, "sha256": "aa"}}); len(got) != 1 || got[0].Path != inside {
			t.Errorf("%q stays inside the working directory: got %+v", inside, got)
		}
	}
	// A review finding of kind security, fixed on purpose where the oracle keeps the path: a path outside the working
	// directory drops the whole list, however it is spelled.
	for _, outside := range []string{"/etc/hostname", "/", "..", "../x", "a/../../x", "./../x", "a/b/../../../x"} {
		if got := revivePlanFiles([]any{ok, obj{"path": outside, "sha256": "bb"}}); got != nil {
			t.Errorf("%q leaves the working directory: got %+v, want the whole list dropped", outside, got)
		}
	}
	for name, raw := range map[string]any{"absent": nil, "empty": []any{}, "not a list": obj{}, "text": "x"} {
		if got := revivePlanFiles(raw); got != nil {
			t.Errorf("%s: got %+v, want absent", name, got)
		}
	}
	for name, bad := range map[string]any{"null": nil, "number": 5, "no path": obj{"sha256": "b"}, "empty path": obj{"path": "", "sha256": "b"},
		"no hash": obj{"path": "a"}, "empty hash": obj{"path": "a", "sha256": ""}, "hash not text": obj{"path": "a", "sha256": 5}} {
		if got := revivePlanFiles([]any{ok, bad}); got != nil {
			t.Errorf("%s: got %+v, want the whole list dropped", name, got)
		}
	}
}

func TestReviveSourceIdentity(t *testing.T) {
	id := func(extra obj) obj {
		return with(obj{"kind": "resolved", "commitSha": "c", "dirty": true, "capturedAt": "t"}, extra)
	}
	full := &SourceIdentity{Kind: source.KindResolved, CommitSha: "c", Dirty: true, CapturedAt: "t", TreeHash: ptr(""), SourceRoot: ptr("/ws/../x")}
	if got := reviveSourceIdentity(id(obj{"treeHash": "", "sourceRoot": "/ws/../x", "extra": 1})); !reflect.DeepEqual(got, full) {
		t.Errorf("got %+v, want %+v", got, full)
	}
	// What state.reconstructSourceIdentity refuses is revived with defaults here: that rebuild is strict, this one is not.
	lenient := &SourceIdentity{Kind: source.KindUnavailable, CapturedAt: epoch}
	if got := reviveSourceIdentity(obj{"kind": "unavailable", "commitSha": 5, "dirty": "yes", "capturedAt": nil}); !reflect.DeepEqual(got, lenient) {
		t.Errorf("defaults: got %+v, want %+v", got, lenient)
	}
	if got := reviveSourceIdentity(id(obj{"treeHash": nil})); got == nil || got.TreeHash != nil {
		t.Errorf("a tree hash that is not text is dropped, not an error: got %+v", got)
	}
	for name, raw := range map[string]any{"null": nil, "text": "x", "list": []any{}, "no kind": obj{}, "kind of another case": id(obj{"kind": "Resolved"}),
		"relative root": id(obj{"sourceRoot": "ws"}), "empty root": id(obj{"sourceRoot": ""}), "null root": id(obj{"sourceRoot": nil}), "root not text": id(obj{"sourceRoot": 5})} {
		if got := reviveSourceIdentity(raw); got != nil {
			t.Errorf("%s: got %+v, want nil", name, got)
		}
	}
}

func TestReviveFinalGate(t *testing.T) {
	gate := func(extra obj) obj { return with(obj{"status": "approved", "qaRequired": false}, extra) }
	want := &FinalGateState{Status: GateApproved, UpdatedAt: epoch, Verdict: VerdictPass, TestReceiptPath: ptr(""), SourceIdentity: &SourceIdentity{Kind: source.KindResolved, CapturedAt: epoch}}
	if got := reviveFinalGate(gate(obj{"verdict": "pass", "testReceiptPath": "", "qaReceiptPath": 5, "extra": 1, "sourceIdentity": obj{"kind": "resolved"}})); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := reviveFinalGate(gate(obj{"qaRequired": true, "updatedAt": "t", "reviewRoundId": "r2"})); got == nil || !got.QaRequired || got.UpdatedAt != "t" || *got.ReviewRoundID != "r2" {
		t.Errorf("got %+v", got)
	}
	for name, raw := range map[string]any{"null": nil, "text": "x", "list": []any{}, "empty": obj{}, "no status": gate(obj{"status": nil}),
		"status of a round": gate(obj{"status": "changes_requested"}), "launching is not a gate status": gate(obj{"status": "launching"}),
		"no qaRequired": gate(obj{"qaRequired": nil}), "qaRequired as text": gate(obj{"qaRequired": "true"}), "qaRequired as a number": gate(obj{"qaRequired": 1})} {
		if got := reviveFinalGate(raw); got != nil {
			t.Errorf("%s: got %+v, want no gate", name, got)
		}
	}
}

func TestSchemaConstants(t *testing.T) {
	if SupportedMaxSchemaVersion != 3 || DefaultNewSchemaVersion != 1 {
		t.Errorf("schema versions: max %d, default %d", SupportedMaxSchemaVersion, DefaultNewSchemaVersion)
	}
	if got := GoalplanLockRetryDelaysMs(); !reflect.DeepEqual(got, []int{5, 10, 20, 40}) {
		t.Errorf("lock retry delays %v", got)
	}
	GoalplanLockRetryDelaysMs()[0] = 99
	if GoalplanLockRetryDelaysMs()[0] != 5 {
		t.Error("a caller changed the retry delays for the next one")
	}
}

// A plan stores absent and empty apart: a nil list and a nil pointer are left out, a non-nil empty list is written as [] and a
// pointer to the empty string as "".
func TestStoredShapes(t *testing.T) {
	tasks := []GoalplanTask{{ID: "t1", Title: "x", Status: TaskDone, DependsOn: []string{}}, {ID: "t2", Title: "y", Status: TaskPending}}
	phase := GoalplanWorkPhase{ID: "wp1", Title: "t", Status: WorkPhasePending, Tasks: tasks, CriteriaIDs: []string{}, BlockedReason: ptr("")}
	plan := Goalplan{Objective: "o", Slug: "s", CreatedAt: epoch, UpdatedAt: epoch, WorkPhases: []GoalplanWorkPhase{phase}, Criteria: []GoalplanCriterion{},
		Host: GoalplanHostLink{Source: HostSourceNone}, ReviewRounds: []ReviewRoundState{}}
	const want = `{"objective":"o","slug":"s","createdAt":"1970-01-01T00:00:00.000Z","updatedAt":"1970-01-01T00:00:00.000Z","activeWorkPhaseId":null,` +
		`"workPhases":[{"id":"wp1","title":"t","status":"pending","tasks":[{"id":"t1","title":"x","status":"done","dependsOn":[]},{"id":"t2","title":"y","status":"pending"}],"criteriaIds":[],"blockedReason":""}],` +
		`"criteria":[],"host":{"armed":false,"armedAt":null,"source":"none"},"reviewRounds":[]}`
	if got := compact(t, plan); got != want {
		t.Errorf("stored\n%s\nwant\n%s", got, want)
	}
	plan.ReviewRounds = nil
	if got := compact(t, plan); strings.Contains(got, "reviewRounds") {
		t.Errorf("an absent list is written: %s", got)
	}
}

// A ledger row says which round it is about by two optional texts; an empty one is still text and is written back.
func TestLedgerRowKeepsEmptyRoundText(t *testing.T) {
	for _, row := range []string{
		`{"ts":"t","slug":"s","event":"created","detail":"d"}`,
		`{"ts":"t","slug":"s","event":"steered","detail":"d","roundId":"","launchId":""}`,
		`{"ts":"t","slug":"s","event":"review_round_superseded","detail":"d","roundId":"r1","launchId":"r1-x"}`,
	} {
		var entry GoalplanLedgerEntry
		if err := json.Unmarshal([]byte(row), &entry); err != nil {
			t.Fatal(err)
		}
		if got := compact(t, entry); got != row {
			t.Errorf("ledger row\n%s\nwritten back as\n%s", row, got)
		}
	}
}
