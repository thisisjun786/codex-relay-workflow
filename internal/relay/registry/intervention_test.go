package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// CRW-433: an admission by the relationship's registered parent is a direct parent intervention, recorded in the admission's transaction and read back by intervention-show. The tests drive the real
// command line over a temporary store and read the answers as the JSON the commands print.

type interventionWorld struct {
	t     *testing.T
	state string
	rid   string
}

func newInterventionWorld(t *testing.T) *interventionWorld {
	t.Helper()
	w := &interventionWorld{t: t, state: filepath.Join(t.TempDir(), "state")}
	code, out := w.run("register", "--parent-task", "01parent-task", "--parent-host", "host-a", "--child-task", "01child-task", "--child-host", "host-a", "--issue", "REL-1",
		"--artifact-root", t.TempDir(), "--allowed-recipient", "01parent-task", "--dispatch-request-id", "dispatch-1")
	if code != 0 {
		t.Fatalf("register exited %d: %v", code, out)
	}
	w.rid, _ = out["relationshipId"].(string)
	if w.rid == "" {
		t.Fatalf("register named no relationship: %v", out)
	}
	if code, out := w.run("generation-bind", "--relationship", w.rid, "--generation", "1", "--dispatch-turn-id", "turn-anchor"); code != 0 {
		t.Fatalf("generation-bind exited %d: %v", code, out)
	}
	return w
}

func (w *interventionWorld) run(args ...string) (int, map[string]any) {
	w.t.Helper()
	var stdout, stderr bytes.Buffer
	code := dispatch.Execute(ctx(), "codex-session-relay", append([]string{"--state", w.state}, args...), &stdout, &stderr)
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		w.t.Fatalf("%v exited %d with stdout %q stderr %q", args, code, stdout.String(), stderr.String())
	}
	return code, out
}

func (w *interventionWorld) admit(turn, actor, reason string) map[string]any {
	w.t.Helper()
	args := []string{"admit-turn", "--relationship", w.rid, "--generation", "1", "--turn", turn, "--actor", actor}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	code, out := w.run(args...)
	if code != 0 {
		w.t.Fatalf("admit-turn exited %d: %v", code, out)
	}
	return out
}

// interventions is intervention-show, as the list of what it prints.
func (w *interventionWorld) interventions() []map[string]any {
	w.t.Helper()
	code, out := w.run("intervention-show", "--relationship", w.rid)
	if code != 0 || out["relationshipId"] != w.rid || out["parentTaskId"] != "01parent-task" || fmt.Sprint(out["executionGeneration"]) != "1" {
		w.t.Fatalf("intervention-show exited %d: %v", code, out)
	}
	var list []map[string]any
	for _, item := range out["interventions"].([]any) {
		list = append(list, item.(map[string]any))
	}
	return list
}

// Criterion c2: a turn the registered parent admitted is recorded as a direct parent intervention (relationship, generation, turn, actor, reason) and read back; the admission itself is what it was.
func TestParentAdmissionIsRecordedAsADirectParentIntervention(t *testing.T) {
	w := newInterventionWorld(t)
	if got := w.interventions(); len(got) != 0 {
		t.Fatalf("interventions before any admission = %v", got)
	}

	out := w.admit("turn-bridge", "01parent-task", "sent the split over the bridge")
	if fmt.Sprint(out) != fmt.Sprint(map[string]any{"relationship": w.rid, "generation": float64(1), "turn": "turn-bridge", "evidence": "explicit_admission"}) {
		t.Fatalf("admit-turn answers %v: its output is what it was", out)
	}
	got := w.interventions()
	if len(got) != 1 || got[0]["kind"] != "direct_parent_intervention" || fmt.Sprint(got[0]["generation"]) != "1" || got[0]["turn"] != "turn-bridge" || got[0]["anchorTurnId"] != "turn-anchor" ||
		got[0]["actor"] != "01parent-task" || got[0]["reason"] != "sent the split over the bridge" || got[0]["recordedAt"] == "" {
		t.Fatalf("interventions = %v", got)
	}

	// the same statement again is not a second intervention; the same turn with another reason is another statement
	w.admit("turn-bridge", "01parent-task", "sent the split over the bridge")
	if got := w.interventions(); len(got) != 1 {
		t.Fatalf("the same admission twice made %d interventions", len(got))
	}
	w.admit("turn-bridge", "01parent-task", "and the new criteria")
	w.admit("turn-second", "01parent-task", "")
	got = w.interventions()
	if len(got) != 3 || got[1]["reason"] != "and the new criteria" || got[2]["turn"] != "turn-second" || got[2]["reason"] != nil {
		t.Fatalf("interventions = %v", got)
	}
	if n := count(t, w.state, "SELECT COUNT(*) FROM journal WHERE kind = 'turn_admitted'"); n != 4 {
		t.Fatalf("the admission journal holds %d rows, one for each call as before", n)
	}
}

// Another actor (an operator confirming a continuation, or the child) is admitted as before and leaves no intervention; an unknown relationship is refused.
func TestOnlyTheRegisteredParentIsRecordedAsIntervening(t *testing.T) {
	w := newInterventionWorld(t)
	w.admit("turn-operator", "operator", "owner-confirmed continuation")
	w.admit("turn-child", "01child-task", "")
	if got := w.interventions(); len(got) != 0 {
		t.Fatalf("an actor that is not the registered parent was recorded as intervening: %v", got)
	}
	if n := count(t, w.state, "SELECT COUNT(*) FROM generation_turns WHERE turn_id IN ('turn-operator', 'turn-child')"); n != 2 {
		t.Fatalf("%d of the two admissions were stored", n)
	}
	code, out := w.run("intervention-show", "--relationship", "rel-0000000000000000")
	if code != 2 || out["reason"] != "unregistered_relationship" {
		t.Fatalf("intervention-show of an unknown relationship = %d %v", code, out)
	}
}
