//go:build dev

package stopevents

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func refusedInvocation(t *testing.T, h *host, reason string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(h.at(loadFixture(t), 0), &payload); err != nil {
		t.Fatal(err)
	}
	switch reason {
	case hook.IdentityFieldsIncomplete:
		delete(payload, "last_assistant_message")
	case hook.TranscriptPathMissing:
		delete(payload, "transcript_path")
	case "transcript_line_unreadable":
		f, err := os.OpenFile(h.transcript, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString("{\"turn_id\":\"" + payload["turn_id"].(string) + "\",\"broken\":\n{}\n")
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal(writeErr, closeErr)
		}
	default:
		t.Fatalf("unknown refusal %q", reason)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	before := len(h.rowPaths(h.journal))
	h.run(h.settings, raw)
	rows := h.rowPaths(h.journal)
	if len(rows) != before+1 {
		t.Fatalf("refusal did not write one row: %v", rows)
	}
	for _, row := range rows {
		if valueAt(t, row, "acceptance") == hook.Unestablished {
			return row
		}
	}
	t.Fatal("no refused invocation")
	return ""
}

func exclusionReports(t *testing.T, answer map[string]any, want int) []any {
	t.Helper()
	items, ok := answer["excludedInvocations"].([]any)
	if !ok || len(items) != want {
		t.Fatalf("excludedInvocations = %v, want %d named reports", answer["excludedInvocations"], want)
	}
	return items
}

func TestExcludedInvocationsNameEachIdentityRefusal(t *testing.T) {
	for _, reason := range []string{hook.IdentityFieldsIncomplete, hook.TranscriptPathMissing, "transcript_line_unreadable"} {
		t.Run(reason, func(t *testing.T) {
			h := newHost(t, hook.Release)
			path := refusedInvocation(t, h, reason)
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			report := exclusionReports(t, answer, 1)[0].(map[string]any)
			if report["row"] != path || report["reason"] != "unestablished:"+reason || report["excludedFrom"] != "per_event_acceptance_count" || report["preventsTrue"] != true {
				t.Fatal(report)
			}
			for _, field := range []string{"at", "sessionId", "turnId"} {
				if report[field] != valueAt(t, path, field) {
					t.Fatalf("%s does not identify the invocation: %v", field, report)
				}
			}
			evidence := report["evidence"].(map[string]any)
			if evidence["acceptance"] != hook.Unestablished || evidence["eventKey"] != nil || evidence["acceptedAs"] != nil || evidence["guardInvoked"] != true || evidence["guardDecision"] != hook.Release || evidence["held"] != false || evidence["adapterOutcome"] != hook.GuardAnswered {
				t.Fatal(evidence)
			}
			identity := evidence["eventIdentity"].(map[string]any)
			if identity["reason"] != reason || identity["established"] != false || identity["answerItem"] != nil {
				t.Fatal(identity)
			}
			if reason == "transcript_line_unreadable" {
				if identity["transcriptPath"] != h.transcript || identity["scannedLines"].(float64) == 0 {
					t.Fatal(identity)
				}
			} else if identity["transcriptPath"] != nil || identity["scannedBytes"] != 0.0 || identity["scannedLines"] != 0.0 {
				t.Fatal(identity)
			}
		})
	}
}

func TestExcludedInvocationsKeepRepeatedRowsAndWindowFilters(t *testing.T) {
	h := newHost(t, hook.Release)
	for range 2 {
		h.run(h.settings, looseStop)
	}
	rows := h.rowPaths(h.journal)
	for i, at := range []string{"2000-01-01T00:00:01Z", "2000-01-01T00:00:02Z"} {
		change(t, rows[i], set("at", at))
	}
	_, answer := verify(t, roots(h.journal)...)
	items := exclusionReports(t, answer, 2)
	if items[0].(map[string]any)["row"] == items[1].(map[string]any)["row"] || answer["unjudgedInvocations"].(map[string]any)["unestablished:transcript_path_missing"] != 2.0 {
		t.Fatal(answer)
	}
	_, answer = verify(t, append(roots(h.journal), "--since", "2000-01-01T00:00:02Z", "--until", "2000-01-01T00:00:03Z", "--session", "s", "--turn", "t")...)
	if exclusionReports(t, answer, 1)[0].(map[string]any)["row"] != rows[1] {
		t.Fatal(answer)
	}
	for _, filter := range [][]string{{"--session", "other"}, {"--turn", "other"}, {"--until", "2000-01-01T00:00:01Z"}} {
		_, answer = verify(t, append(roots(h.journal), filter...)...)
		if _, exists := answer["excludedInvocations"]; exists {
			t.Fatal(answer)
		}
	}
}

func TestExcludedInvocationsNeverOverrideEventIntegrity(t *testing.T) {
	first, second := newHost(t, hook.Release), newHost(t, hook.Release)
	payload := first.at(loadFixture(t), 0)
	first.run(first.settings, payload)
	second.run(second.settings, payload)
	refusal := refusedInvocation(t, first, hook.TranscriptPathMissing)
	code, answer := verify(t, roots(first.journal)...)
	expectVerdict(t, code, answer, 3, "UNREADABLE")
	exclusionReports(t, answer, 1)
	code, answer = verify(t, roots(first.journal, second.journal)...)
	expectVerdict(t, code, answer, 1, "FALSE")
	if len(listed(answer, "eventsWithMoreThanOneAcceptance")) != 1 {
		t.Fatal(answer)
	}
	exclusionReports(t, answer, 1)
	change(t, refusal, set("eventIdentity.established", true))
	code, answer = verify(t, roots(first.journal)...)
	expectVerdict(t, code, answer, 3, "UNREADABLE")
	if _, exists := answer["excludedInvocations"]; exists || len(listed(answer, "rowsUnreadable")) != 1 {
		t.Fatal(answer)
	}
}

func TestExcludedInvocationsExplainThePrescanExemption(t *testing.T) {
	h := newHost(t, hook.Release)
	if err := os.Remove(h.state + "/control.sock"); err != nil {
		t.Fatal(err)
	}
	h.run(h.settings, looseStop)
	code, answer := verify(t, roots(h.journal)...)
	expectVerdict(t, code, answer, 0, "TRUE")
	report := exclusionReports(t, answer, 1)[0].(map[string]any)
	if report["preventsTrue"] != false || report["reason"] != "no_event:guard_unreachable" || report["evidence"].(map[string]any)["eventIdentity"] != nil {
		t.Fatal(report)
	}
}

func TestEstablishedEventsOmitExcludedInvocations(t *testing.T) {
	h, _ := oneEvent(t)
	code, answer := verify(t, roots(h.journal)...)
	expectVerdict(t, code, answer, 0, "TRUE")
	if _, exists := answer["excludedInvocations"]; exists {
		t.Fatal(answer)
	}
}

func TestExcludedInvocationsDoNotExemptMalformedStdin(t *testing.T) {
	h := newHost(t, hook.Release)
	h.run(h.settings, []byte("{"))
	code, answer := verify(t, roots(h.journal)...)
	expectVerdict(t, code, answer, 3, "UNREADABLE")
	report := exclusionReports(t, answer, 1)[0].(map[string]any)
	if report["reason"] != "no_event:stdin_not_json" || report["preventsTrue"] != true || report["evidence"].(map[string]any)["eventIdentity"] != nil {
		t.Fatal(report)
	}
}

func TestExcludedInvocationsExplainStdinReadFailures(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "host_leaves_input_open"
		if closed {
			name = "descriptor_read_fails"
		}
		t.Run(name, func(t *testing.T) {
			h := newHost(t, hook.Release)
			t.Setenv("CODEX_HOME", h.codex)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			if closed {
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var out bytes.Buffer
			if code := hook.Run(ctx, nil, reader, &out, time.Now()); code != 0 || out.Len() != 0 {
				t.Fatalf("hook exit=%d stdout=%q", code, out.String())
			}
			rows := h.rowPaths(h.journal)
			if len(rows) != 1 {
				t.Fatalf("rows = %v", rows)
			}
			// A row this build wrote names its cause (CRW-504).
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			recorded := exclusionReports(t, answer, 1)[0].(map[string]any)["evidence"].(map[string]any)["stdinRead"].(map[string]any)
			expectNamedRead(t, recorded, map[bool]string{false: "input_late", true: "read_error"}[closed])
			// The same row without the key is a legacy one: its cause was discarded.
			change(t, rows[0], pop("stdinRead"))
			for _, elapsed := range []int{0, 100, 900} {
				change(t, rows[0], set("elapsedMs", elapsed))
				code, answer := verify(t, roots(h.journal)...)
				expectVerdict(t, code, answer, 3, "UNREADABLE")
				report := exclusionReports(t, answer, 1)[0].(map[string]any)
				evidence := report["evidence"].(map[string]any)
				reading, ok := evidence["stdinRead"].(map[string]any)
				if !ok || reading["case"] != "read_failed_cause_unrecorded" || reading["detail"] != "the Stop payload could not be read from stdin" || reading["elapsedMs"] != float64(elapsed) {
					t.Fatalf("stdinRead = %v", evidence["stdinRead"])
				}
				if report["reason"] != "no_event:stdin_unreadable" || report["preventsTrue"] != true || report["sessionId"] != nil || report["turnId"] != nil || evidence["guardInvoked"] != false || answer["unjudgedInvocations"].(map[string]any)["no_event:stdin_unreadable"] != 1.0 {
					t.Fatal(report, answer)
				}
			}
		})
	}
}

func TestExcludedInvocationsStdinDiagnosticContract(t *testing.T) {
	diagnostics := map[string]any{}
	for _, tc := range []struct{ name, detail, want string }{
		{"invalid_utf8", "", "invalid_utf8"},
		{"read_failure", "the Stop payload could not be read from stdin", "read_failed_cause_unrecorded"},
		{"unknown_detail", "a different input read failure", "detail_unrecognized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t, hook.Release)
			h.run(h.settings, []byte{0xff})
			path := h.rowPaths(h.journal)[0]
			// The contract below is the legacy row's: three keys, as before CRW-504.
			change(t, path, pop("stdinRead"))
			if tc.detail != "" {
				change(t, path, set("detail", tc.detail))
			}
			change(t, path, set("elapsedMs", 100))
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			evidence := exclusionReports(t, answer, 1)[0].(map[string]any)["evidence"].(map[string]any)
			reading, ok := evidence["stdinRead"].(map[string]any)
			if !ok || len(reading) != 3 || reading["case"] != tc.want || reading["detail"] != valueAt(t, path, "detail") || reading["elapsedMs"] != 100.0 {
				t.Fatalf("stdinRead = %v", evidence["stdinRead"])
			}
			if tc.name == "invalid_utf8" && !strings.HasPrefix(reading["detail"].(string), "the Stop payload is not UTF-8: ") {
				t.Fatal(reading)
			}
			diagnostics[tc.name] = reading
			change(t, path, set("detail", nil))
			code, answer = verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			if len(listed(answer, "rowsUnreadable")) != 1 || answer["excludedInvocations"] != nil {
				t.Fatal(answer)
			}
		})
	}
	golden.CheckJSON(t, "stdinRead", diagnostics)
}

func TestExcludedInvocationsEmptyEOFHasNoStdinReadDiagnostic(t *testing.T) {
	h := newHost(t, hook.Release)
	h.run(h.settings, nil)
	code, answer := verify(t, roots(h.journal)...)
	expectVerdict(t, code, answer, 3, "UNREADABLE")
	report := exclusionReports(t, answer, 1)[0].(map[string]any)
	if report["reason"] != "no_event:stdin_not_json" || report["evidence"].(map[string]any)["stdinRead"] != nil {
		t.Fatal(report)
	}
}

// expectNamedRead is a stdinRead diagnostic of a row that recorded its cause: the case plus the
// four recorded facts beside the detail and elapsedMs every diagnostic has.
func expectNamedRead(t *testing.T, reading map[string]any, cause string) {
	t.Helper()
	if len(reading) != 7 || reading["case"] != cause {
		t.Fatalf("stdinRead = %v, want case %s with seven keys", reading, cause)
	}
	if text, ok := reading["error"].(string); !ok || text == "" {
		t.Fatalf("stdinRead = %v", reading)
	}
	for _, key := range []string{"bytesRead", "waitStartedMs", "waitEndedMs", "elapsedMs"} {
		if _, ok := reading[key].(float64); !ok {
			t.Fatalf("stdinRead.%s = %v", key, reading[key])
		}
	}
	if _, ok := reading["detail"].(string); !ok {
		t.Fatalf("stdinRead = %v", reading)
	}
}

const genericReadDetail = "the Stop payload could not be read from stdin"

func recordedRead(cause, text string, bytes int64) hook.Object {
	return hook.Object{{Key: "cause", Value: cause}, {Key: "error", Value: text}, {Key: "bytesRead", Value: bytes}, {Key: "waitStartedMs", Value: int64(2)}, {Key: "waitEndedMs", Value: int64(101)}}
}

// stdinUnreadableRow is a real stdin_unreadable row to edit: invalid UTF-8 on stdin.
func stdinUnreadableRow(t *testing.T) (*host, string) {
	t.Helper()
	h := newHost(t, hook.Release)
	h.run(h.settings, []byte{0xff})
	return h, h.rowPaths(h.journal)[0]
}

// Each recorded cause is the judge's case, whatever else the row says, and its facts are reported.
func TestExcludedInvocationsNameTheRecordedStdinReadCause(t *testing.T) {
	utf8Detail := "the Stop payload is not UTF-8: 'utf-8' codec can't decode byte 0xff in position 6: invalid start byte"
	for _, c := range []struct {
		name, detail string
		reading      hook.Object
	}{
		{"input_late nothing written", genericReadDetail, recordedRead("input_late", "the Stop payload did not arrive within the input allocation", 0)},
		{"input_late partial payload", genericReadDetail, recordedRead("input_late", "the Stop payload did not arrive within the input allocation", 18)},
		{"work_ended", genericReadDetail, recordedRead("work_ended", "context deadline exceeded", 0)},
		{"read_error", genericReadDetail, recordedRead("read_error", "read |0: is a directory", 0)},
		{"invalid_utf8", utf8Detail, recordedRead("invalid_utf8", strings.TrimPrefix(utf8Detail, "the Stop payload is not UTF-8: "), 7)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, path := stdinUnreadableRow(t)
			change(t, path, set("detail", c.detail))
			change(t, path, set("stdinRead", c.reading))
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			report := exclusionReports(t, answer, 1)[0].(map[string]any)
			reading := report["evidence"].(map[string]any)["stdinRead"].(map[string]any)
			cause := c.reading.Get("cause").(string)
			expectNamedRead(t, reading, cause)
			if reading["error"] != c.reading.Get("error") || reading["detail"] != c.detail || reading["bytesRead"] != float64(c.reading.Get("bytesRead").(int64)) || reading["waitStartedMs"] != 2.0 || reading["waitEndedMs"] != 101.0 {
				t.Fatalf("stdinRead = %v", reading)
			}
			if report["reason"] != "no_event:stdin_unreadable" || report["preventsTrue"] != true || answer["unjudgedInvocations"].(map[string]any)["no_event:stdin_unreadable"] != 1.0 || len(listed(answer, "rowsUnreadable")) != 0 {
				t.Fatal(report, answer)
			}
		})
	}
}

// A row whose stdinRead is not one this adapter writes cannot be vouched for.
func TestMalformedStdinReadIsUnreadable(t *testing.T) {
	utf8Detail := "the Stop payload is not UTF-8: x"
	good := func() hook.Object { return recordedRead("read_error", "read |0: is a directory", 0) }
	drop := func(key string) hook.Object { return setAt(good(), []string{key}, nil, true) }
	for _, c := range []struct {
		name    string
		detail  string
		reading any
	}{
		{"null", genericReadDetail, nil},
		{"not an object", genericReadDetail, "read_error"},
		{"an extra key", genericReadDetail, append(good(), hook.Object{{Key: "payload", Value: "x"}}...)},
		{"missing cause", genericReadDetail, drop("cause")},
		{"missing error", genericReadDetail, drop("error")},
		{"missing bytesRead", genericReadDetail, drop("bytesRead")},
		{"missing waitStartedMs", genericReadDetail, drop("waitStartedMs")},
		{"missing waitEndedMs", genericReadDetail, drop("waitEndedMs")},
		{"unknown cause", genericReadDetail, setAt(good(), []string{"cause"}, "timeout", false)},
		{"error not a string", genericReadDetail, setAt(good(), []string{"error"}, int64(1), false)},
		{"negative bytes", genericReadDetail, setAt(good(), []string{"bytesRead"}, int64(-1), false)},
		{"bytes as text", genericReadDetail, setAt(good(), []string{"bytesRead"}, "3", false)},
		{"bytes as bool", genericReadDetail, setAt(good(), []string{"bytesRead"}, true, false)},
		{"negative wait start", genericReadDetail, setAt(good(), []string{"waitStartedMs"}, int64(-1), false)},
		{"wait end as text", genericReadDetail, setAt(good(), []string{"waitEndedMs"}, "x", false)},
		{"invalid_utf8 with the generic detail", genericReadDetail, recordedRead("invalid_utf8", "x", 1)},
		{"invalid_utf8 with no bytes", utf8Detail, recordedRead("invalid_utf8", "x", 0)},
		{"read_error with the UTF-8 detail", utf8Detail, good()},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, path := stdinUnreadableRow(t)
			change(t, path, set("detail", c.detail))
			change(t, path, set("stdinRead", c.reading))
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			if len(listed(answer, "rowsUnreadable")) != 1 || answer["excludedInvocations"] != nil {
				t.Fatal(answer)
			}
		})
	}
}

// stdinRead belongs to stdin_unreadable rows alone.
func TestStdinReadOnAnotherOutcomeIsUnreadable(t *testing.T) {
	for _, outcome := range []string{"stdin_not_json", "guard_unreachable"} {
		t.Run(outcome, func(t *testing.T) {
			h := newHost(t, hook.Release)
			if outcome == "guard_unreachable" {
				if err := os.Remove(h.state + "/control.sock"); err != nil {
					t.Fatal(err)
				}
				h.run(h.settings, looseStop)
			} else {
				h.run(h.settings, []byte("{"))
			}
			change(t, h.rowPaths(h.journal)[0], set("stdinRead", recordedRead("read_error", "read |0: is a directory", 0)))
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			if len(listed(answer, "rowsUnreadable")) != 1 || answer["excludedInvocations"] != nil {
				t.Fatal(answer)
			}
		})
	}
}
