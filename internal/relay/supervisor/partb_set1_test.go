package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func set1Capture(t *testing.T, id string) (map[string]any, *store.Store) {
	t.Helper()
	root := t.TempDir()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/report_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	// The store Python's fixture left is recorded with the answer (pythonTree).
	output := pythonTree(t, id, root, func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, id, root)
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root, "TMPDIR="+os.TempDir())
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("capture: %v: %s", err, output)
		}
		return output, nil
	})
	var want map[string]any
	if err := json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	ownCopied(t, filepath.Join(root, "relay.sqlite3"), "go")
	s, err := store.Open(context.Background(), filepath.Join(root, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return want, s
}

func set1Fields() map[string]any {
	fields := map[string]any{"repository": "thisisjun786/codex-relay-workflow", "pr_number": 12, "pr_url": "https://github.com/thisisjun786/codex-relay-workflow/pull/12", "pr_state": "ready", "base_ref": "dev", "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6", "head_sha": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "criteria_digest": "d1e2f3", "cxc_status": "DONE", "cxc_reason": "every recorded criterion has fresh proof on this head", "summary": "delivery messages now lead with the pull request", "next_action": "review the diff and record a verdict", "evidence": []any{map[string]any{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}}, "unresolved": []any{"the CLI verb lands after PR 8 merges"}}
	fields["handoff"] = map[string]any{"isDraft": false, "baseVerifiedAt": "2026-09-20T09:00:00Z", "requiredDeclared": []any{"dev-gate"}, "checks": []any{map[string]any{"runId": "run-dev-gate", "name": "dev-gate", "headSha": fields["head_sha"], "conclusion": "success", "attempt": 1}}, "reviewCoverage": map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 1, "threadsSeen": []any{"PRRT_ready"}, "unresolved": 0}, "threadDispositions": []any{map[string]any{"threadId": "PRRT_ready", "disposition": "fixed", "evidence": "addressed and rechecked on this head", "addressedBy": "a1b2c3d"}}, "criterionEvidence": []any{}, "limitations": []any{}}
	return fields
}

func Test24_RC_2_LiveHumanStatusStorage(t *testing.T) {
	want, s := set1Capture(t, "RC-2-human-stored")
	event := want["eventId"].(string)
	statuses := map[string]any{}
	for index, status := range []string{"BLOCKED", "UNSAFE", "NEEDS_HUMAN"} {
		fields := map[string]any{"repository": "repo/project", "cxc_status": status, "cxc_reason": "reason for " + status, "summary": "stopped", "next_action": "ask the parent", "submission_no": index + 1}
		stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
		if err != nil {
			t.Fatal(err)
		}
		read, err := ReadWorkReport(context.Background(), s, event)
		if err != nil {
			t.Fatal(err)
		}
		statuses[status] = map[string]any{"stored": stored, "read": read}
	}
	set2Compare(t, map[string]any{"eventId": event, "statuses": statuses}, want)
}

func Test24_RC_1_2_3_LiveWholeOutput(t *testing.T) {
	for _, id := range []string{"RC-1-ready", "RC-2-ready", "RC-3-ready"} {
		t.Run(id, func(t *testing.T) {
			want, s := set1Capture(t, id)
			event := want["eventId"].(string)
			stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, set1Fields())
			if err != nil {
				t.Fatal(err)
			}
			read, err := ReadWorkReport(context.Background(), s, event)
			if err != nil {
				t.Fatal(err)
			}
			message, err := delivery.Preview(context.Background(), s, event)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{"eventId": event, "stored": stored, "read": read, "message": message}
			switch id {
			case "RC-1-ready":
				promotion := map[string]any{}
				for _, fact := range []string{"cxc_done", "pull_request_opened", "review_pass", "required_checks_green"} {
					promotion[fact] = nonVerification[fact]
				}
				got["promotion"] = promotion
				rows, err := s.All(context.Background(), "SELECT * FROM verdicts WHERE event_id=?", event)
				if err != nil {
					t.Fatal(err)
				}
				if rows == nil {
					got["verdictRows"] = []any{}
				} else {
					got["verdictRows"] = rows
				}
			case "RC-2-ready":
				got["unknown"] = reportRefusalValue(checkReportStatus("SHIPPED", "ready_for_review"))
				got["contradiction"] = reportRefusalValue(checkReportStatus("BLOCKED", "ready_for_review"))
				human := map[string]any{}
				for _, status := range []string{"BLOCKED", "UNSAFE", "NEEDS_HUMAN"} {
					human[status] = map[string]any{"outcomes": reportOutcomes[status], "meaning": reportMeanings[status]}
				}
				got["human"] = human
			case "RC-3-ready":
				head := stored["headSha"].(string)
				got["head"] = reportRefusalValue(assertReportCurrent(head, 1, strings.Repeat("9", 40), 1))
				got["generation"] = reportRefusalValue(assertReportCurrent(head, 1, "", 99))
				got["current"] = reportRefusalValue(assertReportCurrent(head, 1, head, 1))
			}
			set2Compare(t, got, want)
		})
	}
}

func Test24_RC_2_3_LivePureRefusalOutputs(t *testing.T) {
	for _, id := range []string{"RC-2-status-refusals", "RC-3-current-refusals"} {
		t.Run(id, func(t *testing.T) {
			want := pythonReportCapture(t, id)
			got := map[string]any{}
			if id == "RC-2-status-refusals" {
				got["unknown"] = reportRefusalValue(checkReportStatus("SHIPPED", "ready_for_review"))
				got["contradiction"] = reportRefusalValue(checkReportStatus("BLOCKED", "ready_for_review"))
				compatible := map[string]any{}
				meanings := map[string]any{}
				for _, status := range []string{"BLOCKED", "UNSAFE", "NEEDS_HUMAN"} {
					compatible[status] = reportOutcomes[status]
					meanings[status] = reportMeanings[status]
				}
				got["compatible"] = compatible
				got["meanings"] = meanings
			} else {
				head := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
				got["head"] = reportRefusalValue(assertReportCurrent(head, 1, strings.Repeat("9", 40), 1))
				got["generation"] = reportRefusalValue(assertReportCurrent(head, 1, "", 99))
				got["current"] = reportRefusalValue(assertReportCurrent(head, 1, head, 1))
			}
			set2Compare(t, got, want)
		})
	}
}

func Test24_RC_6_LiveMissingEvent(t *testing.T) {
	want, s := set1Capture(t, "RC-6-missing-event")
	_, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), strings.Repeat("0", 32), set1Fields())
	set2Compare(t, map[string]any{"missing": reportRefusalValue(err)}, want)
}

func Test24_RC_6_LiveReportIdentity(t *testing.T) {
	want, s := set1Capture(t, "RC-6-record")
	event := want["eventId"].(string)
	fields := map[string]any{
		"repository": "thisisjun786/codex-relay-workflow", "pr_number": 12, "pr_url": "https://github.com/thisisjun786/codex-relay-workflow/pull/12", "pr_state": "ready", "base_ref": "dev", "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6", "head_sha": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "criteria_digest": "d1e2f3", "cxc_status": "DONE", "cxc_reason": "every recorded criterion has fresh proof on this head", "summary": "delivery messages now lead with the pull request", "next_action": "review the diff and record a verdict",
		"evidence": []any{map[string]any{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}}, "unresolved": []any{"the CLI verb lands after PR 8 merges"},
	}
	fields["handoff"] = map[string]any{"isDraft": false, "baseVerifiedAt": "2026-09-20T09:00:00Z", "requiredDeclared": []any{"dev-gate"}, "checks": []any{map[string]any{"runId": "run-dev-gate", "name": "dev-gate", "headSha": fields["head_sha"], "conclusion": "success", "attempt": 1}}, "reviewCoverage": map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 1, "threadsSeen": []any{"PRRT_ready"}, "unresolved": 0}, "threadDispositions": []any{map[string]any{"threadId": "PRRT_ready", "disposition": "fixed", "evidence": "addressed and rechecked on this head", "addressedBy": "a1b2c3d"}}, "criterionEvidence": []any{}, "limitations": []any{}}
	_, missing := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), "00000000000000000000000000000000", fields)
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadWorkReport(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.NewService(s, delivery.NewFakeClock()).PreviewMessage(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"missing": reportRefusalValue(missing), "stored": stored, "read": read, "eventId": event, "message": message}, want)
}

func Test24_RC_8_LiveBoundaryVariants(t *testing.T) {
	for _, id := range []string{"RC-8-largest", "RC-8-worst", "RC-8-checks", "RC-8-none"} {
		t.Run(id, func(t *testing.T) {
			want, s := set1Capture(t, id)
			event := want["eventId"].(string)
			rawFields, err := json.Marshal(want["fields"])
			if err != nil {
				t.Fatal(err)
			}
			var captured map[string]any
			if err = json.Unmarshal(rawFields, &captured); err != nil {
				t.Fatal(err)
			}
			fields := set1CapturedFields(captured)
			stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
			if err != nil {
				t.Fatalf("record: %v", err)
			}
			message, err := delivery.Preview(context.Background(), s, event)
			if err != nil {
				t.Fatal(err)
			}
			set2Compare(t, map[string]any{"eventId": event, "fields": want["fields"], "stored": stored, "message": message}, want)
		})
	}
}

func Test24_RC_9_LiveConfirmationGate(t *testing.T) {
	want, s := set1Capture(t, "RC-9-too-many")
	event := want["eventId"].(string)
	huge := set1CapturedFields(want["huge"].(map[string]any))
	ordinary := set1CapturedFields(want["ordinary"].(map[string]any))
	_, rejected := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, huge)
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "huge": want["huge"], "ordinary": want["ordinary"], "rejected": reportRefusalValue(rejected), "stored": stored, "message": message}, want)
}

func set1CapturedFields(fields map[string]any) map[string]any {
	copied := map[string]any{}
	for k, v := range fields {
		copied[k] = v
	}
	copied["pr_number"] = 12
	for _, raw := range copied["evidence"].([]any) {
		item := raw.(map[string]any)
		item["exitCode"] = int(item["exitCode"].(float64))
	}
	h, hasHandoff := copied["handoff"].(map[string]any)
	if !hasHandoff {
		return copied
	}
	coverage := h["reviewCoverage"].(map[string]any)
	for _, key := range []string{"pagesRead", "totalCount", "unresolved"} {
		coverage[key] = int(coverage[key].(float64))
	}
	for _, raw := range h["checks"].([]any) {
		item := raw.(map[string]any)
		item["attempt"] = int(item["attempt"].(float64))
	}
	return copied
}

func Test24_RC_8_LiveAcceptedConfirmations(t *testing.T) {
	want, s := set1Capture(t, "RC-8-accepted")
	event := want["eventId"].(string)
	fields := map[string]any{}
	for k, v := range want["fields"].(map[string]any) {
		fields[k] = v
	}
	// The capture's JSON numbers arrive as float64; the public recorder accepts integer
	// check attempts and coverage counts supplied by the original Python caller.
	fields["pr_number"] = 12
	for _, raw := range fields["evidence"].([]any) {
		item := raw.(map[string]any)
		item["exitCode"] = int(item["exitCode"].(float64))
	}
	h := fields["handoff"].(map[string]any)
	coverage := h["reviewCoverage"].(map[string]any)
	for _, key := range []string{"pagesRead", "totalCount", "unresolved"} {
		coverage[key] = int(coverage[key].(float64))
	}
	checks := h["checks"].([]any)
	for _, raw := range checks {
		item := raw.(map[string]any)
		if n, ok := item["attempt"].(float64); ok {
			item["attempt"] = int(n)
		}
	}
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	d := delivery.NewService(s, delivery.NewFakeClock())
	messages := map[string]any{}
	for _, budget := range []int{4000, 6000} {
		text, err := d.PreviewReport(context.Background(), event, "del-x-a1", budget)
		if err != nil {
			t.Fatal(err)
		}
		messages[fmt.Sprint(budget)] = text
	}
	_, impossible := d.PreviewReport(context.Background(), event, "del-x-a1", 1700)
	impossibleText := any(nil)
	if impossible != nil {
		impossibleText = impossible.Error()
	}
	compareReportValues(t, map[string]any{"eventId": event, "fields": want["fields"], "stored": stored, "messages": messages, "impossible": impossibleText}, want)
}

func Test24_RC_7_LiveKoreanBudget(t *testing.T) {
	want, s := set1Capture(t, "RC-7-korean-budget")
	event := want["eventId"].(string)
	fields := set1Fields()
	fields["summary"] = strings.Repeat("전달 메시지가 풀리퀘스트를 먼저 말하도록 바꿉니다. ", 12)
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.NewService(s, delivery.NewFakeClock()).PreviewReport(context.Background(), event, "del-z-a1", 2500)
	if err != nil {
		t.Fatal(err)
	}
	set2Compare(t, map[string]any{"eventId": event, "stored": stored, "message": message}, want)
}

func Test24_RC_7_LiveUnicode(t *testing.T) {
	want, s := set1Capture(t, "RC-7-unicode")
	event := want["eventId"].(string)
	fields := set1Fields()
	fields["summary"] = "검토 준비 완료"
	items := make([]any, 150)
	for n := range items {
		items[n] = fmt.Sprintf("한글 결과 %d - 재검토 필요", n)
	}
	fields["unresolved"] = items
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.NewService(s, delivery.NewFakeClock()).PreviewReport(context.Background(), event, "del-x-a1", 2500)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "stored": stored, "message": message}, want)
}

func Test24_RC_7_LiveStress(t *testing.T) {
	want, s := set1Capture(t, "RC-7-stress")
	event := want["eventId"].(string)
	fields := set1Fields()
	items := make([]any, 2000)
	for n := range items {
		items[n] = fmt.Sprintf("finding %d with proof still needed", n)
	}
	fields["unresolved"] = items
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	d := delivery.NewService(s, delivery.NewFakeClock())
	messages := map[string]any{}
	for _, budget := range []int{1700, 2500, 4000} {
		message, err := d.PreviewReport(context.Background(), event, "del-x-a1", budget)
		if err != nil {
			t.Fatal(err)
		}
		messages[fmt.Sprint(budget)] = message
	}
	_, impossible := d.PreviewReport(context.Background(), event, "del-x-a1", 120)
	var refusal any
	if impossible != nil {
		refusal = impossible.Error()
	}
	compareReportValues(t, map[string]any{"eventId": event, "stored": stored, "messages": messages, "impossible": refusal}, want)
}

func Test24_RC_7_LiveElision(t *testing.T) {
	want, s := set1Capture(t, "RC-7-elision")
	event := want["eventId"].(string)
	fields := map[string]any{"repository": "thisisjun786/codex-relay-workflow", "pr_number": 12, "pr_url": "https://github.com/thisisjun786/codex-relay-workflow/pull/12", "pr_state": "ready", "base_ref": "dev", "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6", "head_sha": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "criteria_digest": "d1e2f3", "cxc_status": "DONE", "cxc_reason": "every recorded criterion has fresh proof on this head", "summary": "delivery messages now lead with the pull request", "next_action": "review the diff and record a verdict", "evidence": []any{map[string]any{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}}}
	items := make([]any, 24)
	for n := range items {
		items[n] = fmt.Sprintf("finding %d: something specific that still needs doing", n)
	}
	fields["unresolved"] = items
	fields["handoff"] = map[string]any{"isDraft": false, "baseVerifiedAt": "2026-09-20T09:00:00Z", "requiredDeclared": []any{"dev-gate"}, "checks": []any{map[string]any{"runId": "run-dev-gate", "name": "dev-gate", "headSha": fields["head_sha"], "conclusion": "success", "attempt": 1}}, "reviewCoverage": map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 1, "threadsSeen": []any{"PRRT_ready"}, "unresolved": 0}, "threadDispositions": []any{map[string]any{"threadId": "PRRT_ready", "disposition": "fixed", "evidence": "addressed and rechecked on this head", "addressedBy": "a1b2c3d"}}, "criterionEvidence": []any{}, "limitations": []any{}}
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	d := delivery.NewService(s, delivery.NewFakeClock())
	defaultMessage, err := d.PreviewMessage(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	tight, err := d.PreviewReport(context.Background(), event, "del-x-a1", 1700)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "stored": stored, "default": defaultMessage, "tight": tight}, want)
}

func Test24_RC_10_LiveManifest(t *testing.T) {
	want, s := set1Capture(t, "RC-10-manifest")
	// The Python capture records after enqueue but before report.record; use Python's
	// own saved report for this renderer-only case, so no caller-supplied field differs.
	event := want["eventId"].(string)
	row, err := s.One(context.Background(), "SELECT receipt FROM events WHERE event_id = ?", event)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err = json.Unmarshal([]byte(row.Get("receipt").(string)), &receipt); err != nil {
		t.Fatal(err)
	}
	reportFields := map[string]any{"repository": "thisisjun786/codex-relay-workflow", "pr_number": 12, "pr_url": "https://github.com/thisisjun786/codex-relay-workflow/pull/12", "pr_state": "ready", "base_ref": "dev", "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6", "head_sha": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "criteria_digest": "d1e2f3", "cxc_status": "DONE", "cxc_reason": "every recorded criterion has fresh proof on this head", "summary": "delivery messages now lead with the pull request", "next_action": "review the diff and record a verdict", "evidence": []any{map[string]any{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}}, "unresolved": []any{"the CLI verb lands after PR 8 merges"}}
	reportFields["handoff"] = map[string]any{"isDraft": false, "baseVerifiedAt": "2026-09-20T09:00:00Z", "requiredDeclared": []any{"dev-gate"}, "checks": []any{map[string]any{"runId": "run-dev-gate", "name": "dev-gate", "headSha": reportFields["head_sha"], "conclusion": "success", "attempt": 1}}, "reviewCoverage": map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 1, "threadsSeen": []any{"PRRT_ready"}, "unresolved": 0}, "threadDispositions": []any{map[string]any{"threadId": "PRRT_ready", "disposition": "fixed", "evidence": "addressed and rechecked on this head", "addressedBy": "a1b2c3d"}}, "criterionEvidence": []any{}, "limitations": []any{}}
	if _, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, reportFields); err != nil {
		t.Fatal(err)
	}
	d := delivery.NewService(s, delivery.NewFakeClock())
	got := map[string]any{"eventId": event}
	for _, test := range []struct {
		key, ref string
		budget   int
	}{{"plain", "/var/lib/relay/frozen/abc123", 6000}, {"tight", "/var/lib/relay/frozen/abc123", 1700}, {"long", "/var/lib/relay/frozen/" + strings.Repeat("x", 9000), 6000}} {
		receipt["manifestRef"] = test.ref
		encoded, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec("UPDATE events SET receipt=? WHERE event_id=?", string(encoded), event); err != nil {
			t.Fatal(err)
		}
		message, err := d.PreviewReport(context.Background(), event, "del-x-a1", test.budget)
		if err != nil {
			t.Fatal(err)
		}
		got[test.key] = message
	}
	compareReportValues(t, got, want)
}

func Test24_RC_10_LiveUnencodableManifest(t *testing.T) {
	want, s := set1Capture(t, "RC-10-surrogate")
	event := want["eventId"].(string)
	if _, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, set1Fields()); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.One(context.Background(), "SELECT receipt FROM events WHERE event_id=?", event)
	if err != nil {
		t.Fatal(err)
	}
	raw := receipt.Get("receipt").(string)
	// Python's raw receipt has a lone surrogate. The Go JSON boundary replaces it
	// with U+FFFD; compare the actual rendered bytes rather than asserting a guess.
	var source map[string]any
	if err = json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	source["manifestRef"] = "/frozen/\\ud800"
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE events SET receipt=? WHERE event_id=?", string(encoded), event); err != nil {
		t.Fatal(err)
	}
	message, err := delivery.NewService(s, delivery.NewFakeClock()).PreviewReport(context.Background(), event, "del-u-a1", 6000)
	got := any(message)
	if err != nil {
		got = err.Error()
	}
	set2Compare(t, map[string]any{"eventId": event, "message": got}, want)
}

type reportScriptHost struct {
	*sendHost
	script string
}

func (h *reportScriptHost) ListTurnIDs(string, int) ([]any, error) { return []any{}, nil }

func (h *reportScriptHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	if h.script == "busy" {
		return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "failed"}, {Key: "threadId", Value: thread}, {Key: "retrySafe", Value: false}, {Key: "error", Value: "thread/read: Thread is active; message withheld. Wait for completion."}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "Thread is active"}}}}, nil
	}
	if h.script == "approval_policy" {
		return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "failed"}, {Key: "threadId", Value: thread}, {Key: "retrySafe", Value: false}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "error", Value: "thread/resume: Interactive approvals unsupported; message withheld."}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "unsupported_approval_policy"}, {Key: "message", Value: "unsupported"}}}}, nil
	}
	h.sends = append(h.sends, message)
	return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "accepted"}, {Key: "threadId", Value: thread}, {Key: "turnId", Value: "turn-" + thread + "-1"}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "retrySafe", Value: false}}, nil
}
func deliveryObjMap(row delivery.Obj) any {
	if row == nil {
		return nil
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return err.Error()
	}
	var fields []struct {
		Key   string
		Value any
	}
	if err = json.Unmarshal(raw, &fields); err != nil {
		return err.Error()
	}
	out := map[string]any{}
	for _, f := range fields {
		out[f.Key] = f.Value
	}
	return out
}

func Test24_RC_11_LiveFrozenSubmission(t *testing.T) {
	for _, name := range []string{"RC-11-never-frozen", "RC-11-between"} {
		t.Run(name, func(t *testing.T) {
			want, s := set1Capture(t, name)
			event := want["eventId"].(string)
			d := delivery.NewService(s, delivery.NewFakeClock())
			host := &reportScriptHost{sendHost: &sendHost{status: "idle"}}
			attempt, err := d.Attempt(context.Background(), event, host, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			fields := set1Fields()
			var corrected any
			if name == "RC-11-never-frozen" {
				fields["summary"] = "second"
				fields["submission_no"] = 2
			} else {
				fields["summary"] = "third"
				fields["submission_no"] = 3
			}
			newest, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
			if err != nil {
				t.Fatal(err)
			}
			if name == "RC-11-never-frozen" {
				fields = set1Fields()
				fields["summary"] = "second, corrected"
				fields["submission_no"] = 2
				corrected, err = RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			fields = set1Fields()
			if name == "RC-11-never-frozen" {
				fields["summary"] = "rewriting history"
				fields["submission_no"] = 1
			} else {
				fields["summary"] = "second, invisible"
				fields["submission_no"] = 2
			}
			_, older := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
			history, err := ReadWorkReports(context.Background(), s, event)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := d.PreviewMessage(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			compareReportValues(t, map[string]any{"eventId": event, "first": want["first"], "attempt": deliveryObjMap(attempt), "newest": newest, "corrected": corrected, "older": reportRefusalValue(older), "history": history, "preview": preview}, want)
		})
	}
}

func Test24_RC_11_LiveAttemptPaths(t *testing.T) {
	for _, tc := range []struct{ name, script string }{{"RC-11-busy", "busy"}, {"RC-11-inbox", "approval_policy"}, {"RC-11-legacy", ""}, {"RC-11-dispatched", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			want, s := set1Capture(t, tc.name)
			event := want["eventId"].(string)
			d := delivery.NewService(s, delivery.NewFakeClock())
			host := &reportScriptHost{sendHost: &sendHost{status: "idle"}, script: tc.script}
			var attempt any
			record, err := d.Attempt(context.Background(), event, host, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			attempt = deliveryObjMap(record)
			var first any
			if tc.name != "RC-11-legacy" {
				first, err = ReadWorkReport(context.Background(), s, event)
				if err != nil {
					t.Fatal(err)
				}
			}
			var updated any
			var rejected any
			if tc.name == "RC-11-busy" {
				fields := set1Fields()
				fields["summary"] = "corrected before send"
				updated, err = RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				fields := set1Fields()
				fields["summary"] = "quietly different now"
				_, failure := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
				rejected = reportRefusalValue(failure)
			}
			fields := set1Fields()
			fields["summary"] = "openly revised"
			fields["submission_no"] = 2
			second, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
			if err != nil {
				t.Fatal(err)
			}
			history, err := ReadWorkReports(context.Background(), s, event)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := d.PreviewMessage(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			compareReportValues(t, map[string]any{"eventId": event, "attempt": attempt, "first": first, "updated": updated, "rejected": rejected, "second": second, "history": history, "preview": preview}, want)
		})
	}
}

func Test24_RC_11_LiveSubmissions(t *testing.T) {
	want, s := set1Capture(t, "RC-11-submissions")
	event := want["eventId"].(string)
	first, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, set1Fields())
	if err != nil {
		t.Fatal(err)
	}
	fields := set1Fields()
	fields["summary"] = "corrected before send"
	correction, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	fields = set1Fields()
	fields["summary"] = "second submission"
	fields["submission_no"] = 2
	newest, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	fields = set1Fields()
	fields["summary"] = "older submission"
	fields["submission_no"] = 1
	_, older := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	history, err := ReadWorkReports(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "first": first, "correction": correction, "newest": newest, "older": reportRefusalValue(older), "history": history, "message": message}, want)
}

func Test24_RC_12_LiveOmissionToShow(t *testing.T) {
	want, s := set1Capture(t, "RC-12-omission-show")
	event := want["eventId"].(string)
	fields := set1Fields()
	items := make([]any, 30)
	for n := range items {
		items[n] = fmt.Sprintf("finding %d: something that still needs doing", n)
	}
	fields["unresolved"] = items
	stored, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	tight, err := delivery.NewService(s, delivery.NewFakeClock()).PreviewReport(context.Background(), event, "del-r-a1", 1700)
	if err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	payload := set1ShowBinary(t, path, event, false)
	compareReportValues(t, map[string]any{"eventId": event, "stored": stored, "tight": tight, "show": payload}, want)
}

func set1ShowBinary(t *testing.T, path, event string, message bool) any {
	t.Helper()
	binary := supervisorBinary(t)
	args := []string{"relay", "--state", filepath.Dir(path), "show", "--event", event}
	if message {
		args = append(args, "--message")
	}
	command := exec.Command(binary, args...)
	command.Env = append(os.Environ(), "HOME="+filepath.Dir(path), "XDG_STATE_HOME="+filepath.Dir(path), "CODEX_HOME="+filepath.Dir(path))
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("show: %v stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
	var got any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func Test24_RC_12_LiveDeliveredHistory(t *testing.T) {
	want, s := set1Capture(t, "RC-12-delivered-history")
	event := want["eventId"].(string)
	d := delivery.NewService(s, delivery.NewFakeClock())
	host := &reportScriptHost{sendHost: &sendHost{status: "idle"}}
	attempt, err := d.Attempt(context.Background(), event, host, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	fields := set1Fields()
	fields["unresolved"] = []any{"only this now"}
	fields["summary"] = "second submission"
	fields["submission_no"] = 2
	second, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	history, err := ReadWorkReports(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	show := set1ShowBinary(t, path, event, false)
	set2Compare(t, map[string]any{"eventId": event, "first": want["first"], "attempt": deliveryObjMap(attempt), "second": second, "history": history, "show": show}, want)
}

func Test24_RC_12_LiveShowMessage(t *testing.T) {
	want, s := set1Capture(t, "RC-12-show")
	event := want["eventId"].(string)
	if _, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, set1Fields()); err != nil {
		t.Fatal(err)
	}
	// The command opens its own connection; close the snapshot reader first.
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	binary := supervisorBinary(t)
	command := exec.Command(binary, "relay", "--state", filepath.Dir(path), "show", "--event", event, "--message")
	command.Env = append(os.Environ(), "HOME="+filepath.Dir(path), "XDG_STATE_HOME="+filepath.Dir(path), "CODEX_HOME="+filepath.Dir(path))
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("show: %v stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
	var got any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "payload": got}, want)
}

func Test24_RC_12_LiveHistory(t *testing.T) {
	want, s := set1Capture(t, "RC-12-history")
	event := want["eventId"].(string)
	fields := set1Fields()
	items := make([]any, 30)
	for n := range items {
		items[n] = fmt.Sprintf("finding %d: something that still needs doing", n)
	}
	fields["unresolved"] = items
	first, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	fields = set1Fields()
	fields["summary"] = "openly revised"
	fields["submission_no"] = 2
	second, err := RecordWorkReport(context.Background(), s, delivery.NewFakeClock(), event, fields)
	if err != nil {
		t.Fatal(err)
	}
	history, err := ReadWorkReports(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	message, err := delivery.Preview(context.Background(), s, event)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "first": first, "second": second, "history": history, "message": message}, want)
}

func Test24_RC_13_LivePreview(t *testing.T) {
	want, s := set1Capture(t, "RC-13-preview")
	event := want["eventId"].(string)
	d := delivery.NewService(s, delivery.NewFakeClock())
	before, err := d.PreviewMessage(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	host := &reportScriptHost{sendHost: &sendHost{status: "idle"}}
	record, err := d.Attempt(context.Background(), event, host, nil, "")
	if err != nil {
		t.Fatalf("actual send: %v", err)
	}
	if record == nil {
		t.Fatal("actual send returned no attempt")
	}
	var request any
	for _, field := range record {
		if field.Key == "requestId" {
			request = field.Value
			break
		}
	}
	if request == nil {
		t.Fatalf("send lacks requestId: %v", record)
	}
	frozen, err := s.One(context.Background(), "SELECT message FROM attempt_messages WHERE request_id=?", request)
	if err != nil || frozen == nil {
		t.Fatalf("frozen attempt: %v %v", frozen, err)
	}
	after, err := d.PreviewMessage(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	compareReportValues(t, map[string]any{"eventId": event, "requestId": request, "before": before, "sent": frozen.Get("message"), "after": after}, want)
}

func compareReportValues(t *testing.T, got, want any) {
	t.Helper()
	bytes, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err = json.Unmarshal(bytes, &normalized); err != nil {
		t.Fatal(err)
	}
	if jsonText(normalized) != jsonText(want) {
		t.Errorf("Go=%s\nPython=%s", jsonText(normalized), jsonText(want))
	}
}
