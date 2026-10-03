package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

type holdCase struct {
	Kind     string         `json:"kind"`
	Source   string         `json:"source"`
	Code     *string        `json:"code"`
	Revision bool           `json:"revision"`
	Recovery map[string]any `json:"recovery"`
}

func holdOf(t *testing.T, kind, code, source string, revision bool) map[string]any {
	t.Helper()
	return plain(t, SettingsHoldRecovery(kind, code, source, revision)).(map[string]any)
}

func strp(s string) *string { return &s }

// holdCodes are the codes the recovery table is read for, no code included.
var holdCodes = []*string{strp(SettingsNotPreserved), strp(SettingUnobservable), strp(EnvironmentsUnknown),
	strp(UnverifiablePermissionProfile), strp(SettingsDifferAfterLoad), strp(UnsupportedApprovalPolicy),
	strp(SettingsUnavailable), strp(SettingsIncomplete), strp(SettingsMistyped), strp(UnsupportedSandboxType),
	strp("role_policy_unconfigured"), strp("role_binding_mismatch"), strp("settings_record_stale_for_role"), nil}

// holdTable is the whole recovery table (3 kinds x 3 sources x 14 codes x revision).
func holdTable(t *testing.T) []holdCase {
	t.Helper()
	var cases []holdCase
	for _, kind := range []string{"withheld", "capped", "channel_closed"} {
		for _, source := range []string{"attempt", "pre_send", "undetermined"} {
			for _, code := range holdCodes {
				for _, revision := range []bool{false, true} {
					cases = append(cases, holdCase{Kind: kind, Source: source, Code: code, Revision: revision,
						Recovery: holdOf(t, kind, str(code), source, revision)})
				}
			}
		}
	}
	return cases
}

// jsonLines is one compact JSON document per row, for a table golden read row by row.
func jsonLines(t *testing.T, rows []any) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

type readingRow struct {
	columns  map[string]*string
	reading  map[string]any
	recovery map[string]any
}

// packed is a journal cell as the store packs it: the sequence and the detail's JSON text, in
// json.dumps's spelling.
func packed(seq int, detail string) *string {
	quoted, err := json.Marshal(detail)
	if err != nil {
		panic(err)
	}
	return strp(fmt.Sprintf(`{"seq": %d, "detail": %s}`, seq, quoted))
}

// readingColumns is the table of SETTINGS_HOLD_COLUMNS rows the reading is compared over: every
// state, settled send, reconciliation, pre-send withhold, request and order of the three times.
func readingColumns() []map[string]*string {
	sent := []*string{nil,
		packed(10, `{"requestId": "r1", "settingsRefusal": {"reason": "settings_not_preserved", "field": "runtimeWorkspaceRoots"}}`),
		packed(10, `{"requestId": "r1", "settingsRefusal": null}`),
		packed(10, `{"requestId": "r1", "settingsRefusal": {"reason": 7}}`),
		packed(10, `{"requestId": "r1"}`),
		strp("not json")}
	reconciled := []*string{nil, packed(30, `{"settingsRefusal": {"reason": "setting_unobservable", "field": 3}}`),
		packed(5, `{"settingsRefusal": null}`)}
	presend := []*string{nil,
		packed(20, `{"operation": "settings_check", "reason": "settings_unavailable", "detail": "no row"}`),
		packed(20, `{"operation": "lifecycle_read", "reason": "recipient_archived"}`),
		packed(20, `{"operation": "role_check", "reason": "role_binding_mismatch", "detail": 9}`),
		packed(40, `{"operation": 5, "reason": "relationship_not_active"}`)}
	states := [][2]*string{{strp("withheld_pre_send"), nil}, {strp("withheld_pre_send"), strp("attempt_cap")},
		{strp("withheld_pre_send"), strp("host_lost_turn")}, {strp("inbox_only"), nil}, {strp("queued"), nil}}
	first, second := strp("2026-01-01T00:00:01"), strp("2026-01-01T00:00:02")
	times := [][3]*string{{nil, nil, nil}, {first, nil, nil}, {first, second, nil}, {first, nil, second}, {first, first, nil}}
	var out []map[string]*string
	for _, state := range states {
		for _, s := range sent {
			for _, r := range reconciled {
				for _, p := range presend {
					for _, request := range []*string{nil, strp("r1")} {
						for _, at := range times {
							out = append(out, map[string]*string{"sh_state": state[0], "sh_hold_reason": state[1],
								"sh_settled_sent": s, "sh_settled_reconciled": r, "sh_presend": p, "sh_request": request,
								"sh_settings_at": at[0], "sh_lifecycle_at": at[1], "sh_inactive_at": at[2]})
						}
					}
				}
			}
		}
	}
	return out
}

func str(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func (r readingRow) holdColumns() *HoldColumns {
	c := r.columns
	return &HoldColumns{State: str(c["sh_state"]), HoldReason: str(c["sh_hold_reason"]), SettledSent: str(c["sh_settled_sent"]),
		SettledReconciled: str(c["sh_settled_reconciled"]), Presend: str(c["sh_presend"]), Request: str(c["sh_request"]),
		SettingsAt: str(c["sh_settings_at"]), LifecycleAt: str(c["sh_lifecycle_at"]), InactiveAt: str(c["sh_inactive_at"])}
}

// readingTable is Go's reading of every row of readingColumns, with the recovery status attaches
// to it.
func readingTable(t *testing.T) []readingRow {
	t.Helper()
	columns := readingColumns()
	rows := make([]readingRow, len(columns))
	for i, c := range columns {
		row := readingRow{columns: c}
		row.reading = plain(t, SettingsHoldReading(row.holdColumns())).(map[string]any)
		if hold := pyjson.Map(row.reading["hold"]); hold != nil {
			reason, _ := hold["reason"].(string)
			row.recovery = holdOf(t, row.reading["kind"].(string), reason, hold["source"].(string), false)
		}
		rows[i] = row
	}
	return rows
}

// readingsWhere is every row of readingTable matching pick; the property needs at least one.
func readingsWhere(t *testing.T, pick func(readingRow) bool) []readingRow {
	t.Helper()
	var picked []readingRow
	for _, row := range readingTable(t) {
		if pick(row) {
			picked = append(picked, row)
		}
	}
	if len(picked) == 0 {
		t.Fatal("no row matched the property")
	}
	return picked
}

// The whole recovery table (3 kinds x 3 sources x 14 codes x revision) and the whole reading
// table (4500 rows, one per line as [columns, reading, recovery]) are compared with the goldens,
// which began as Python's settings_hold_recovery and settings_hold_reading; the SHN properties
// below read the same tables.
func Test25_SHN_the_recovery_and_reading_tables_are_the_goldens(t *testing.T) {
	cases := holdTable(t)
	if len(cases) != 252 {
		t.Fatalf("%d cases", len(cases))
	}
	golden.CheckJSON(t, "holds", cases)
	rows := readingTable(t)
	lines := make([]any, len(rows))
	for i, row := range rows {
		lines[i] = []any{row.columns, row.reading, row.recovery}
	}
	golden.Check(t, "readings", jsonLines(t, lines))
}

func hold(r readingRow) map[string]any { return pyjson.Map(r.reading["hold"]) }

// SHN-1: a withheld attempt hold on settings_not_preserved (runtimeWorkspaceRoots) is
// {kind: withheld, source: attempt, reason, field}; recovery operator + settings-show, then
// names settings-record --source user_transition.
func Test25_SHN1_a_withheld_attempt_hold_names_the_reason_and_the_operator(t *testing.T) {
	rows := readingsWhere(t, func(r readingRow) bool {
		h := hold(r)
		return r.reading["kind"] == "withheld" && h != nil && h["source"] == "attempt" && h["reason"] == SettingsNotPreserved
	})
	for _, r := range rows {
		if hold(r)["field"] != "runtimeWorkspaceRoots" || r.recovery["actor"] != "operator" || r.recovery["command"] != SettingsShow ||
			!strings.Contains(r.recovery["then"].(string), "settings-record --source user_transition") {
			t.Fatal(r.reading, r.recovery)
		}
	}
}

// SHN-2: a code the daemon can still clear (setting_unobservable) stays the daemon's.
func Test25_SHN2_an_answer_the_daemon_can_clear_stays_the_daemons(t *testing.T) {
	for _, source := range []string{"attempt", "pre_send"} {
		got := holdOf(t, "withheld", SettingUnobservable, source, false)
		if got["actor"] != "daemon" || got["command"] != SettingsShow || got["then"] != HoldDaemonThen {
			t.Fatal(got)
		}
	}
	if CompletionNextAction("received", map[string]any{"completion": map[string]any{"delivery": map[string]any{"state": "withheld_pre_send",
		"settingsHold": map[string]any{"kind": "withheld", "source": "attempt", "reason": SettingUnobservable}}}}) != "daemon_delivers" {
		t.Fatal("daemon code became a person's")
	}
}

// SHN-3: after the attempt cap a settings hold is kind capped; parent recovers via show-event
// and laterDeliveries names settings-record; next action parent_recovers_settings_hold.
func Test25_SHN3_after_the_cap_the_parent_reads_the_report(t *testing.T) {
	rows := readingsWhere(t, func(r readingRow) bool {
		return r.reading["kind"] == "capped" && hold(r) != nil && hold(r)["source"] == "attempt"
	})
	for _, r := range rows {
		if r.recovery["actor"] != "parent" || r.recovery["command"] != ShowEvent || !strings.Contains(r.recovery["laterDeliveries"].(string), "settings-record") {
			t.Fatal(r.recovery)
		}
	}
	if CompletionNextAction("received", map[string]any{"completion": map[string]any{"delivery": map[string]any{"state": "withheld_pre_send", "holdReason": "attempt_cap",
		"settingsHold": map[string]any{"kind": "capped", "source": "attempt", "reason": SettingsNotPreserved}}}}) != ActionParentRecoversSettings {
		t.Fatal("capped")
	}
}

// SHN-4: a correction held on the child's settings: withheld -> operator; capped -> parent with
// show-event; closed channel -> the correction's own then (no acknowledgement, generation-open).
func Test25_SHN4_a_correction_held_on_the_childs_settings_is_named(t *testing.T) {
	closed := holdOf(t, "channel_closed", SettingsNotPreserved, "attempt", true)
	if !strings.Contains(closed["then"].(string), "takes no acknowledgement") || !strings.Contains(closed["then"].(string), "generation-open") ||
		!strings.Contains(closed["laterDeliveries"].(string), "never or on-request") {
		t.Fatal(closed)
	}
	if got := holdOf(t, "capped", SettingsNotPreserved, "attempt", true); got["actor"] != "parent" || got["command"] != ShowEvent {
		t.Fatal(got)
	}
}

// SHN-5: a closed channel on a completion: kind channel_closed, parent recovery with show-event
// and "never or on-request"; next action parent_acknowledges.
func Test25_SHN5_a_closed_channel_names_the_parent(t *testing.T) {
	readingsWhere(t, func(r readingRow) bool { return r.reading["kind"] == "channel_closed" && hold(r) != nil })
	got := holdOf(t, "channel_closed", UnsupportedApprovalPolicy, "attempt", false)
	if got["actor"] != "parent" || got["then"] != HoldChannelThen || got["laterDeliveries"] != LaterByOwner {
		t.Fatal(got)
	}
	if CompletionNextAction("received", map[string]any{"completion": map[string]any{"delivery": map[string]any{"state": "inbox_only", "holdReason": "push_channel_closed"}}}) != ActionAwaitingAck {
		t.Fatal("closed channel next action")
	}
}

// SHN-6: a pre-send record refusal (settings_unavailable) is {source: pre_send, detail}, the
// operator's, with then "record it again from the creation result".
func Test25_SHN6_a_record_refusal_before_any_attempt_is_named(t *testing.T) {
	rows := readingsWhere(t, func(r readingRow) bool {
		return hold(r) != nil && hold(r)["reason"] == SettingsUnavailable && r.reading["kind"] == "withheld"
	})
	for _, r := range rows {
		if hold(r)["source"] != "pre_send" || hold(r)["detail"] != "no row" || r.recovery["actor"] != "operator" ||
			!strings.Contains(r.recovery["then"].(string), "record it again from the creation result") ||
			strings.Contains(r.recovery["then"].(string), "bring the recipient back") {
			t.Fatal(r.reading, r.recovery)
		}
	}
}

// SHN-7: role-gate refusals carry every repair and point at refusalDetail; the refusal text is
// the chosen withhold's own (a non-text detail is dropped, never borrowed).
func Test25_SHN7_a_role_gate_refusal_carries_the_repair_its_refusal_names(t *testing.T) {
	for _, code := range []string{"role_policy_unconfigured", "role_binding_mismatch", "settings_record_stale_for_role"} {
		then := holdOf(t, "withheld", code, "pre_send", false)["then"].(string)
		for _, must := range []string{"refusalDetail", "declare the role", "restart", "fix the binding or the creation"} {
			if !strings.Contains(strings.ToLower(then), strings.ToLower(must)) {
				t.Fatalf("%s: missing %q", code, must)
			}
		}
		if strings.Contains(then, "do not re-record") || strings.Contains(then, "bring the recipient back") {
			t.Fatal(then)
		}
	}
	rows := readingsWhere(t, func(r readingRow) bool { return hold(r) != nil && hold(r)["reason"] == "role_binding_mismatch" })
	for _, r := range rows {
		if hold(r)["detail"] != nil {
			t.Fatal("a non-text refusal detail was carried")
		}
	}
}

// SHN-8: a refusal row whose withhold did not take effect (the state is not a hold) earns no
// hold and no recovery.
func Test25_SHN8_a_withhold_that_did_not_take_effect_earns_no_recovery(t *testing.T) {
	rows := readingsWhere(t, func(r readingRow) bool {
		return str(r.columns["sh_state"]) == "queued" || str(r.columns["sh_hold_reason"]) == "host_lost_turn"
	})
	for _, r := range rows {
		if r.reading["hold"] != nil || r.recovery != nil {
			t.Fatal(r.columns)
		}
	}
}

// SHN-9: the current cause wins whatever the clock: the later of the pre-send withhold and the
// settled attempt is chosen; a later lifecycle or pause pre-send withhold is no settings hold.
func Test25_SHN9_the_current_cause_wins(t *testing.T) {
	rows := readingsWhere(t, func(r readingRow) bool {
		p := str(r.columns["sh_presend"])
		return r.reading["kind"] == "withheld" && (strings.Contains(p, "lifecycle_read") || strings.Contains(p, "relationship_not_active"))
	})
	for _, r := range rows {
		if r.reading["chosen"] == "pre_send" && r.reading["hold"] != nil {
			t.Fatal("a lifecycle or pause withhold read as a settings hold", r.columns)
		}
	}
}

// SHN-10: an attempt settled only by reconciliation names its cause (requestId, field kept when
// text); a refusal whose code is not text names no cause.
func Test25_SHN10_reconciliation_names_the_cause_and_a_non_text_code_names_none(t *testing.T) {
	readingsWhere(t, func(r readingRow) bool {
		return strings.Contains(str(r.columns["sh_settled_reconciled"]), "setting_unobservable")
	})
	rows := readingsWhere(t, func(r readingRow) bool {
		return strings.Contains(str(r.columns["sh_settled_sent"]), `\"reason\": 7`) && r.columns["sh_settled_reconciled"] == nil && r.columns["sh_presend"] == nil && r.reading["kind"] != nil
	})
	for _, r := range rows {
		if r.reading["chosen"] != "attempt" || r.reading["hold"] != nil {
			t.Fatal(r.reading)
		}
	}
}

// SHN-12 and SHN-13: rows written before causes were recorded are undetermined, never guessed;
// a strictly later pause or lifecycle withhold clears them; a timestamp tie stays undetermined.
func Test25_SHN12_an_unrecorded_cause_is_undetermined(t *testing.T) {
	rows := readingsWhere(t, func(r readingRow) bool { return hold(r) != nil && hold(r)["source"] == "undetermined" })
	for _, r := range rows {
		want := map[string]string{"withheld": "daemon", "capped": "parent", "channel_closed": "parent"}[r.reading["kind"].(string)]
		if r.recovery["actor"] != want {
			t.Fatal(r.reading, r.recovery)
		}
	}
	if !strings.Contains(holdOf(t, "withheld", "", "undetermined", false)["then"].(string), "nothing here claims a settings fix") {
		t.Fatal("undetermined then")
	}
}

func Test25_SHN13_a_strictly_later_pause_or_lifecycle_withhold_is_no_settings_hold(t *testing.T) {
	rows := readingsWhere(t, func(r readingRow) bool {
		return r.reading["kind"] == "withheld" && r.reading["definitive"] == false && str(r.columns["sh_settings_at"]) != "" &&
			(str(r.columns["sh_lifecycle_at"]) > str(r.columns["sh_settings_at"]) || str(r.columns["sh_inactive_at"]) > str(r.columns["sh_settings_at"]))
	})
	for _, r := range rows {
		if r.reading["hold"] != nil {
			t.Fatal(r.columns)
		}
	}
}

// nextRows is the projection table next-action precedence is compared over: every delivery state,
// hold, settings hold, pacing and host-lost count, read as a completion under each
// acknowledgement and assignment state and as a correction under each supersession and
// undelivered reason. A row is [state, ack, deliveryState, holdReason, settingsHold, pacing,
// hostLost, supersession, undeliveredReason]; needs_changes rows are corrections.
func nextRows(t *testing.T) [][9]any {
	t.Helper()
	values := func(raw string) []any {
		var out []any
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	states := values(`["queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain", "dispatched", "inbox_only", "acknowledged", "superseded"]`)
	holds := values(`[null, "attempt_cap", "push_channel_closed", "host_lost_turn", "unknown_send_lost", "unknown_send_undecided"]`)
	settingsHolds := values(`[null,
		{"kind": "withheld", "source": "attempt", "reason": "settings_not_preserved"},
		{"kind": "withheld", "source": "pre_send", "reason": "setting_unobservable"},
		{"kind": "withheld", "source": "undetermined", "reason": null},
		{"kind": "capped", "source": "attempt", "reason": "settings_not_preserved"},
		{"kind": "withheld", "source": "pre_send", "reason": "role_binding_mismatch"}]`)
	pacings := values(`[null, {"reason": "hourly_cap", "reopensAt": null}]`)
	lost := values(`[0, 1]`)
	acks := values(`[null, {"settlement": "verified", "accepted": true}, {"settlement": "unverified", "lastReason": null},
		{"settlement": "unverified", "lastReason": "revision_mismatch"}]`)
	supersessions := values(`[null, {"reason": "superseded_revision"}]`)
	reasons := values(`[null, {"source": "deliveries.hold_reason"}]`)
	var rows [][9]any
	for _, state := range states {
		for _, hold := range holds {
			for _, sh := range settingsHolds {
				for _, pacing := range pacings {
					for _, n := range lost {
						for _, ack := range acks {
							for _, assignment := range []any{"received", "requested"} {
								rows = append(rows, [9]any{assignment, ack, state, hold, sh, pacing, n, nil, nil})
							}
						}
						for _, supersession := range supersessions {
							for _, reason := range reasons {
								rows = append(rows, [9]any{"needs_changes", nil, state, hold, sh, pacing, n, supersession, reason})
							}
						}
					}
				}
			}
		}
	}
	return rows
}

// SHN-14: next-action precedence over the whole projection table (15552 completion and
// correction projections), compared with the golden one row per line as the row and its action.
func Test25_SHN14_next_action_precedence(t *testing.T) {
	rows := nextRows(t)
	lines := make([]any, len(rows))
	seen := map[string]int{}
	for i, r := range rows {
		state := r[0].(string)
		delivery := map[string]any{"state": r[2], "holdReason": r[3], "hostLostAttempts": r[6], "pacing": r[5], "settingsHold": r[4]}
		var got string
		if state == "needs_changes" {
			got = CorrectionNextAction(state, map[string]any{"correction": map[string]any{"supersession": r[7], "undeliveredReason": r[8], "delivery": delivery}})
		} else {
			got = CompletionNextAction(state, map[string]any{"completion": map[string]any{"ack": r[1], "delivery": delivery}})
		}
		lines[i] = append(r[:], got)
		seen[got]++
	}
	golden.Check(t, "actions", jsonLines(t, lines))
	for _, action := range []string{ActionOperatorRestoresSettings, ActionParentRecoversSettings, ActionSendPolicy} {
		if seen[action] == 0 {
			t.Fatalf("table never reaches %s", action)
		}
	}
}
