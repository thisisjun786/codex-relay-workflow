package registry

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

type holdCase struct {
	Kind     string         `json:"kind"`
	Source   string         `json:"source"`
	Code     *string        `json:"code"`
	Revision bool           `json:"revision"`
	Recovery map[string]any `json:"recovery"`
}

func pythonHolds(t *testing.T) []holdCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/python_holds.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []holdCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func holdOf(t *testing.T, kind, code, source string, revision bool) map[string]any {
	t.Helper()
	return plain(t, SettingsHoldRecovery(kind, code, source, revision)).(map[string]any)
}

// The whole recovery table (3 kinds x 3 sources x 14 codes x revision), compared with Python.
func sameHoldsAsPython(t *testing.T) {
	t.Helper()
	cases := pythonHolds(t)
	if len(cases) != 252 {
		t.Fatalf("%d cases", len(cases))
	}
	for _, c := range cases {
		code := ""
		if c.Code != nil {
			code = *c.Code
		}
		if got := holdOf(t, c.Kind, code, c.Source, c.Revision); !reflect.DeepEqual(got, c.Recovery) {
			t.Errorf("%s/%s/%s/%v: go %v, python %v", c.Kind, c.Source, code, c.Revision, got, c.Recovery)
		}
	}
}

func gz(t *testing.T, name string) []byte {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type readingRow struct {
	columns  map[string]*string
	reading  map[string]any
	recovery map[string]any
}

func pythonReadings(t *testing.T) []readingRow {
	t.Helper()
	var rows [][3]json.RawMessage
	if err := json.Unmarshal(gz(t, "testdata/python_reading.json.gz"), &rows); err != nil {
		t.Fatal(err)
	}
	out := make([]readingRow, len(rows))
	for i, r := range rows {
		if err := json.Unmarshal(r[0], &out[i].columns); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(r[1], &out[i].reading); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(r[2], &out[i].recovery); err != nil {
			t.Fatal(err)
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

// goReading is one row's reading and the recovery status attaches to it, from Go.
func (r readingRow) goReading(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	reading := plain(t, SettingsHoldReading(r.holdColumns())).(map[string]any)
	var recovery map[string]any
	if hold := obj(reading["hold"]); hold != nil {
		reason, _ := hold["reason"].(string)
		recovery = holdOf(t, reading["kind"].(string), reason, hold["source"].(string), false)
	}
	return reading, recovery
}

// sameReadingsAsPython checks every row (4500) and returns those matching pick.
func sameReadingsAsPython(t *testing.T, pick func(readingRow) bool) []readingRow {
	t.Helper()
	var picked []readingRow
	for i, row := range pythonReadings(t) {
		reading, recovery := row.goReading(t)
		if !reflect.DeepEqual(reading, row.reading) || !reflect.DeepEqual(recovery, row.recovery) {
			t.Fatalf("row %d %v: go %v / %v, python %v / %v", i, row.columns, reading, recovery, row.reading, row.recovery)
		}
		if pick(row) {
			picked = append(picked, row)
		}
	}
	if len(picked) == 0 {
		t.Fatal("no row matched the property")
	}
	return picked
}

func hold(r readingRow) map[string]any { return obj(r.reading["hold"]) }

// SHN-1: a withheld attempt hold on settings_not_preserved (runtimeWorkspaceRoots) is
// {kind: withheld, source: attempt, reason, field}; recovery operator + settings-show, then
// names settings-record --source user_transition.
func Test25_SHN1_a_withheld_attempt_hold_names_the_reason_and_the_operator(t *testing.T) {
	sameHoldsAsPython(t)
	rows := sameReadingsAsPython(t, func(r readingRow) bool {
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
	sameHoldsAsPython(t)
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
	sameHoldsAsPython(t)
	rows := sameReadingsAsPython(t, func(r readingRow) bool {
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
	sameHoldsAsPython(t)
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
	sameHoldsAsPython(t)
	sameReadingsAsPython(t, func(r readingRow) bool { return r.reading["kind"] == "channel_closed" && hold(r) != nil })
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
	rows := sameReadingsAsPython(t, func(r readingRow) bool {
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
	sameHoldsAsPython(t)
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
	rows := sameReadingsAsPython(t, func(r readingRow) bool { return hold(r) != nil && hold(r)["reason"] == "role_binding_mismatch" })
	for _, r := range rows {
		if hold(r)["detail"] != nil {
			t.Fatal("a non-text refusal detail was carried")
		}
	}
}

// SHN-8: a refusal row whose withhold did not take effect (the state is not a hold) earns no
// hold and no recovery.
func Test25_SHN8_a_withhold_that_did_not_take_effect_earns_no_recovery(t *testing.T) {
	rows := sameReadingsAsPython(t, func(r readingRow) bool {
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
	rows := sameReadingsAsPython(t, func(r readingRow) bool {
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
	sameReadingsAsPython(t, func(r readingRow) bool {
		return strings.Contains(str(r.columns["sh_settled_reconciled"]), "setting_unobservable")
	})
	rows := sameReadingsAsPython(t, func(r readingRow) bool {
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
	sameHoldsAsPython(t)
	rows := sameReadingsAsPython(t, func(r readingRow) bool { return hold(r) != nil && hold(r)["source"] == "undetermined" })
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
	rows := sameReadingsAsPython(t, func(r readingRow) bool {
		return r.reading["kind"] == "withheld" && r.reading["definitive"] == false && str(r.columns["sh_settings_at"]) != "" &&
			(str(r.columns["sh_lifecycle_at"]) > str(r.columns["sh_settings_at"]) || str(r.columns["sh_inactive_at"]) > str(r.columns["sh_settings_at"]))
	})
	for _, r := range rows {
		if r.reading["hold"] != nil {
			t.Fatal(r.columns)
		}
	}
}

// SHN-14: next-action precedence, compared with Python over the whole projection table (15552
// completion and correction projections).
func Test25_SHN14_next_action_precedence_matches_python(t *testing.T) {
	var rows [][10]any
	if err := json.Unmarshal(gz(t, "testdata/python_next.json.gz"), &rows); err != nil {
		t.Fatal(err)
	}
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
		want, _ := r[9].(string)
		if got != want {
			t.Fatalf("row %d %v: go %q, python %q", i, r, got, want)
		}
		seen[want]++
	}
	for _, action := range []string{ActionOperatorRestoresSettings, ActionParentRecoversSettings, ActionSendPolicy} {
		if seen[action] == 0 {
			t.Fatalf("table never reaches %s", action)
		}
	}
}
