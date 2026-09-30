package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// A real withheld delivery and its settled settings refusal: compare the complete
// derived observation against the Python faultsweep source, including its recovery.
func Test22_SettingsHoldWholePythonObservation(t *testing.T) {
	for _, held := range []bool{true, false} {
		t.Run(fmt.Sprint(held), func(t *testing.T) { testSettingsHoldWholePythonObservation(t, held) })
	}
}
func testSettingsHoldWholePythonObservation(t *testing.T, held bool) {
	home := t.TempDir()
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	seed := func(dir string) *store.Store {
		s, e := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		state, hold, attempts := "withheld_pre_send", any("attempt_cap"), 3
		if !held {
			state, hold, attempts = "queued", nil, 1
		}
		_, e = s.Q(context.Background()).ExecContext(context.Background(), `INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,hold_reason,attempt_count,created_at,updated_at) VALUES('evt1','rel','completion','parent','thread',?,?,?,'now','now')`, state, hold, attempts)
		if e != nil {
			t.Fatal(e)
		}
		for _, statement := range []string{
			`INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('req1','evt1',1,'completion','settled','settings_rejected','now')`,
			`INSERT INTO journal(at,kind,subject,detail) VALUES('now','delivery_attempted','evt1','{"requestId":"req1","settingsRefusal":{"reason":"settings_not_preserved","field":"runtimeWorkspaceRoots"}}')`,
		} {
			if _, e = s.Q(context.Background()).ExecContext(context.Background(), statement); e != nil {
				t.Fatal(e)
			}
		}
		return s
	}
	gs := seed(filepath.Join(home, "go"))
	defer gs.Close()
	script := `import json,sys
from codex_session_relay.store import Store
from codex_session_relay import faultsweep
s=Store(sys.argv[1]); old=faultsweep.installation
faultsweep.installation=lambda: {'package':'codex-session-relay','version':'test','location':'test','revision':None,'revisionRecord':sys.argv[2],'revisionReason':'no host record at '+sys.argv[2]}
try:
 page=(faultsweep.delivery_faults if sys.argv[3]=='held' else faultsweep.retry_faults)(s,product='crw',scope={})
 print(json.dumps(page,sort_keys=True))
finally: faultsweep.installation=old;s.close()`
	output := pyAnswer(t, "faultsweep page", nil, pyRunPaths(t, home), func() ([]byte, error) {
		ps := seed(filepath.Join(home, "py"))
		ps.Close()
		// Go seeded Python's store as well; Python reads it after a takeover.
		testsupport.HandOver(t, filepath.Join(home, "py", "relay.sqlite3"), "python")
		cmd := exec.Command("uv", "run", "--no-sync", "python", "-c", script, filepath.Join(home, "py", "relay.sqlite3"), filepath.Join(home, "state", "codex-relay-workflow", "host-record.json"), func() string {
			if held {
				return "held"
			}
			return "retry"
		}())
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
		output, e := cmd.CombinedOutput()
		if e != nil {
			return nil, fmt.Errorf("Python %v %s", e, output)
		}
		return output, nil
	})
	var want map[string]any
	if e := json.Unmarshal(output, &want); e != nil {
		t.Fatal(e)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	sw := &Sweeper{Store: gs, HostRecordPath: testHostRecordPath(), MaxAttempts: 3, Current: func(context.Context, string) (bool, error) { return true, nil }, Installation: Installation{Package: "codex-session-relay", Version: "test", Location: "test"}, Program: func() []string { return []string{"codex-session-relay"} }}
	var got page
	if held {
		got, e = sw.deliveryFaults(context.Background(), "crw", nil)
	} else {
		got, e = sw.retryFaults(context.Background(), "crw", nil)
	}
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(got.observations)
	if e != nil {
		t.Fatal(e)
	}
	var observed any
	if e = json.Unmarshal(raw, &observed); e != nil {
		t.Fatal(e)
	}
	goObservations := observed.([]any)
	pyObservations := want["observations"].([]any)
	if len(goObservations) != len(pyObservations) {
		t.Fatalf("Go %d observations, Python %d", len(goObservations), len(pyObservations))
	}
	for i, raw := range goObservations {
		g := raw.(map[string]any)
		p := pyObservations[i].(map[string]any)
		for _, pair := range [][2]string{{"Product", "product"}, {"FaultClass", "faultClass"}, {"Signature", "signature"}, {"OccurrenceKey", "occurrenceKey"}, {"Scope", "scope"}, {"Detail", "detail"}, {"Evidence", "evidence"}, {"Cleared", "cleared"}} {
			if pair[0] == "Evidence" {
				ge := g[pair[0]].([]any)
				pe := p[pair[1]].([]any)
				if len(ge) != len(pe) {
					t.Fatalf("Go %d evidence items, Python %d", len(ge), len(pe))
				}
				for n := range ge {
					ga := ge[n].(map[string]any)
					pa := pe[n].(map[string]any)
					if ga["kind"] == "recovery" {
						goRecovery := ga["observed"].(map[string]any)
						pyRecovery := pa["observed"].(map[string]any)
						// The program and isolated store directories differ; compare each supported command's arguments separately.
						goRecovery["command"] = strings.TrimPrefix(goRecovery["command"].(string), sw.Program()[0]+" --state "+filepath.Join(home, "go"))
						pyCommand := pyRecovery["command"].(string)
						pyRecovery["command"] = pyCommand[strings.Index(pyCommand, " show --event "):]
					}
				}
			}
			if !reflect.DeepEqual(g[pair[0]], p[pair[1]]) {
				t.Fatalf("%s: Go %v Python %v", pair[1], g[pair[0]], p[pair[1]])
			}
		}
	}
}
