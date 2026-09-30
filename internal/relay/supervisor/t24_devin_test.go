package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func devinPythonStage(t *testing.T, f *stageFixture, project string, readings []map[string]any) supervisorCapture {
	t.Helper()
	raw, err := json.Marshal(readings)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `
import json,sqlite3,sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.linkage import Linkage
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
from codex_session_relay import supervisorchannel
store=Store(sys.argv[1]); clock=FakeClock()
supervisorchannel.relay_program=lambda: ('/usr/bin/codex-session-relay',)
channel=supervisorchannel.SupervisorChannel(store, Registry(store,clock), Linkage(store,clock), clock,
    settings=lambda task,runtime=None: {'authorized':task}, state_directory=sys.argv[2])
channel.store_readings=lambda project,observations=(): []
channel._omission_withdrawn=lambda obligation,reading: None
answer=channel.stage_standing(sys.argv[3], observations=json.loads(sys.argv[4]))
tables={}
for row in store.all("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
    name=row['name']; values=[dict(one) for one in store.all('SELECT * FROM '+name+' ORDER BY rowid')]
    if values: tables[name]=values
print(json.dumps({'captures':[answer], 'problems':[], 'tables':tables}, sort_keys=True))
store.close()
`
	// Python answers on a copy it owns of the store as it stands (recorded, pythonOutput).
	out := pythonOutput(t, pyKey(t, "stage"), func() ([]byte, error) {
		copyPath := filepath.Join(t.TempDir(), "relay.sqlite3")
		if _, err := f.s.DB.ExecContext(f.ctx, "VACUUM INTO ?", copyPath); err != nil {
			return nil, err
		}
		ownCopied(t, copyPath, "python")
		cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), "-c", script, copyPath, filepath.Join(f.root, "state"), project, string(raw))
		home := t.TempDir()
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_DATA_HOME="+home, "XDG_CONFIG_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm", "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("live Python stage: %v\n%s", err, out)
		}
		return out, nil
	}, pyoracle.Substitute(f.root, "<fixture>"))
	var capture supervisorCapture
	if err := json.Unmarshal(out, &capture); err != nil {
		t.Fatalf("live Python JSON: %v\n%s", err, out)
	}
	return capture
}

func devinFixture(t *testing.T) *stageFixture {
	t.Helper()
	f := fixture24(t)
	for _, table := range []string{"work_reports", "events"} {
		if _, err := f.s.DB.ExecContext(f.ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func copyReading(t *testing.T, reading map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(reading)
	var copied map[string]any
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func compareDevinStage(t *testing.T, f *stageFixture, project string, readings []map[string]any) map[string]any {
	t.Helper()
	python := devinPythonStage(t, f, project, readings)
	answer, err := f.c.StageStandingWithObservations(context.Background(), project, readings, f.at)
	if err != nil {
		t.Fatal(err)
	}
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
	return answer
}

func addForeignRelationship(t *testing.T, f *stageFixture) {
	t.Helper()
	if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-foreign','BETA-1','active','parent-b','host','child-b','host',1,'[]','[]','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordRelationshipScope(f.ctx, "rel-foreign", "BETA", "t"); err != nil {
		t.Fatal(err)
	}
}

func Test24DevinSelectorConflictMatchesLivePython(t *testing.T) {
	f := devinFixture(t)
	first := omissionReading24(f)
	second := copyReading(t, first)
	second["selectors"].(map[string]any)["workspace"] = "/new"
	answer := compareDevinStage(t, f, "PRJ-1", []map[string]any{first, second})
	want := []any{map[string]any{"obligationId": ObservationObligation(first).ID, "kind": "unreported", "reason": "contradictory_observation", "detail": "2 readings of this omission disagree about what it is or where it can be read, and a report carries exactly one; nothing was staged for it"}}
	if !reflect.DeepEqual(answer["refused"], want) || len(answer["staged"].([]any)) != 0 {
		t.Fatalf("answer %#v", answer)
	}
}

func Test24DevinForeignRelationshipMatchesLivePython(t *testing.T) {
	f := devinFixture(t)
	addForeignRelationship(t, f)
	foreign := omissionReading24(f)
	foreign["relationshipId"] = "rel-foreign"
	foreign["selectors"].(map[string]any)["turn"] = "turn-foreign"
	answer := compareDevinStage(t, f, "PRJ-1", []map[string]any{foreign})
	if len(answer["staged"].([]any)) != 0 || len(answer["refused"].([]any)) != 0 || len(answer["gaps"].([]any)) != 0 {
		t.Fatalf("foreign reading was not ignored: %#v", answer)
	}
}

type devinReadbackCase struct {
	name      string
	uncertain bool
	named     string
	holder    string
	turns     map[string]float64
}

// devinTokens substitutes the delivery tokens Go's attempts drew at random, which Python's
// readback on a copy of Go's store names.
func devinTokens(t *testing.T, f *stageFixture) []pyoracle.Option {
	t.Helper()
	rows, err := f.s.DB.QueryContext(f.ctx, "SELECT delivery_token FROM supervisor_attempts ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var options []pyoracle.Option
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		options = append(options, pyoracle.Substitute(token, fmt.Sprintf("<delivery token %d>", len(options)+1)))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return options
}

func devinPythonReadback(t *testing.T, f *stageFixture, messageID string, tc devinReadbackCase) supervisorCapture {
	t.Helper()
	turns, err := json.Marshal(tc.turns)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `
import json,sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.hostadapter import TokenScan,TurnInfo
from codex_session_relay.linkage import Linkage
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
from codex_session_relay.supervisorchannel import SupervisorChannel,supervisor_read_proof
class Host:
 def __init__(self,turns,holder): self.turns=turns; self.holder=holder
 def read_turn(self,thread,turn):
  at=self.turns.get(turn)
  return None if at is None else TurnInfo(turn,'completed',at)
 def find_token(self,thread,token,limit=200,turn_id=None,message_only=False):
  return TokenScan(True,self.holder,True,1)
store=Store(sys.argv[1]); clock=FakeClock()
channel=SupervisorChannel(store,Registry(store,clock),Linkage(store,clock),clock)
answer=channel.read_back(sys.argv[2],read_turn_id=sys.argv[3],proof=supervisor_read_proof(sys.argv[2],sys.argv[3]),adapter=Host(json.loads(sys.argv[5]),sys.argv[4]))
tables={}
for row in store.all("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
 name=row['name']; values=[dict(one) for one in store.all('SELECT * FROM '+name+' ORDER BY rowid')]
 if values: tables[name]=values
print(json.dumps({'captures':[answer],'problems':[],'tables':tables},sort_keys=True))
store.close()
`
	// Python answers on a copy it owns of the store as it stands (recorded, pythonOutput).
	out := pythonOutput(t, pyKey(t, "readback"), func() ([]byte, error) {
		copyPath := filepath.Join(t.TempDir(), "relay.sqlite3")
		if _, err := f.s.DB.ExecContext(f.ctx, "VACUUM INTO ?", copyPath); err != nil {
			return nil, err
		}
		ownCopied(t, copyPath, "python")
		cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), "-c", script, copyPath, messageID, tc.named, tc.holder, string(turns))
		home := t.TempDir()
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_DATA_HOME="+home, "XDG_CONFIG_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm", "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("live Python readback: %v\n%s", err, out)
		}
		return out, nil
	}, append(devinTokens(t, f), pyoracle.Substitute(f.root, "<fixture>"))...)
	var capture supervisorCapture
	if err := json.Unmarshal(out, &capture); err != nil {
		t.Fatalf("live Python JSON: %v\n%s", err, out)
	}
	return capture
}

func compareDevinReadback(t *testing.T, tc devinReadbackCase) map[string]any {
	t.Helper()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	h := &sendHost{status: "idle"}
	if tc.uncertain {
		h.outcome = "unknown"
	}
	if _, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000); err != nil {
		t.Fatal(err)
	}
	python := devinPythonReadback(t, f, id, tc)
	h.turns = tc.turns
	h.items = map[string]string{tc.holder: h.sends[0]}
	answer, err := f.c.ReadBack(f.ctx, id, tc.named, Proof(id, tc.named), "", h, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
	return answer
}

func Test24DevinReadbackChronologyMatchesLivePython(t *testing.T) {
	cases := []devinReadbackCase{
		{name: "F1 unrelated holder predates transport", named: "turn-read", holder: "turn-old", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-old": 1_699_999_990}},
		{name: "neighbor token in named turn", named: "turn-read", holder: "turn-read", turns: map[string]float64{"turn-read": 1_700_000_010}},
		{name: "neighbor token in transport-reported steered turn", named: "turn-read", holder: "turn-supervisor-1", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-supervisor-1": 1_699_999_990}},
		{name: "neighbor holder time unavailable", named: "turn-read", holder: "turn-missing", turns: map[string]float64{"turn-read": 1_700_000_010}},
		{name: "F2 uncertain holder 0.5ms before transport", uncertain: true, named: "turn-read", holder: "turn-holder", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-holder": 1_699_999_999.9995}},
		{name: "neighbor uncertain holder exactly at transport", uncertain: true, named: "turn-read", holder: "turn-holder", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-holder": 1_700_000_000}},
		{name: "neighbor uncertain holder 1ms before transport", uncertain: true, named: "turn-read", holder: "turn-holder", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-holder": 1_699_999_999.999}},
		{name: "neighbor uncertain holder missing", uncertain: true, named: "turn-read", holder: "turn-missing", turns: map[string]float64{"turn-read": 1_700_000_010}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { compareDevinReadback(t, tc) })
	}
}

func Test24DevinStageStandingOrderMatchesLivePython(t *testing.T) {
	f := fixture24(t)
	first := omissionReading24(f)
	first["executionGeneration"] = 1
	first["selectors"].(map[string]any)["turn"] = "turn-omission-a"
	second := copyReading(t, first)
	second["executionGeneration"] = 1
	second["selectors"].(map[string]any)["turn"] = "turn-omission-c"
	python := devinPythonStage(t, f, "PRJ-1", []map[string]any{first, second})
	answer, err := f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{first, second}, f.at)
	if err != nil {
		t.Fatal(err)
	}
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)

	var firstBytes []byte
	for run := 0; run < 20; run++ {
		runFixture := fixture24(t)
		a := omissionReading24(runFixture)
		a["executionGeneration"] = 1
		a["selectors"].(map[string]any)["turn"] = "turn-omission-a"
		c := copyReading(t, a)
		c["executionGeneration"] = 1
		c["selectors"].(map[string]any)["turn"] = "turn-omission-c"
		answer, err := runFixture.c.StageStandingWithObservations(runFixture.ctx, "PRJ-1", []map[string]any{a, c}, runFixture.at)
		if err != nil {
			t.Fatal(err)
		}
		var journal []string
		rows, err := runFixture.s.DB.QueryContext(runFixture.ctx, "SELECT subject FROM journal WHERE kind='supervisor_report' ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var subject string
			if err := rows.Scan(&subject); err != nil {
				t.Fatal(err)
			}
			journal = append(journal, subject)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		bytes, _ := json.Marshal(map[string]any{"answer": answer, "journal": journal})
		bytes = []byte(strings.ReplaceAll(string(bytes), runFixture.root, "<ROOT>"))
		if run == 0 {
			firstBytes = bytes
		} else if string(bytes) != string(firstBytes) {
			t.Errorf("run %d is nondeterministic\nfirst: %s\nnow: %s", run, firstBytes, bytes)
		}
	}
}

func moveDevinSupervisor(t *testing.T, f *stageFixture) {
	t.Helper()
	if err := f.s.ArchiveScopeBinding(f.ctx, "b-supervisor", "archived", "b-successor", f.at); err != nil {
		t.Fatal(err)
	}
	if err := f.s.InsertScopeBinding(f.ctx, store.ScopeBindingsRow{BindingID: "b-successor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "successor", HostID: "host", Status: "active", Revision: 2, CreatedAt: "t2", UpdatedAt: "t2"}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RepointScopeLink(f.ctx, "lnk-project", "active", "parent", "successor", f.at); err != nil {
		t.Fatal(err)
	}
}

func devinPythonRestage(t *testing.T, f *stageFixture, reading map[string]any) supervisorCapture {
	t.Helper()
	raw, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `
import json,sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.linkage import Linkage
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
from codex_session_relay import supervision,supervisorchannel
store=Store(sys.argv[1]); clock=FakeClock()
supervisorchannel.relay_program=lambda: ('/usr/bin/codex-session-relay',)
channel=supervisorchannel.SupervisorChannel(store,Registry(store,clock),Linkage(store,clock),clock,state_directory=sys.argv[2])
channel._omission_withdrawn=lambda obligation,reading: None
reading=json.loads(sys.argv[3]); obligation=supervision.from_observation(reading)
try:
 answer={'ok':channel.stage(obligation,reading=reading)}
except Exception as error:
 answer={'error':error.__class__.__name__,'reason':getattr(getattr(error,'reason',None),'value',None),'detail':getattr(error,'detail',str(error))}
tables={}
for row in store.all("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
 name=row['name']; values=[dict(one) for one in store.all('SELECT * FROM '+name+' ORDER BY rowid')]
 if values: tables[name]=values
print(json.dumps({'captures':[answer],'problems':[],'tables':tables},sort_keys=True))
store.close()
`
	// Python answers on a copy it owns of the store as it stands (recorded, pythonOutput).
	out := pythonOutput(t, pyKey(t, "restage"), func() ([]byte, error) {
		copyPath := filepath.Join(t.TempDir(), "relay.sqlite3")
		if _, err := f.s.DB.ExecContext(f.ctx, "VACUUM INTO ?", copyPath); err != nil {
			return nil, err
		}
		ownCopied(t, copyPath, "python")
		cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), "-c", script, copyPath, filepath.Join(f.root, "state"), string(raw))
		home := t.TempDir()
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_DATA_HOME="+home, "XDG_CONFIG_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm", "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("live Python restage: %v\n%s", err, out)
		}
		return out, nil
	}, pyoracle.Substitute(f.root, "<fixture>"))
	var capture supervisorCapture
	if err := json.Unmarshal(out, &capture); err != nil {
		t.Fatalf("live Python JSON: %v\n%s", err, out)
	}
	return capture
}

func compareDevinRestage(t *testing.T, moved, changed bool) map[string]any {
	t.Helper()
	f := fixture24(t)
	original := omissionReading24(f)
	original["executionGeneration"] = 1
	o := ObservationObligation(original)
	if _, err := f.c.StageWithReading(f.ctx, *o, original, "", f.at); err != nil {
		t.Fatal(err)
	}
	if moved {
		moveDevinSupervisor(t, f)
	}
	incoming := copyReading(t, original)
	incoming["executionGeneration"] = 1
	if changed {
		incoming["selectors"].(map[string]any)["markerRoot"] = "/markers/b"
	}
	python := devinPythonRestage(t, f, incoming)
	result, err := f.c.StageWithReading(f.ctx, *o, incoming, "", f.at)
	var answer map[string]any
	if err != nil {
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatal(err)
		}
		answer = map[string]any{"error": "DeliveryRefused", "reason": refusal.Reason, "detail": refusal.Detail}
	} else {
		answer = map[string]any{"ok": result}
	}
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
	return answer
}

func Test24DevinRepeatedReadingObservedAtMatchesLivePython(t *testing.T) {
	f := fixture24(t)
	original := omissionReading24(f)
	original["executionGeneration"] = 1
	o := ObservationObligation(original)
	if _, err := f.c.StageWithReading(f.ctx, *o, original, "", f.at); err != nil {
		t.Fatal(err)
	}
	incoming := copyReading(t, original)
	incoming["observedAt"] = "2023-11-14T22:14:20.000000+00:00"
	python := devinPythonRestage(t, f, incoming)
	result, err := f.c.StageWithReading(f.ctx, *o, incoming, "", f.at)
	if err != nil {
		t.Fatal(err)
	}
	compareSupervisorValues(t, []any{map[string]any{"ok": result}}, python)
	compareSupervisorTables(t, f.s, python)
}

func Test24DevinFrozenReadingCheckedBeforeReaddressMatchesLivePython(t *testing.T) {
	for _, tc := range []struct {
		name           string
		moved, changed bool
	}{{"moved identical reading", true, false}, {"moved different reading", true, true}, {"unmoved different reading", false, true}} {
		t.Run(tc.name, func(t *testing.T) { compareDevinRestage(t, tc.moved, tc.changed) })
	}
}

func devinPythonStageUnsent(t *testing.T, f *stageFixture, project string, readings []map[string]any) supervisorCapture {
	t.Helper()
	raw, _ := json.Marshal(readings)
	repo, _ := filepath.Abs("../../..")
	script := `
import json,sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.linkage import Linkage
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
from codex_session_relay import supervisorchannel
store=Store(sys.argv[1]); clock=FakeClock()
supervisorchannel.relay_program=lambda: ('/usr/bin/codex-session-relay',)
channel=supervisorchannel.SupervisorChannel(store,Registry(store,clock),Linkage(store,clock),clock,state_directory=sys.argv[2])
readings=json.loads(sys.argv[3]); channel.store_readings=lambda project: readings; channel._omission_withdrawn=lambda obligation,reading: None
answer=channel.stage_unsent(sys.argv[4])
tables={}
for row in store.all("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
 name=row['name']; values=[dict(one) for one in store.all('SELECT * FROM '+name+' ORDER BY rowid')]
 if values: tables[name]=values
print(json.dumps({'captures':[answer],'problems':[],'tables':tables},sort_keys=True)); store.close()
`
	// Python answers on a copy it owns of the store as it stands (recorded, pythonOutput).
	out := pythonOutput(t, pyKey(t, "stage_unsent"), func() ([]byte, error) {
		copyPath := filepath.Join(t.TempDir(), "relay.sqlite3")
		if _, err := f.s.DB.ExecContext(f.ctx, "VACUUM INTO ?", copyPath); err != nil {
			return nil, err
		}
		ownCopied(t, copyPath, "python")
		cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), "-c", script, copyPath, filepath.Join(f.root, "state"), string(raw), project)
		home := t.TempDir()
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_DATA_HOME="+home, "XDG_CONFIG_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm", "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("live Python stage_unsent: %v\n%s", err, out)
		}
		return out, nil
	}, pyoracle.Substitute(f.root, "<fixture>"))
	var capture supervisorCapture
	if err := json.Unmarshal(out, &capture); err != nil {
		t.Fatalf("live Python JSON: %v\n%s", err, out)
	}
	return capture
}

func Test24DevinStageUnsentFiltersEveryStandingObligationLikeLivePython(t *testing.T) {
	for _, tc := range []struct {
		name, state, hold string
		confirmed         bool
	}{{"dispatched unconfirmed", "dispatched", "", false}, {"sending", "sending", "", false}, {"dispatched confirmed", "dispatched", "", true}, {"held", "queued", "hierarchy_unresolved", false}, {"failed retryable", "deferred_busy", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, reading, project := snapshotFixture(t, "plain")
			relation := reading["relationshipId"].(string)
			if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-f6',?,1,'abc123456789abcdef','ready_for_review','child','child','turn-f6','completed','{}',?,?)`, relation, f.at, f.at); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-f6',1,?,1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','done','merge',?)`, relation, f.at); err != nil {
				t.Fatal(err)
			}
			o := ObservationObligation(reading)
			if _, err := f.c.StageWithReading(f.ctx, *o, reading, "", f.at); err != nil {
				t.Fatal(err)
			}
			event, err := f.c.FromEvent(f.ctx, "event-f6")
			if err != nil || event == nil {
				t.Fatalf("event: %v", err)
			}
			if _, err := f.c.Stage(f.ctx, *event, "", f.at); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET state=?,hold_reason=?", tc.state, nullString(tc.hold)); err != nil {
				t.Fatal(err)
			}
			if tc.confirmed {
				for _, query := range []string{"INSERT INTO sync_targets(relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')", "INSERT INTO sync_outbox(sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) SELECT 'sync-1',relationship_id,issue_key,'coordination_document','doc-1','verdict','event-f6','digest','summary','confirmed','t','t' FROM relationships LIMIT 1"} {
					if _, err := f.s.DB.ExecContext(f.ctx, query); err != nil {
						t.Fatal(err)
					}
				}
			}
			python := devinPythonStageUnsent(t, f, project, []map[string]any{reading})
			answer, err := f.c.stageUnsentWithReadings(f.ctx, project, f.at, []map[string]any{reading})
			if err != nil {
				t.Fatal(err)
			}
			compareSupervisorValues(t, []any{answer}, python)
			compareSupervisorTables(t, f.s, python)
		})
	}
}

func Test24DevinObservationNeighboursMatchLivePython(t *testing.T) {
	t.Run("same selectors and irrelevant metadata", func(t *testing.T) {
		f := devinFixture(t)
		first := omissionReading24(f)
		second := copyReading(t, first)
		second["irrelevant"] = map[string]any{"changed": true}
		answer, err := f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{first, second}, f.at)
		if err != nil {
			t.Fatal(err)
		}
		if len(answer["staged"].([]any)) != 1 || len(answer["refused"].([]any)) != 0 {
			t.Fatalf("identical reading did not converge: %#v", answer)
		}
	})
	t.Run("malformed foreign reading", func(t *testing.T) {
		f := devinFixture(t)
		addForeignRelationship(t, f)
		foreign := omissionReading24(f)
		foreign["relationshipId"] = "rel-foreign"
		foreign["selectors"] = []any{"malformed"}
		answer := compareDevinStage(t, f, "PRJ-1", []map[string]any{foreign})
		if len(answer["staged"].([]any)) != 0 || len(answer["refused"].([]any)) != 0 || len(answer["gaps"].([]any)) != 0 {
			t.Fatalf("malformed foreign reading was not ignored: %#v", answer)
		}
	})
	t.Run("own and foreign readings", func(t *testing.T) {
		f := devinFixture(t)
		addForeignRelationship(t, f)
		own := omissionReading24(f)
		foreign := copyReading(t, own)
		foreign["relationshipId"] = "rel-foreign"
		foreign["selectors"].(map[string]any)["turn"] = "turn-foreign"
		answer, err := f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{foreign, own}, f.at)
		if err != nil {
			t.Fatal(err)
		}
		if len(answer["staged"].([]any)) != 1 || len(answer["refused"].([]any)) != 0 {
			t.Fatalf("mixed readings: %#v", answer)
		}
	})
}
