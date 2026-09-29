package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func pythonSet2(t *testing.T, id string) (map[string]any, *store.Store) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/report_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, id, root)
	cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "XDG_DATA_HOME="+root, "XDG_CONFIG_HOME="+root, "CODEX_HOME="+root, "TMPDIR="+os.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python %s: %v: %s", id, err, output)
	}
	var want map[string]any
	if err := json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	// Input cases must retain Python int versus float; unmarshalling all numbers
	// as float64 had made the validator's wrong type labels look correct.
	var typed map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	decoder.UseNumber()
	if err := decoder.Decode(&typed); err != nil {
		t.Fatal(err)
	}
	if cases, exists := typed["cases"]; exists {
		want["cases"] = cases
	}
	ownCopied(t, filepath.Join(root, "relay.sqlite3"), "go")
	s, err := store.Open(context.Background(), filepath.Join(root, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return want, s
}

// set2Compare reports only the first unequal path, preserving exact values without
// dumping multi-kilobyte messages and hiding the first cause in later differences.
func set2Compare(t *testing.T, got, want any) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	if path, left, right, unequal := set2First(normalized, want, "output"); unequal {
		t.Errorf("first difference at %s: Go=%s Python=%s", path, jsonText(left), jsonText(right))
	}
}
func set2First(got, want any, path string) (string, any, any, bool) {
	if reflect.DeepEqual(got, want) {
		return "", nil, nil, false
	}
	left, leftMap := got.(map[string]any)
	right, rightMap := want.(map[string]any)
	if leftMap && rightMap {
		keys := make([]string, 0, len(left)+len(right))
		seen := map[string]bool{}
		for key := range left {
			keys = append(keys, key)
			seen[key] = true
		}
		for key := range right {
			if !seen[key] {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			l, lok := left[key]
			r, rok := right[key]
			if !lok || !rok {
				return path + "." + key, l, r, true
			}
			if p, a, b, diff := set2First(l, r, path+"."+key); diff {
				return p, a, b, true
			}
		}
	}
	if l, ok := got.([]any); ok {
		if r, ok := want.([]any); ok {
			for i := 0; i < len(l) && i < len(r); i++ {
				if p, a, b, diff := set2First(l[i], r[i], fmt.Sprintf("%s[%d]", path, i)); diff {
					return p, a, b, true
				}
			}
			return path + ".length", len(l), len(r), true
		}
	}
	if l, ok := got.(string); ok {
		if r, ok := want.(string); ok {
			index := 0
			for index < len(l) && index < len(r) && l[index] == r[index] {
				index++
			}
			start := index - 40
			if start < 0 {
				start = 0
			}
			endL := index + 100
			if endL > len(l) {
				endL = len(l)
			}
			endR := index + 100
			if endR > len(r) {
				endR = len(r)
			}
			return fmt.Sprintf("%s byte %d", path, index), l[start:endL], r[start:endR], true
		}
	}
	return path, got, want, true
}

func Test24_RC_14_ReadyWholeMessages(t *testing.T) {
	want, s := pythonSet2(t, "RC-14-ready")
	event := want["eventId"].(string)
	legacy, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	fields := set1Fields()
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	set2Compare(t, map[string]any{"eventId": event, "legacy": legacy, "legacyVersion": reportVersion(false), "stored": stored, "version": reportVersion(true), "message": message}, want)
}

func Test24_RC_4_ReadyQualifiedPR(t *testing.T) {
	want, s := pythonSet2(t, "RC-4-ready")
	event := want["eventId"].(string)
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, set1Fields())
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	mine := []any{stored["relationshipId"], stored["repository"], stored["prNumber"]}
	other := []any{"rel-different", "someone-else/other-project", stored["prNumber"]}
	set2Compare(t, map[string]any{"eventId": event, "stored": stored, "message": message, "ref": reportPRRef(stored["repository"].(string), 12), "key": mine, "otherRef": reportPRRef("someone-else/other-project", 12), "otherKey": other}, want)
}

func Test24_RC_4_LiveTwoParents(t *testing.T) {
	want, s := set1Capture(t, "RC-4-two-parents")
	event := want["eventId"].(string)
	mine, err := ReadWorkReport(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	// Replay Isolation.test_two_parents_holding_the_same_pull_request_number_stay_apart:
	// register the same child under another parent for REL-2, then read its complete record.
	otherID, err := store.RelationshipID("01other-parent", "01child-task", "REL-2")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.One(context.Background(), "SELECT child_cwd FROM relationships WHERE relationship_id=?", mine["relationshipId"])
	if err != nil || parent == nil {
		t.Fatalf("first relation: %v", err)
	}
	childRoot := parent.Get("child_cwd").(string)
	at := delivery.NewFakeClock().ISO()
	err = s.Transaction(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		if _, e := conn.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,parent_cxc_session,child_task_id,child_host_id,child_cwd,child_cxc_session,execution_generation,artifact_roots,allowed_recipients,scope_ref,supersedes,superseded_by,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, otherID, "REL-2", "active", "01other-parent", "host-a", "/other", nil, "01child-task", "host-a", childRoot, nil, 1, jsonText([]string{childRoot}), jsonText([]string{"01other-parent"}), nil, nil, nil, at, at); e != nil {
			return e
		}
		if _, e := conn.ExecContext(ctx, `INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES (?,?,?,?,?,?,?,?)`, otherID, 1, "dispatch-2", "bound", "turn-dispatch-2", "initial_assignment", at, at); e != nil {
			return e
		}
		_, e := conn.ExecContext(ctx, "INSERT INTO journal (at,kind,subject,detail) VALUES (?,?,?,?)", at, "relationship_registered", otherID, `{"issueKey": "REL-2"}`)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	registered, err := s.One(context.Background(), "SELECT * FROM relationships WHERE relationship_id=?", otherID)
	if err != nil || registered == nil {
		t.Fatalf("second registration: %v", err)
	}
	generation, err := s.One(context.Background(), "SELECT * FROM generations WHERE relationship_id=?", otherID)
	if err != nil || generation == nil {
		t.Fatalf("second generation: %v", err)
	}
	other := set1RelationshipRecord(t, registered, generation)
	theirs := []any{otherID, "another-org/another-repo", mine["prNumber"]}
	message, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	set2Compare(t, map[string]any{"eventId": event, "mine": mine, "other": other, "keyMine": []any{mine["relationshipId"], mine["repository"], mine["prNumber"]}, "keyOther": theirs, "message": message}, want)
}

func set1RelationshipRecord(t *testing.T, r, g store.Row) map[string]any {
	t.Helper()
	parsed := func(column string) any {
		var value any
		if err := json.Unmarshal([]byte(r.Get(column).(string)), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	generation := map[string]any{"executionGeneration": g.Get("execution_generation"), "dispatchRequestId": g.Get("dispatch_request_id"), "anchorState": g.Get("anchor_state"), "dispatchTurnId": g.Get("dispatch_turn_id"), "openedAt": g.Get("opened_at"), "boundAt": g.Get("bound_at"), "reason": g.Get("reason")}
	return map[string]any{"relationshipId": r.Get("relationship_id"), "parent": map[string]any{"taskId": r.Get("parent_task_id"), "hostId": r.Get("parent_host_id"), "cwd": r.Get("parent_cwd")}, "child": map[string]any{"taskId": r.Get("child_task_id"), "hostId": r.Get("child_host_id"), "cwd": r.Get("child_cwd")}, "issueKey": r.Get("issue_key"), "status": r.Get("status"), "createdAt": r.Get("created_at"), "executionGeneration": r.Get("execution_generation"), "generations": []any{generation}, "authorizedScope": map[string]any{"artifactRoots": parsed("artifact_roots"), "allowedRecipients": parsed("allowed_recipients"), "scopeRef": r.Get("scope_ref")}, "_bindings": map[string]any{"parentCxcSession": r.Get("parent_cxc_session"), "childCxcSession": r.Get("child_cxc_session"), "supersededBy": r.Get("superseded_by")}, "supersededBy": r.Get("superseded_by")}
}

func Test24_RC_5_ReadyRequiredFields(t *testing.T) {
	want, s := pythonSet2(t, "RC-5-ready")
	fields := set1Fields()
	changes := []struct {
		name  string
		value any
	}{{"head_sha", nil}, {"summary", "   "}, {"next_action", "   "}, {"repository", "   "}, {"summary", map[string]any{"result": "done"}}, {"next_action", map[string]any{"result": "done"}}, {"repository", map[string]any{"result": "done"}}, {"cxc_reason", map[string]any{"result": "done"}}, {"pr_url", "something"}, {"pr_state", "something"}}
	got := []any{}
	for _, tc := range changes {
		input := map[string]any{}
		for k, v := range fields {
			input[k] = v
		}
		if tc.name == "pr_url" || tc.name == "pr_state" {
			input["pr_number"] = nil
			input["pr_url"] = nil
			input["pr_state"] = nil
			input["head_sha"] = nil
		}
		input[tc.name] = tc.value
		_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
		got = append(got, reportRefusalValue(err))
	}
	set2Compare(t, got, want["refusals"])
}

func Test24_RC_28_ReadyRestore(t *testing.T) {
	want, s := pythonSet2(t, "RC-28-ready")
	fields := set1Fields()
	fields["restore"] = map[string]any{"mode": "CXC Loop, HOTL", "phase": "C", "plan": "devlog/_plan/260916_jun131", "skills": []any{"loop", "pull-request"}}
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), fields)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.Preview(context.Background(), s, want["eventId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	set2Compare(t, map[string]any{"eventId": want["eventId"], "stored": stored, "message": message}, want)
}

func Test24_RC_27_TwoStageNoPRReport(t *testing.T) {
	want, s := pythonSet2(t, "RC-27-two-stages")
	event := want["eventId"].(string)
	firstFields := map[string]any{"repository": "thisisjun786/codex-relay-workflow", "cxc_status": "BUDGET_EXHAUSTED", "cxc_reason": "the stated token bound ran out", "summary": "delivery messages now lead with the pull request", "next_action": "review the diff and record a verdict", "base_ref": "dev", "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6", "criteria_digest": "d1e2f3", "evidence": []any{map[string]any{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}}, "unresolved": []any{"the CLI verb lands after PR 8 merges"}}
	first, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, firstFields)
	if err != nil {
		t.Fatal(err)
	}
	firstMessage, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	secondFields := map[string]any{}
	for k, v := range firstFields {
		secondFields[k] = v
	}
	secondFields["cxc_reason"] = "the bound ran out"
	secondFields["head_sha"] = strings.Repeat("f", 40)
	secondFields["base_sha"] = strings.Repeat("e", 40)
	second, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, secondFields)
	if err != nil {
		t.Fatal(err)
	}
	secondMessage, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	set2Compare(t, map[string]any{"eventId": event, "first": first, "firstMessage": firstMessage, "second": second, "secondMessage": secondMessage}, want)
}

func Test24_RC_24_26_ReadyPostRefusalAttempt(t *testing.T) {
	for _, id := range []string{"RC-24-ready", "RC-26-ready"} {
		t.Run(id, func(t *testing.T) {
			want, s := pythonSet2(t, id)
			event := want["eventId"].(string)
			fields := set1Fields()
			if id == "RC-24-ready" {
				fields["evidence"] = []any{1}
			} else {
				fields["summary"] = strings.Repeat("x", 4000)
			}
			_, rejected := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
			set2Compare(t, []any{reportRefusalValue(rejected)}, want["refusals"])
			stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, set1Fields())
			if err != nil {
				t.Fatal(err)
			}
			message, err := delivery.Preview(context.Background(), s, event)
			if err != nil {
				t.Fatal(err)
			}
			d := delivery.NewService(s, delivery.NewFakeClock())
			h := &reportScriptHost{sendHost: &sendHost{status: "idle"}}
			attempt, err := d.Attempt(context.Background(), event, h, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			set2Compare(t, map[string]any{"eventId": event, "refusals": want["refusals"], "stored": stored, "message": message, "attempt": deliveryObjMap(attempt)}, want)
		})
	}
}

func Test24_RC_18_19_20_22_RevisionContract(t *testing.T) {
	for _, id := range []string{"RC-18-revision", "RC-18-elision", "RC-18-preserve", "RC-19-revision", "RC-19-anchor", "RC-19-review-only", "RC-20-revision", "RC-22-revision"} {
		t.Run(id, func(t *testing.T) {
			want, s := pythonSet2(t, id)
			fields := map[string]any{"repository": "thisisjun786/codex-relay-workflow", "pr_number": 12, "pr_url": "https://github.com/thisisjun786/codex-relay-workflow/pull/12", "pr_state": "ready", "base_ref": "dev", "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6", "head_sha": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "criteria_digest": "d1e2f3", "cxc_status": "NEEDS_HUMAN", "cxc_reason": "manifest incomplete", "summary": "delivery messages now lead with the pull request", "next_action": "review the diff and record a verdict", "evidence": []any{map[string]any{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}}, "unresolved": []any{"the CLI verb lands after PR 8 merges"}}
			if id == "RC-18-revision" {
				fields["cxc_reason"] = "the parent judged the manifest incomplete"
				fields["summary"] = "add the migration script and re-submit"
				fields["next_action"] = "add the migration script to the manifest and emit generation 2"
				fields["review"] = map[string]any{"kind": "GO-WITH-FIXES", "blockers": 1, "findings": []any{map[string]any{"id": "c-1", "verdict": "needs_changes", "note": "the migration script is missing from the manifest", "anchor": "migrations/004_add_reports.sql"}}}
			}
			if id == "RC-19-revision" {
				fields["review"] = map[string]any{"kind": "FAIL", "findings": []any{map[string]any{"id": "c-9", "note": "separate review finding", "anchor": "migrations/004.sql"}}}
			}
			if id == "RC-19-anchor" || id == "RC-19-review-only" || id == "RC-18-elision" {
				findings := []any{map[string]any{"id": "c-1", "anchor": "migrations/004.sql"}}
				if id == "RC-19-review-only" {
					findings = []any{map[string]any{"id": "c-1", "verdict": "needs_changes", "note": "", "anchor": "migrations/004.sql"}, map[string]any{"id": "c-9", "verdict": "needs_changes", "note": "a reviewer noticed this separately"}}
				}
				blockers := 1
				if id == "RC-18-elision" {
					blockers = 2
					evidence := []any{}
					for n := 0; n < 30; n++ {
						evidence = append(evidence, map[string]any{"check": fmt.Sprintf("a long check name number %d that takes up room", n), "exitCode": 0})
					}
					fields["evidence"] = evidence
				}
				fields["review"] = map[string]any{"kind": "GO-WITH-FIXES", "blockers": blockers, "findings": findings}
			}
			if id == "RC-18-preserve" {
				unresolved := []any{}
				for n := 0; n < 40; n++ {
					unresolved = append(unresolved, fmt.Sprintf("open item %d with some length to it", n))
				}
				fields["unresolved"] = unresolved
			}
			if id == "RC-22-revision" {
				fields["review"] = map[string]any{"kind": "PASS"}
				_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), fields)
				compareReportValues(t, reportRefusalValue(err), want["refusal"])
				return
			}
			clock := delivery.NewFakeClock()
			clock.Advance(5)
			stored, err := RecordWorkReport(context.Background(), s, clock, want["eventId"].(string), fields)
			if err != nil {
				t.Errorf("first difference: record Go=%s Python=%s", jsonText(reportRefusalValue(err)), jsonText(want["stored"]))
				return
			}
			read, err := ReadWorkReport(context.Background(), s, want["eventId"].(string))
			if err != nil {
				t.Fatal(err)
			}
			message, err := delivery.Preview(context.Background(), s, want["eventId"].(string))
			if err != nil {
				t.Errorf("first difference: render Go=%v Python=%q", err, want["message"])
				return
			}
			got := map[string]any{"eventId": want["eventId"], "outcome": want["outcome"], "stored": stored, "read": read, "message": message}
			if budgets, ok := want["budgets"].(map[string]any); ok {
				gotBudgets := map[string]any{}
				service := delivery.NewService(s, clock)
				for key := range budgets {
					limit, err := strconv.Atoi(key)
					if err != nil {
						t.Fatal(err)
					}
					request := "del-y-a1"
					if key == "2650" {
						request = "del-p-a1"
					}
					text, err := service.PreviewReport(context.Background(), want["eventId"].(string), request, limit)
					if err != nil {
						t.Errorf("first difference: budget %d Go=%v Python=%q", limit, err, budgets[key])
						return
					}
					gotBudgets[key] = text
				}
				got["budgets"] = gotBudgets
			}
			set2Compare(t, got, want)
		})
	}
}

func Test24_RC_21_RevisionReviewRefusals(t *testing.T) {
	want, s := pythonSet2(t, "RC-21-revision-review")
	fields := map[string]any{"repository": "thisisjun786/codex-relay-workflow", "pr_number": 12, "pr_url": "https://github.com/thisisjun786/codex-relay-workflow/pull/12", "pr_state": "ready", "base_ref": "dev", "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6", "head_sha": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "criteria_digest": "d1e2f3", "cxc_status": "NEEDS_HUMAN", "cxc_reason": "manifest incomplete", "summary": "delivery messages now lead with the pull request", "next_action": "review the diff and record a verdict", "evidence": []any{map[string]any{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}}, "unresolved": []any{"the CLI verb lands after PR 8 merges"}}
	got := []any{}
	for _, value := range want["cases"].([]any) {
		input := map[string]any{}
		for k, v := range fields {
			input[k] = v
		}
		input["review"] = value
		clock := delivery.NewFakeClock()
		clock.Advance(5)
		_, err := RecordWorkReport(context.Background(), s, clock, want["eventId"].(string), input)
		got = append(got, reportRefusalValue(err))
	}
	compareReportValues(t, got, want["refusals"])
}

func Test24_RC_4_14_27_WholeRenderedReports(t *testing.T) {
	for _, id := range []string{"RC-4-record", "RC-4-isolation", "RC-14-report", "RC-19-unresolved", "RC-27-blocked"} {
		t.Run(id, func(t *testing.T) {
			want, s := pythonSet2(t, id)
			fields := map[string]any{"repository": "repo/project", "cxc_status": "BLOCKED", "cxc_reason": "waiting", "summary": "done", "next_action": "review"}
			if id == "RC-27-blocked" {
				fields["cxc_status"] = "BUDGET_EXHAUSTED"
				fields["cxc_reason"] = "the stated token bound ran out"
				fields["base_ref"] = "dev"
				fields["base_sha"] = "c56576d5be412b5bc352dd93b9eb37ab279a12f6"
				fields["head_sha"] = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
				fields["criteria_digest"] = "d1e2f3"
			} else {
				fields["pr_number"] = 12
				fields["head_sha"] = "a1b2c3"
			}
			if id == "RC-19-unresolved" {
				fields["unresolved"] = []any{map[string]any{"id": "c-1", "note": "still open"}}
			}
			stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), fields)
			if err != nil {
				t.Fatal(err)
			}
			message, err := delivery.Preview(context.Background(), s, want["eventId"].(string))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{"stored": stored, "eventId": want["eventId"], "version": reportVersion(true), "message": message, "ref": "", "key": nil, "cases": nil, "refusals": map[string]any{}}
			if fields["pr_number"] != nil {
				got["ref"] = reportPRRef("repo/project", 12)
				got["key"] = []any{stored["relationshipId"], "repo/project", 12}
			}
			if id == "RC-4-isolation" {
				got["otherRef"] = reportPRRef("another-org/another-repo", 12)
				got["otherKey"] = []any{"rel-different", "another-org/another-repo", 12}
			}
			set2Compare(t, got, want)
		})
	}
}

func Test24_RC_21_22_23_24_28_RecordRefusals(t *testing.T) {
	for _, id := range []string{"RC-21-review", "RC-22-direction", "RC-23-lines", "RC-24-entries", "RC-28-restore"} {
		t.Run(id, func(t *testing.T) {
			want, s := pythonSet2(t, id)
			fields := map[string]any{"repository": "repo/project", "cxc_status": "BLOCKED", "cxc_reason": "waiting", "summary": "done", "next_action": "review"}
			got := map[string]any{}
			for field, values := range want["cases"].(map[string]any) {
				for index, value := range values.([]any) {
					input := map[string]any{}
					for k, v := range fields {
						input[k] = v
					}
					input[field] = value
					_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
					got[fmt.Sprintf("%s-%d", field, index)] = reportRefusalValue(err)
				}
			}
			if !reflect.DeepEqual(got, want["refusals"]) {
				t.Errorf("Go=%s Python=%s", jsonText(got), jsonText(want["refusals"]))
			}
			if id == "RC-23-lines" {
				surrogates := map[string]any{}
				for _, field := range []string{"summary", "evidence", "unresolved"} {
					input := map[string]any{}
					for k, v := range fields {
						input[k] = v
					}
					if field == "summary" {
						input[field] = string([]byte{0xed, 0xa0, 0x80})
					} else {
						input[field] = []any{string([]byte{0xed, 0xa0, 0x80})}
					}
					_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
					surrogates[field] = reportRefusalValue(err)
				}
				compareReportValues(t, surrogates, want["surrogates"])
			}
			if id == "RC-23-lines" || id == "RC-24-entries" || id == "RC-28-restore" {
				input := map[string]any{}
				for k, v := range fields {
					input[k] = v
				}
				if id == "RC-24-entries" {
					input["evidence"] = []any{"pytest passed"}
				}
				if id == "RC-28-restore" {
					input["restore"] = map[string]any{"mode": "CXC Loop, HOTL", "phase": "C", "plan": "devlog/_plan/260916_jun131", "skills": []any{"loop", "pull-request"}}
				}
				stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
				if err != nil {
					t.Fatal(err)
				}
				key := "valid"
				if id == "RC-24-entries" {
					key = "tuple"
				}
				if id == "RC-28-restore" {
					key = "known"
				}
				compareReportValues(t, stored, want[key])
				message, err := delivery.Preview(context.Background(), s, want["eventId"].(string))
				if err != nil {
					t.Fatal(err)
				}
				compareReportValues(t, message, want["message"])
				if id == "RC-24-entries" {
					input["evidence"] = []any{map[string]any{"check": "ruff", "exitCode": 1}, map[string]any{"check": "pytest"}}
					stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
					if err != nil {
						t.Fatal(err)
					}
					compareReportValues(t, stored, want["exitCode"])
					message, err := delivery.Preview(context.Background(), s, want["eventId"].(string))
					if err != nil {
						t.Fatal(err)
					}
					compareReportValues(t, message, want["exitMessage"])
				}
				if id == "RC-28-restore" {
					input["restore"] = map[string]any{"mode": "   ", "scope": nil}
					stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
					if err != nil {
						t.Fatal(err)
					}
					compareReportValues(t, stored, want["blank"])
					message, err := delivery.Preview(context.Background(), s, want["eventId"].(string))
					if err != nil {
						t.Fatal(err)
					}
					compareReportValues(t, message, want["blankMessage"])
				}
			}
		})
	}
}

func Test24_RC_5_WholeRecordRefusals(t *testing.T) {
	want, s := pythonSet2(t, "RC-5-record")
	fields := map[string]any{"repository": "repo/project", "cxc_status": "BLOCKED", "cxc_reason": "waiting", "summary": "done", "next_action": "review", "pr_number": 12, "head_sha": "a1b2c3"}
	got := map[string]any{}
	for field, values := range want["cases"].(map[string]any) {
		for index, value := range values.([]any) {
			input := map[string]any{}
			for k, v := range fields {
				input[k] = v
			}
			input[field] = value
			if field == "pr_url" || field == "pr_state" {
				input["pr_number"] = nil
			}
			_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
			got[fmt.Sprintf("%s-%d", field, index)] = reportRefusalValue(err)
		}
	}
	if !reflect.DeepEqual(got, want["refusals"]) {
		t.Errorf("Go=%s Python=%s", jsonText(got), jsonText(want["refusals"]))
	}
}

func Test24_RC_26_LengthBoundaries(t *testing.T) {
	want, s := pythonSet2(t, "RC-26-lengths")
	fields := map[string]any{"repository": "repo/project", "cxc_status": "BLOCKED", "cxc_reason": "waiting", "summary": "done", "next_action": "review"}
	got := map[string]any{}
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"summary", []string{strings.Repeat("x", 4000)}}, {"next_action", []string{strings.Repeat("x", 4000)}},
		{"pr_state", []string{strings.Repeat("x", 10000)}},
		{"pr_url", []string{strings.Repeat("x", 10000), "https://x.invalid/" + strings.Repeat("x", 3000)}},
		{"base_ref", []string{strings.Repeat("x", 10000)}}, {"base_sha", []string{strings.Repeat("x", 10000)}},
		{"head_sha", []string{strings.Repeat("x", 10000)}}, {"criteria_digest", []string{strings.Repeat("x", 10000)}},
	} {
		for index, value := range tc.values {
			input := map[string]any{}
			for k, v := range fields {
				input[k] = v
			}
			input[tc.name] = value
			_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
			got[fmt.Sprintf("%s-%d", tc.name, index)] = reportRefusalValue(err)
		}
	}
	if !reflect.DeepEqual(got, want["refusals"]) {
		t.Errorf("Go=%s Python=%s", jsonText(got), jsonText(want["refusals"]))
	}
	input := map[string]any{}
	for k, v := range fields {
		input[k] = v
	}
	input["pr_number"] = 12
	input["head_sha"] = "a1b2c3"
	input["pr_url"] = "https://x.invalid/" + strings.Repeat("x", 406)
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err = json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, want["url"]) {
		t.Errorf("stored Go=%s Python=%s", jsonText(actual), jsonText(want["url"]))
	}
	message, err := delivery.Preview(context.Background(), s, want["eventId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, message, want["message"])
}

func Test24_RC_25_NumberBoundaries(t *testing.T) {
	want, s := pythonSet2(t, "RC-25-numbers")
	fields := map[string]any{"repository": "repo/project", "cxc_status": "BLOCKED", "cxc_reason": "waiting", "summary": "done", "next_action": "review"}
	got := map[string]any{}
	for _, tc := range []struct {
		name   string
		values []any
	}{
		{"pr_number", []any{uint64(1) << 63, json.Number("1" + strings.Repeat("0", 6000)), json.Number("-1" + strings.Repeat("0", 6000))}},
		{"submission_no", []any{uint64(1) << 63, true, 1.9, 0, -1, nil, "bad"}},
	} {
		for index, value := range tc.values {
			input := map[string]any{}
			for k, v := range fields {
				input[k] = v
			}
			input[tc.name] = value
			_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
			got[fmt.Sprintf("%s-%d", tc.name, index)] = reportRefusalValue(err)
		}
	}
	if !reflect.DeepEqual(got, want["refusals"]) {
		t.Errorf("Go=%s Python=%s", jsonText(got), jsonText(want["refusals"]))
	}
	input := map[string]any{}
	for k, v := range fields {
		input[k] = v
	}
	input["pr_number"] = int64(1<<63 - 1)
	input["head_sha"] = "a1b2c3"
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), want["eventId"].(string), input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err = json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, want["max"]) {
		t.Errorf("stored Go=%s Python=%s", jsonText(actual), jsonText(want["max"]))
	}
}

func Test24_RC_14_LegacyWholeMessage(t *testing.T) {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/report_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	db := filepath.Join(root, "relay.sqlite3")
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, "RC-14-legacy", root)
	cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "XDG_DATA_HOME="+root, "XDG_CONFIG_HOME="+root, "CODEX_HOME="+root, "TMPDIR="+os.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python: %v: %s", err, output)
	}
	var want struct {
		Message string `json:"message"`
		Version string `json:"version"`
		EventID string `json:"eventId"`
	}
	if err := json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	if got := reportVersion(false); got != want.Version {
		t.Errorf("version Go %q Python %q", got, want.Version)
	}
	ownCopied(t, db, "go")
	s, err := store.Open(context.Background(), db, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, err := delivery.Preview(context.Background(), s, want.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if got != want.Message {
		t.Errorf("legacy message Go=%q Python=%q", got, want.Message)
	}
}
