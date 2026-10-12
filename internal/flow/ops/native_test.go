package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// F14 (CRW-1207): the run imports the hook's ledger of the native spawns of a bound thread, joined with the turn's
// CollabAgentToolCall items and the configuration of the threads they created.

const (
	nThread   = "0190a000-0000-7000-8000-00000000000a"
	nReceiver = "0190a000-0000-7000-8000-0000000000a1"
	nCall     = "c_000000000001"
	nRun      = "run-native"
	nTurn     = "turn-1"
)

var nNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func sp(s string) *string { return &s }

// nativeHost is a host whose turn holds items and whose threads read back a configuration.
type nativeHost struct {
	items   []json.RawMessage
	itemErr error
	cfg     map[string][2]*string
	cfgErr  map[string]error
}

func (h *nativeHost) TurnItems(context.Context, string, string) ([]json.RawMessage, error) {
	return h.items, h.itemErr
}

func (h *nativeHost) ThreadConfig(_ context.Context, id string) (*string, *string, error) {
	if err := h.cfgErr[id]; err != nil {
		return nil, nil, err
	}
	c, ok := h.cfg[id]
	if !ok {
		return nil, nil, errors.New("thread not found: " + id)
	}
	return c[0], c[1], nil
}

// spawnItem is a CollabAgentToolCall item as the host reads it back.
func spawnItem(t *testing.T, id string, receivers []string, model, effort *string) json.RawMessage {
	t.Helper()
	item := map[string]any{"type": "collabAgentToolCall", "id": id, "tool": "spawnAgent", "status": "completed", "senderThreadId": nThread,
		"receiverThreadIds": receivers, "agentsStates": map[string]any{}, "model": model, "reasoningEffort": effort}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ledger makes a flow-bound directory in a temporary home whose ledger of the thread holds the decisions.
func ledger(t *testing.T, decisions ...bound.Decision) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	one := 1
	line := bound.Line{RunID: nRun, ThreadID: nThread, Kind: bound.KindEngine, CallID: nCall, Role: "executor", NativeAllowance: &one,
		HelperPairs: map[string]bound.Pair{bound.HelperExplorer: {Model: "explorer-model", Effort: "high"}}}
	if err := (&bound.Writer{}).Append(line); err != nil {
		t.Fatal(err)
	}
	dir, err := bound.Dir()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		if d.RunID == "" {
			d.RunID = nRun
		}
		if d.ThreadID == "" {
			d.ThreadID = nThread
		}
		if d.CallID == "" {
			d.CallID = nCall
		}
		if d.ThreadKind == "" {
			d.ThreadKind = bound.KindEngine
		}
		if err := bound.AppendDecision(dir, d); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func allowed(tool, reqModel, reqEffort string) bound.Decision {
	return bound.Decision{ToolUseID: tool, Decision: bound.DecisionAllowed, HelperRole: bound.HelperExplorer, RequestedModel: reqModel, RequestedEffort: reqEffort,
		AppliedModel: "explorer-model", AppliedEffort: "high"}
}

func importTurn(t *testing.T, h *nativeHost, dir string) NativeImport {
	t.Helper()
	got, err := ImportNativeSpawns(context.Background(), h, dir, NativeTurn{RunID: nRun, ThreadID: nThread, CallID: nCall, TurnID: nTurn, Node: "n1"}, nNow)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func rowsOf(in NativeImport, kind string) []SpawnRow {
	var out []SpawnRow
	for _, r := range in.Rows {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// runRow makes the run's row in flow_runs, which flow_spawn_decisions points at.
func runRow(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB.Exec(`INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at)
 VALUES (?, 'wf', 'sd', '[]', 'ad', 'plan-1', 1, 'task-1', 'nonce', '', '2026-10-12T09:00:00Z')`, nRun); err != nil {
		t.Fatal(err)
	}
}

// What the hook decided about a spawn is imported into flow_spawn_decisions with the run, the call, the tool use id, the pair the
// caller sent and the pair the hook applied; an import repeated for the same turn writes nothing more.
func TestNativeSpawnDecisionImported(t *testing.T) {
	refused := bound.Decision{ToolUseID: "tool-2", Decision: bound.DecisionRefused, Reason: bound.ReasonNativeLimitRefused, RequestedModel: "m", RequestedEffort: "low", Detail: "over the limit"}
	dir := ledger(t, allowed("tool-1", "", "low"), refused)
	h := &nativeHost{
		items: []json.RawMessage{spawnItem(t, "tool-1", []string{nReceiver}, sp("explorer-model"), sp("high"))},
		cfg:   map[string][2]*string{nReceiver: {sp("explorer-model"), sp("high")}},
	}
	got := importTurn(t, h, dir)
	if len(got.Escalations) != 0 {
		t.Fatalf("escalations %+v", got.Escalations)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runRow(t, st)
	n, err := StoreSpawnRows(context.Background(), st.DB, got.Rows)
	if err != nil || n != len(got.Rows) || n != 3 {
		t.Fatalf("stored %d of %d rows: %v", n, len(got.Rows), err)
	}
	if n, err := StoreSpawnRows(context.Background(), st.DB, importTurn(t, h, dir).Rows); err != nil || n != 0 {
		t.Fatalf("the same import again wrote %d rows: %v", n, err)
	}
	type stored struct{ run, kind, call, node, rule, decision, reason, rm, re, am, ae, tool, evidence, at string }
	read := func(where string) (s stored) {
		t.Helper()
		err := st.DB.QueryRow(`SELECT run_id, kind, call_id, node_id, rule_id, decision, reason, requested_model, requested_effort, applied_model, applied_effort, tool_use_id, evidence_json, decided_at
 FROM flow_spawn_decisions WHERE `+where).Scan(&s.run, &s.kind, &s.call, &s.node, &s.rule, &s.decision, &s.reason, &s.rm, &s.re, &s.am, &s.ae, &s.tool, &s.evidence, &s.at)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		return s
	}
	a := read(`kind = 'native_spawn' AND decision = 'allowed'`)
	if a.run != nRun || a.call != nCall || a.node != "n1" || a.rule != NativeRule || a.tool != "tool-1" || a.rm != "" || a.re != "low" || a.am != "explorer-model" || a.ae != "high" || a.at != "2026-10-12T09:00:00Z" {
		t.Fatalf("allowed decision %+v", a)
	}
	r := read(`kind = 'native_spawn' AND decision = 'refused'`)
	if r.reason != bound.ReasonNativeLimitRefused || r.tool != "tool-2" || r.rm != "m" || r.re != "low" || r.am != "" || !strings.Contains(r.evidence, "over the limit") {
		t.Fatalf("refused decision %+v", r)
	}
	o := read(`kind = 'native_observation'`)
	if o.decision != ObservationObserved || o.tool != "tool-1" || !strings.Contains(o.evidence, nReceiver) {
		t.Fatalf("observation %+v", o)
	}
}

// The request the caller sent, the pair the hook applied and the pair the receiver reads back are three values that stay apart.
func TestNativeThreePairsRecordedSeparately(t *testing.T) {
	dir := ledger(t, allowed("tool-1", "someone-elses", "low"))
	// the host's item shows what the call was made with: the applied pair; the receiver reads back a third spelling of the effort
	h := &nativeHost{
		items: []json.RawMessage{spawnItem(t, "tool-1", []string{nReceiver}, sp("explorer-model"), sp("high"))},
		cfg:   map[string][2]*string{nReceiver: {sp("explorer-model"), sp("high")}},
	}
	obs := rowsOf(importTurn(t, h, dir), NativeKindObservation)
	if len(obs) != 1 {
		t.Fatalf("%+v", obs)
	}
	o := obs[0]
	if o.RequestedModel != "someone-elses" || o.RequestedEffort != "low" || o.AppliedModel != "explorer-model" || o.AppliedEffort != "high" {
		t.Fatalf("requested and applied: %+v", o)
	}
	observed, _ := o.Evidence["observed"].(map[string]any)
	if model, _ := observed["model"].(*string); model == nil || *model != "explorer-model" {
		t.Fatalf("observed %+v", observed)
	}
	if o.Decision != ObservationObserved {
		t.Fatalf("decision %s", o.Decision)
	}
	req, _ := o.Evidence["requested"].(map[string]any)
	applied, _ := o.Evidence["applied"].(map[string]any)
	if req["effort"] != "low" || applied["effort"] != "high" {
		t.Fatalf("evidence requested %+v applied %+v", req, applied)
	}
}

// A receiver that reads back another pair than the hook applied is a pair_mismatch, and the run escalates it once.
func TestNativeConfiguredDiffersFromApplied(t *testing.T) {
	for _, c := range []struct {
		name          string
		model, effort *string
	}{
		{"model", sp("claude-opus"), sp("high")},
		{"effort", sp("explorer-model"), sp("low")},
		{"effort, model unread", nil, sp("low")},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := ledger(t, allowed("tool-1", "", ""))
			h := &nativeHost{
				items: []json.RawMessage{spawnItem(t, "tool-1", []string{nReceiver}, sp("explorer-model"), sp("high"))},
				cfg:   map[string][2]*string{nReceiver: {c.model, c.effort}},
			}
			got := importTurn(t, h, dir)
			obs := rowsOf(got, NativeKindObservation)
			if len(obs) != 1 || obs[0].Decision != ObservationPairMismatch {
				t.Fatalf("%+v", obs)
			}
			if len(got.Escalations) != 1 || got.Escalations[0].Reason != ObservationPairMismatch || got.Escalations[0].ToolUseID != "tool-1" || got.Escalations[0].CallID != nCall {
				t.Fatalf("escalations %+v", got.Escalations)
			}
			j := engine.NewMemoryJournal()
			EscalateNative(j, nRun, got.Escalations)
			EscalateNative(j, nRun, importTurn(t, h, dir).Escalations)
			var rows []engine.Event
			for _, e := range j.Events() {
				if e.Event == engine.EvEscalation {
					rows = append(rows, e)
				}
			}
			if len(rows) != 1 || rows[0].Data["reason"] != ObservationPairMismatch || rows[0].Data["class"] != "configuration_fault" || rows[0].Data["tool_use_id"] != "tool-1" {
				t.Fatalf("journal escalations %+v", rows)
			}
		})
	}
}

// A model or effort that is not there is unobserved, never a success: no pair_mismatch, no escalation, no observed row.
func TestNativeObservedMissingIsUnobserved(t *testing.T) {
	for _, c := range []struct {
		name   string
		model  *string
		effort *string
		err    error
		reason string
	}{
		{"null model", nil, sp("high"), nil, UnobservedNullModel},
		{"null effort", sp("explorer-model"), nil, nil, UnobservedNullEffort},
		{"both null", nil, nil, nil, UnobservedNullModel},
		{"read failed", nil, nil, errors.New("thread/read failed"), UnobservedReadFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := ledger(t, allowed("tool-1", "", ""))
			h := &nativeHost{
				items:  []json.RawMessage{spawnItem(t, "tool-1", []string{nReceiver}, sp("explorer-model"), sp("high"))},
				cfg:    map[string][2]*string{nReceiver: {c.model, c.effort}},
				cfgErr: map[string]error{nReceiver: c.err},
			}
			got := importTurn(t, h, dir)
			obs := rowsOf(got, NativeKindObservation)
			if len(obs) != 1 || obs[0].Decision != ObservationUnobserved || obs[0].Reason != c.reason {
				t.Fatalf("%+v", obs)
			}
			if len(got.Escalations) != 0 {
				t.Fatalf("an unobserved pair escalated: %+v", got.Escalations)
			}
			for _, r := range got.Rows {
				if r.Decision == ObservationObserved {
					t.Fatalf("an observed row: %+v", r)
				}
			}
		})
	}
	// one receiver read, one not: each is recorded for itself
	dir := ledger(t, allowed("tool-1", "", ""))
	other := "0190a000-0000-7000-8000-0000000000a2"
	h := &nativeHost{
		items: []json.RawMessage{spawnItem(t, "tool-1", []string{nReceiver, other}, sp("explorer-model"), sp("high"))},
		cfg:   map[string][2]*string{nReceiver: {sp("explorer-model"), sp("high")}},
	}
	obs := rowsOf(importTurn(t, h, dir), NativeKindObservation)
	var kinds []string
	for _, o := range obs {
		kinds = append(kinds, o.Decision+"/"+o.Reason)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"observed/", "unobserved/read_failed"}) {
		t.Fatalf("%v", kinds)
	}
}

// A spawn call that created no thread (or whose item names none) leaves nothing to read: unobserved.
func TestNativeNoReceiverIsUnobserved(t *testing.T) {
	for name, receivers := range map[string][]string{"empty list": {}, "no list": nil} {
		t.Run(name, func(t *testing.T) {
			dir := ledger(t, allowed("tool-1", "", ""))
			h := &nativeHost{items: []json.RawMessage{spawnItem(t, "tool-1", receivers, nil, nil)}}
			got := importTurn(t, h, dir)
			obs := rowsOf(got, NativeKindObservation)
			if len(obs) != 1 || obs[0].Decision != ObservationUnobserved || obs[0].Reason != UnobservedNoReceiver || len(got.Escalations) != 0 {
				t.Fatalf("%+v %+v", obs, got.Escalations)
			}
		})
	}
}

// A CollabAgentToolCall of a bound thread the hook did not decide is unbound_spawn, a configuration fault: its binding file may have
// vanished, or the hook and the run read different homes. Items that are not spawns of this thread do not count.
func TestNativeSpawnWithoutAppliedRecordEscalates(t *testing.T) {
	foreign := func(id string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"type": "collabAgentToolCall", "id": id, "tool": "spawnAgent", "status": "completed", "senderThreadId": "0190a000-0000-7000-8000-00000000ffff", "receiverThreadIds": []string{nReceiver}})
		return raw
	}
	wait, _ := json.Marshal(map[string]any{"type": "collabAgentToolCall", "id": "tool-wait", "tool": "wait", "status": "completed", "senderThreadId": nThread, "receiverThreadIds": []string{nReceiver}})
	message, _ := json.Marshal(map[string]any{"type": "agentMessage", "id": "m", "text": "ok"})
	run := func(t *testing.T, dir string) NativeImport {
		h := &nativeHost{
			items: []json.RawMessage{message, wait, foreign("tool-foreign"), spawnItem(t, "tool-x", []string{nReceiver}, sp("explorer-model"), sp("high"))},
			cfg:   map[string][2]*string{nReceiver: {sp("explorer-model"), sp("high")}},
		}
		return importTurn(t, h, dir)
	}
	check := func(t *testing.T, got NativeImport, why string) {
		t.Helper()
		obs := rowsOf(got, NativeKindObservation)
		if len(obs) != 1 || obs[0].Decision != ObservationUnbound || obs[0].ToolUseID != "tool-x" || obs[0].Reason != why {
			t.Fatalf("%+v", obs)
		}
		if len(got.Escalations) != 1 || got.Escalations[0].Reason != ObservationUnbound || got.Escalations[0].ToolUseID != "tool-x" {
			t.Fatalf("escalations %+v", got.Escalations)
		}
		j := engine.NewMemoryJournal()
		EscalateNative(j, nRun, got.Escalations)
		if events := j.Events(); len(events) != 1 || events[0].Event != engine.EvEscalation || events[0].Data["reason"] != ObservationUnbound {
			t.Fatalf("journal %+v", events)
		}
	}
	t.Run("ledger without the tool use", func(t *testing.T) {
		check(t, run(t, ledger(t, allowed("tool-1", "", ""))), "no_applied_record")
	})
	t.Run("only a refusal recorded", func(t *testing.T) {
		check(t, run(t, ledger(t, bound.Decision{ToolUseID: "tool-x", Decision: bound.DecisionRefused, Reason: bound.ReasonNativeLimitRefused})), "only_a_refusal_recorded")
	})
	t.Run("no ledger at all", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		dir, err := bound.Dir()
		if err != nil {
			t.Fatal(err)
		}
		check(t, run(t, dir), "no_applied_record")
	})
	t.Run("a decision of another call does not apply", func(t *testing.T) {
		other := allowed("tool-x", "", "")
		other.CallID = "c_000000000099"
		check(t, run(t, ledger(t, other)), "no_applied_record")
	})
}

// CRW-1207 correction round (d2): an observation that says something new is a new row. The id covers the receiver, the decision,
// the reason and the configured pair, so a transient failure of thread/read stored first does not keep the later reading out of
// flow_spawn_decisions, while the same facts seen again stay one row.
func TestNativeChangedObservationIsANewRow(t *testing.T) {
	dir := ledger(t, allowed("tool-1", "", "low"))
	items := []json.RawMessage{spawnItem(t, "tool-1", []string{nReceiver}, sp("explorer-model"), sp("high"))}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runRow(t, st)
	store := func(h *nativeHost) (NativeImport, int) {
		got := importTurn(t, h, dir)
		n, err := StoreSpawnRows(context.Background(), st.DB, got.Rows)
		if err != nil {
			t.Fatal(err)
		}
		return got, n
	}
	failing := &nativeHost{items: items, cfgErr: map[string]error{nReceiver: errors.New("rpc timeout")}}
	if _, n := store(failing); n != 2 { // the decision and the unreadable observation
		t.Fatalf("first import stored %d rows", n)
	}
	// the same facts, even with another error text, are the same row
	failing.cfgErr[nReceiver] = errors.New("rpc timeout again")
	if _, n := store(failing); n != 0 {
		t.Fatalf("the same unreadable observation was stored again: %d", n)
	}
	// the thread reads now, with another effort than the hook applied
	mismatch := &nativeHost{items: items, cfg: map[string][2]*string{nReceiver: {sp("explorer-model"), sp("low")}}}
	got, n := store(mismatch)
	if n != 1 {
		t.Fatalf("the mismatch observation stored %d rows", n)
	}
	if len(got.Escalations) != 1 || got.Escalations[0].Reason != ObservationPairMismatch {
		t.Fatalf("escalations %+v", got.Escalations)
	}
	// and then a reading that agrees: a third fact
	good := &nativeHost{items: items, cfg: map[string][2]*string{nReceiver: {sp("explorer-model"), sp("high")}}}
	if _, n := store(good); n != 1 {
		t.Fatalf("the observed reading stored %d rows", n)
	}
	if _, n := store(mismatch); n != 0 {
		t.Fatalf("a reading already recorded was stored again: %d", n)
	}
	rows, err := st.DB.Query(`SELECT decision, reason FROM flow_spawn_decisions WHERE kind = 'native_observation' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var d, r string
		if err := rows.Scan(&d, &r); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, d+"/"+r)
	}
	if strings.Join(seen, ",") != "unobserved/read_failed,pair_mismatch/,observed/" {
		t.Fatalf("observations %v", seen)
	}
}

// CRW-1207 correction round (d4): the import reads a ledger that has a cut tail.
func TestNativeImportReadsALedgerWithACutTail(t *testing.T) {
	dir := ledger(t, allowed("tool-1", "", ""))
	path := filepath.Join(dir, bound.DecisionsDir, nThread+bound.FileSuffix)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"run_id":"run-native","thread_id":"` + nThread + `","tool_use_id":"tool-2","deci`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	h := &nativeHost{items: []json.RawMessage{spawnItem(t, "tool-1", []string{nReceiver}, sp("explorer-model"), sp("high"))},
		cfg: map[string][2]*string{nReceiver: {sp("explorer-model"), sp("high")}}}
	got := importTurn(t, h, dir)
	if len(rowsOf(got, NativeKindSpawn)) != 1 || len(rowsOf(got, NativeKindObservation)) != 1 {
		t.Fatalf("rows %+v", got.Rows)
	}
}
