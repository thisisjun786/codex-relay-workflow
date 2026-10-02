package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// managedCLI is the built relay CLI over a fake app-server, for requests whose refusals must not reach the
// app-server's thread/start.
type managedCLI struct {
	t        *testing.T
	state    string
	scope    string
	marker   string
	settings map[string]any
	host     *fakehost.Server
}

func newManagedCLI(t *testing.T) *managedCLI {
	t.Helper()
	root := t.TempDir()
	c := &managedCLI{t: t, state: filepath.Join(root, "state"), scope: filepath.Join(root, "scopes"), marker: filepath.Join(root, "markers")}
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", c.scope)
	workspace := filepath.Join(root, "work")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, policyPath)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	c.settings = map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	c.host = fakehost.Start(t)
	response := func() map[string]any {
		r := map[string]any{}
		for k, v := range c.settings {
			if k != "environments" {
				r[k] = v
			}
		}
		r["thread"] = map[string]any{"id": "managed-child", "environments": c.settings["environments"]}
		return r
	}
	var mu sync.Mutex
	turns := 0
	c.host.Respond("thread/start", fakehost.Reply{Result: response()})
	c.host.Respond("thread/name/set", fakehost.Reply{Result: map[string]any{}})
	c.host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		turns++
		id := "standby"
		if turns > 1 {
			id = "business"
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": id}}}
	})
	c.host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true, "model": "gpt-5.4", "reasoningEffort": "medium", "cwd": workspace}}})
	c.host.Respond("thread/resume", fakehost.Reply{Result: response()})
	c.host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	c.host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "standby", "status": "completed"}}, "nextCursor": nil}})
	c.host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		data := []any{}
		if params["archived"] != true {
			data = append(data, map[string]any{"id": "managed-child"})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	s, err := store.Open(context.Background(), filepath.Join(c.state, "relay.sqlite3"), c.host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	publishWorker(t, c.state, c.host.SocketPath, c.scope, filepath.Dir(suiteBinary))
	return c
}

// request is a managed-start request for the CLI's workspace; change edits it before it is sent.
func (c *managedCLI) request(change func(map[string]any)) []byte {
	c.t.Helper()
	workspace := c.settings["cwd"].(string)
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-pre", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": c.settings}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": c.settings}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	if change != nil {
		change(request)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		c.t.Fatal(err)
	}
	return raw
}

// start runs managed-start and returns what it printed and its exit code.
func (c *managedCLI) start(raw []byte) (stdout, stderr string, code int) {
	c.t.Helper()
	var out, errOut bytes.Buffer
	cmd := exec.Command(suiteBinary, "relay", "--state", c.state, "--socket", c.host.SocketPath, "managed-start", "--request", string(raw), "--marker-root", c.marker)
	cmd.Env = append(os.Environ(), "CODEX_SESSION_RELAY_SCOPE_DIR="+c.scope)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			c.t.Fatalf("managed-start: %v %s", err, &errOut)
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code
}

// withStore runs f over the CLI's store, closed again before the next command.
func (c *managedCLI) withStore(f func(*store.Store)) {
	c.t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(c.state, "relay.sqlite3"), c.host.SocketPath)
	if err != nil {
		c.t.Fatal(err)
	}
	defer s.Close()
	f(s)
}

// Requests that registration or the settings record refuse once the task exists are refused before the
// app-server is asked to start a thread, and answer on the built CLI as they always did: the same printed
// answer and the same exit code. On the baseline commit each of these printed the same text and exited
// with the same code after the app-server had been asked to start one thread.
func Test28_ManagedStartRefusesBeforeCreation(t *testing.T) {
	recordParentSettings := func(c *managedCLI) {
		c.withStore(func(s *store.Store) {
			recorded := map[string]any{}
			for k, v := range c.settings {
				recorded[k] = v
			}
			recorded["model"] = "gpt-other"
			recorded["citedRole"] = "parent"
			if _, err := managed.EnsureSettings(context.Background(), s, "parent", recorded, "earlier_registration", "2026-09-26T00:00:00.000000+00:00"); err != nil {
				t.Fatal(err)
			}
		})
	}
	cases := []struct {
		name    string
		change  func(map[string]any)
		prepare func(*managedCLI)
		code    int
		stdout  string
	}{
		{name: "issue key holding the separator", change: func(r map[string]any) { r["issueKey"] = "REL|MANAGED" },
			code: 3, stdout: "{\n  \"error\": \"host\",\n  \"detail\": \"issue_key must not contain '|', which is the field separator\"\n}\n"},
		{name: "parent task id holding the separator", change: func(r map[string]any) {
			r["parent"].(map[string]any)["taskId"] = "par|ent"
			r["allowedRecipients"] = []any{"par|ent"}
		}, code: 3, stdout: "{\n  \"error\": \"host\",\n  \"detail\": \"parent_task_id must not contain '|', which is the field separator\"\n}\n"},
		{name: "host id holding the separator under a project", change: func(r map[string]any) {
			r["projectKey"] = "P-SCOPE"
			r["parent"].(map[string]any)["hostId"] = "ho|st"
			r["child"].(map[string]any)["hostId"] = "ho|st"
		}, prepare: func(c *managedCLI) {
			c.withStore(func(s *store.Store) {
				reg := &registry.Registry{Store: s, Now: registry.SystemISO}
				if _, err := reg.BindScopeAs(context.Background(), "parent", "P-SCOPE", registry.Endpoint{TaskID: "parent", HostID: "host"}, registry.Active); err != nil {
					t.Fatal(err)
				}
			})
		}, code: 2, stdout: "{\n  \"error\": \"refused\",\n  \"reason\": \"unregistered_scope\",\n  \"detail\": \"the child host id must not contain '|', which is the field separator\"\n}\n"},
		{name: "parent settings that differ from the recorded ones", prepare: recordParentSettings,
			code: 2, stdout: "{\n  \"error\": \"refused\",\n  \"reason\": \"relationship_conflict\",\n  \"detail\": \"'parent' already has execution settings that differ from this record; ensure_only does not overwrite them\"\n}\n"},
		// The one answer that changes. A rival owner of the issue is refused with the child's own binding
		// (duplicate_scope_owner on the baseline, after the thread existed), which cannot be asked before
		// the child exists; the settings refusal can, so it answers first, with the code it always had.
		{name: "differing settings before a rival owner of the issue", change: func(r map[string]any) { r["projectKey"] = "P-SCOPE" }, prepare: func(c *managedCLI) {
			recordParentSettings(c)
			c.withStore(func(s *store.Store) {
				reg := &registry.Registry{Store: s, Now: registry.SystemISO}
				if _, err := reg.BindScopeAs(context.Background(), "parent", "P-SCOPE", registry.Endpoint{TaskID: "parent", HostID: "host"}, registry.Active); err != nil {
					t.Fatal(err)
				}
				if _, err := reg.BindScopeAs(context.Background(), "child", "REL-MANAGED", registry.Endpoint{TaskID: "rival", HostID: "host"}, registry.Active); err != nil {
					t.Fatal(err)
				}
			})
		}, code: 2, stdout: "{\n  \"error\": \"refused\",\n  \"reason\": \"relationship_conflict\",\n  \"detail\": \"'parent' already has execution settings that differ from this record; ensure_only does not overwrite them\"\n}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := newManagedCLI(t)
			if c.prepare != nil {
				c.prepare(cli)
			}
			stdout, stderr, code := cli.start(cli.request(c.change))
			if code != c.code || stdout != c.stdout || stderr != "" {
				t.Fatalf("exit %d stdout %q stderr %q, want exit %d stdout %q", code, stdout, stderr, c.code, c.stdout)
			}
			if n := cli.host.Count("thread/start"); n != 0 {
				t.Fatalf("the refused request asked the app-server to start %d threads", n)
			}
		})
	}
}
