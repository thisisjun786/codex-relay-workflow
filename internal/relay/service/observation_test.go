package service

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// observationSeed is the fixture both runtimes' stores hold before the daemon ticks.
var observationSeed = []string{
	"INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES('r','REL-1','active','parent','host','/parent','child','host','/child',1,'[]','[\"parent\"]','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
	"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('r',1,'dispatch','bound','anchor','initial_assignment','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
}

const observationStaged = "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at,stage) VALUES('staged-event','r',1,'revision','ready_for_review','child','child','anchor','inProgress','{}','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z','staged')"

// seedObservation writes the fixture through the store writer of the runtime under test, into
// the store that runtime owns: each store is its own runtime's from creation, so the two
// stores' schema_meta differ by the owner alone.
func seedObservation(t *testing.T, home, socket string, staged, python bool) {
	t.Helper()
	statements := append([]string{}, observationSeed...)
	if staged {
		statements = append(statements, observationStaged)
	}
	path := home + "/state/relay.sqlite3"
	if !python {
		prepareParityOwnership(t, home, false, []string{"--socket", socket})
		defer inRuntimeScope(t, home)()
		s, err := store.Open(context.Background(), path, socket)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range statements {
			if _, err = s.DB.Exec(statement); err != nil {
				t.Fatal(errors.Join(err, s.Close()))
			}
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	encoded, err := json.Marshal(statements)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(testRoot, ".venv/bin/python"), "-c", `import json, sys
from codex_session_relay.store import Store
s=Store(sys.argv[1], socket_path=sys.argv[2])
try:
 for statement in json.loads(sys.argv[3]):
  s.db.execute(statement)
finally:
 s.close()
`, path, socket, string(encoded))
	cmd.Env = environment(home)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v %s", err, raw)
	}
}
func Test29ObservationConsoleTables(t *testing.T) {
	invokeFixed := fixedClockRuntime(t)
	for _, scenario := range []string{"completed", "absent", "failed", "interrupted", "staged-completed", "staged-failed"} {
		t.Run(scenario, func(t *testing.T) {
			status := strings.TrimPrefix(scenario, "staged-")
			staged := strings.HasPrefix(scenario, "staged-")
			home := t.TempDir()
			host := fakehost.Start(t)
			host.Handle("thread/turns/list", func(json.RawMessage) fakehost.Reply {
				turns := []any{}
				if status != "absent" {
					turns = append(turns, map[string]any{"id": "anchor", "status": status})
				}
				return fakehost.Reply{Result: map[string]any{"data": turns, "nextCursor": nil}}
			})
			host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
				return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "parent", "status": map[string]any{"type": "active"}, "canAcceptDirectInput": true}}}
			})
			host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
				var p map[string]any
				if err := json.Unmarshal(raw, &p); err != nil {
					panic(err)
				}
				data := []any{}
				if p["archived"] != true {
					data = append(data, map[string]any{"id": "parent"})
				}
				return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
			})
			host.Handle("thread/goal/get", func(json.RawMessage) fakehost.Reply { return fakehost.Reply{Result: map[string]any{"goal": nil}} })
			args := []string{"--socket", host.SocketPath, "daemon", "--max-ticks", "1", "--allow-isolated-scope"}
			seedObservation(t, home, host.SocketPath, staged, true)
			want := invokeFixed(home, true, args)
			wt := tables(t, home, testsupport.Python)
			wf := files(t, home, testsupport.Python)
			resetRuntime(t, home)
			seedObservation(t, home, host.SocketPath, staged, false)
			got := invokeFixed(home, false, args)
			gt := tables(t, home, testsupport.Go)
			gf := files(t, home, testsupport.Go)
			compare(t, want, got)
			if wt != gt {
				t.Fatalf("table byte difference\nPython %s\nGo %s", wt, gt)
			}
			for name, value := range wf {
				if value != gf[name] {
					t.Fatalf("%s\nPython %s\nGo %s", name, value, gf[name])
				}
			}
		})
	}
}
