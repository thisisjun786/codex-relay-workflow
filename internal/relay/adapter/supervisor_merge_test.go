package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

type supervisorRun struct {
	Code   int
	Stdout string
	Stderr string
}

func seedSupervisorCLI(t *testing.T, state, socket string) string {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, stmt := range []string{
		`INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-1','REL-1','active','parent','host','child','host',1,'[]','[]','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-1','rel-1',1,'abc123456789abcdef','ready_for_review','child','child','turn-1','completed','{}','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',1,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done','merge','2023-11-14T22:13:20.000000+00:00')`,
		`INSERT INTO authorized_settings(task_id,settings,source,recorded_at) VALUES ('supervisor','{"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false},"approvalPolicy":"never","cwd":"/workspace/example/codex-session-relay/relay-core","runtimeWorkspaceRoots":["/workspace/example/codex-session-relay/relay-core"],"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh","environments":[{"environmentId":"local","cwd":"/workspace/example/codex-session-relay/relay-core","runtimeWorkspaceRoots":["/workspace/example/codex-session-relay/relay-core"]}]}','creation_result','2023-11-14T22:13:20.000000+00:00')`,
	} {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := storeseed.RecordRelationshipScope(ctx, s, "rel-1", "PRJ-1", "t"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.ScopeBindingsRow{{BindingID: "b-child", Role: "child", ScopeKind: "issue", ScopeKey: "REL-1", TaskID: "child", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-parent", Role: "parent", ScopeKind: "project", ScopeKey: "PRJ-1", TaskID: "parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-supervisor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "supervisor", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err := storeseed.InsertScopeBinding(ctx, s, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []store.ScopeLinksRow{{LinkID: "lnk-issue", LinkKind: "execution", UpperKind: "project", UpperKey: "PRJ-1", UpperTaskID: "parent", LowerKind: "issue", LowerKey: "REL-1", LowerTaskID: "child", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: "INI-1", UpperTaskID: "supervisor", LowerKind: "project", LowerKey: "PRJ-1", LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err := storeseed.InsertScopeLink(ctx, s, l); err != nil {
			t.Fatal(err)
		}
	}
	channel := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: "codex-session-relay", Socket: socket}
	o, err := channel.FromEvent(ctx, "event-1")
	if err != nil || o == nil {
		t.Fatalf("obligation %v %v", o, err)
	}
	staged, err := channel.Stage(ctx, *o, "", "2023-11-14T22:13:20.000000+00:00")
	if err != nil {
		t.Fatal(err)
	}
	return staged["messageId"].(string)
}

func scriptSupervisorHost(host *fakehost.Server, status string) {
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": status}, "canAcceptDirectInput": true}}})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"status": nil}}})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		data := []any{}
		if params["archived"] == false {
			data = append(data, map[string]any{"id": "supervisor"})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "supervisor-turn"}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "supervisor-turn", "status": "completed", "startedAt": 1700000000}}, "nextCursor": nil}})
	host.Handle("thread/items/list", func(_ json.RawMessage) fakehost.Reply {
		requests := host.Requests()
		message := ""
		for _, request := range requests {
			if request.Method != "turn/start" {
				continue
			}
			var params map[string]any
			_ = json.Unmarshal(request.Params, &params)
			for _, item := range params["input"].([]any) {
				if object, ok := item.(map[string]any); ok {
					message, _ = object["text"].(string)
				}
			}
		}
		return fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"turnId": "supervisor-turn", "item": map[string]any{"id": "item-1", "type": "userMessage", "text": message}}}, "nextCursor": nil}}
	})
}

func runSupervisorBinary(t *testing.T, program, state, socket, message string) []supervisorRun {
	t.Helper()
	prefix := []string{}
	if program == suiteBinary {
		prefix = []string{"relay"}
	}
	proof := sha256.Sum256([]byte(message + "|supervisor-turn"))
	commands := [][]string{{"--state", state, "--socket", socket, "supervisor-send", "--message", message}, {"--state", state, "--socket", socket, "supervisor-read", "--message", message, "--turn", "supervisor-turn", "--proof", hex.EncodeToString(proof[:]), "--as", "supervisor"}}
	var runs []supervisorRun
	for _, argv := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, program, append(prefix, argv...)...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		cancel()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, supervisorRun{code, out.String(), stderr.String()})
	}
	return runs
}

func normalizeSupervisorTables(tables map[string]any, state, socket string) map[string]any {
	delete(tables, "schema_meta")
	var replace func(any) any
	replace = func(value any) any {
		switch v := value.(type) {
		case string:
			v = strings.ReplaceAll(v, state, "STATE")
			return strings.ReplaceAll(v, socket, "SOCKET")
		case []any:
			for i := range v {
				v[i] = replace(v[i])
			}
		}
		return value
	}
	for key, value := range tables {
		tables[key] = replace(value)
	}
	return tables
}

func supervisorTables(t *testing.T, state string) map[string]any {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all := allTables(t, s)
	populated := map[string]any{}
	for name, rows := range all {
		if reflect.ValueOf(rows).Len() > 0 {
			populated[name] = rows
		}
	}
	return populated
}

func Test28_HOST_24_SupervisorSendRead(t *testing.T) {
	shareGoldens(t)
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"supervisor":{"expectation":"record"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
	for _, tc := range []struct {
		name, status string
		unreachable  bool
		refused      bool
	}{{"success", "idle", false, false}, {"busy", "active", false, false}, {"missing", "idle", true, false}, {"refused", "idle", true, true}} {
		for _, mode := range []struct{ name, program string }{{"crw-relay", suiteBinary}, {"codex-session-relay", suiteAlias}} {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				host := fakehost.Start(t)
				if tc.unreachable {
					host.Close()
					if !tc.refused {
						_ = os.Remove(host.SocketPath)
					}
				} else {
					scriptSupervisorHost(host, tc.status)
				}
				state := filepath.Join(t.TempDir(), "go")
				id := seedSupervisorCLI(t, state, host.SocketPath)
				runs := runSupervisorBinary(t, mode.program, state, host.SocketPath, id)
				// The golden holds the tables as JSON reads them back.
				var tables map[string]any
				encoded, err := json.Marshal(normalizeSupervisorTables(supervisorTables(t, state), state, host.SocketPath))
				if err != nil {
					t.Fatal(err)
				}
				if err := decodeNumbers(encoded, &tables); err != nil {
					t.Fatal(err)
				}
				expectJSON(t, "supervisor", map[string]any{"runs": runs, "tables": tables}, golden.Substitute(host.SocketPath, "<host-socket>"))
			})
		}
	}
}
