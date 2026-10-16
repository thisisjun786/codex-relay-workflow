package journal

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/crwcmd"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// TestWakeNoticeNamesTheRunningCrw (CRW-1572): the wake notice of a session run's owner names its commands with the crw that runs,
// in its text and its evidence, so the owner runs them as written whether or not crw is on its PATH.
func TestWakeNoticeNamesTheRunningCrw(t *testing.T) {
	const word = "'/opt/crw test/bin/crw'"
	defer crwcmd.Override(word)()
	st, _ := newStore(t)
	j := newJournal(t, st)
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "w", "script_digest": "d", "args_digest": "a"}})
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := StageWake(st)(ctx, engine.SessionWake{Run: runID, Owner: identity.ActorTaskID, Batch: "b1", ID: "b1", Ops: []string{"c1/t1-a1"}}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB.QueryRow(`SELECT packet FROM supervisor_messages WHERE relationship_id = ?`, "run:"+runID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var packet struct {
		Body     string   `json:"body"`
		Evidence []string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(raw), &packet); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	next := word + " flow next " + runID + " --json"
	if !strings.Contains(packet.Body, "Take them: "+next+", then spawn_agent") || !strings.Contains(packet.Body, "run "+word+" flow dispatched for it.") ||
		strings.Contains(packet.Body, "crw flow ") {
		t.Errorf("the wake's text does not name the running crw: %q", packet.Body)
	}
	if len(packet.Evidence) != 1 || packet.Evidence[0] != next {
		t.Errorf("the wake's evidence %q, want [%q]", packet.Evidence, next)
	}
}
