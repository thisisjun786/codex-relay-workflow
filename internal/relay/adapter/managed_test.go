package adapter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func Test28_ManagedSixMethodsRealSocket(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	// One host, one scope registry root: the store records the scope key of the root it is
	// created under, and the built CLI below serves it under that same root.
	scope := filepath.Join(root, "scopes")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", scope)
	workspace := filepath.Join(root, "work")
	marker := filepath.Join(root, "markers")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	policyRaw := []byte(`{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`)
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, policyRaw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, policyPath)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	policy, err := execution.FromBytes(policyRaw, policyPath)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-real-host", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": settings}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	host := fakehost.Start(t)
	var mu sync.Mutex
	turns := 0
	response := func() map[string]any {
		r := map[string]any{}
		for k, v := range settings {
			if k != "environments" {
				r[k] = v
			}
		}
		r["thread"] = map[string]any{"id": "managed-child", "environments": settings["environments"]}
		return r
	}
	host.Respond("thread/start", fakehost.Reply{Result: response()})
	host.Respond("thread/name/set", fakehost.Reply{Result: map[string]any{}})
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		turns++
		id := "standby"
		if turns > 1 {
			id = "business"
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": id}}}
	})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true, "model": "gpt-5.4", "reasoningEffort": "medium", "cwd": workspace}}})
	host.Respond("thread/resume", fakehost.Reply{Result: response()})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "standby", "status": "completed"}}, "nextCursor": nil}})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
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
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(host.SocketPath, state, Options{Policy: policy, Clock: delivery.NewFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	engine := managed.Start{Store: s, Adapter: Managed{a}, Now: delivery.NewFakeClock().ISO, Socket: host.SocketPath, MarkerRoot: marker, StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := engine.Run(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if field(result, "state") != "admitted" {
		t.Fatalf("managed not admitted: %s", dumps(result, false))
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := contract.Emit(&got, result); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(map[string]any{"state": state, "marker": marker, "socket": host.SocketPath, "request": request})
	if err != nil {
		t.Fatal(err)
	}
	// Python replays on the ledger Go's adapter opened, whose name is a digest of the socket's
	// path; the request fingerprint is a digest of a request naming the test's directories
	// (asGoAnswers). The ledger's device and inode are its file's.
	var goReceipt struct {
		RequestFingerprint string `json:"requestFingerprint"`
		Ledger             struct {
			RealPath string `json:"realPath"`
		} `json:"ledger"`
	}
	if err := json.Unmarshal(got.Bytes(), &goReceipt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(goReceipt.Ledger.RealPath)
	if err != nil {
		t.Fatal(err)
	}
	ledgerFile := info.Sys().(*syscall.Stat_t)
	derived := []pyoracle.Option{
		pyoracle.Substitute(host.SocketPath, "<host-socket>"),
		asGoAnswers(filepath.Base(goReceipt.Ledger.RealPath), "<ledger-file>"),
		asGoAnswers(goReceipt.RequestFingerprint, "<request-fingerprint>"),
		pyoracle.Substitute(fmt.Sprintf(`"device": %d,`, uint64(ledgerFile.Dev)), `"device": "<ledger-device>",`),
		pyoracle.Substitute(fmt.Sprintf(`"inode": %d`, ledgerFile.Ino), `"inode": "<ledger-inode>"`),
	}
	repo := pyRepo(t)
	out := pyOutput(t, "managed_capture.py", func() *exec.Cmd {
		// Python replays the start from the store Go wrote, after a takeover; the built CLI
		// then replays it again after the store is taken back.
		testsupport.HandOver(t, filepath.Join(state, "relay.sqlite3"), "python")
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/managed_capture.py"))
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(spec)
		return cmd
	}, derived...)
	if !bytes.Equal(got.Bytes(), out) {
		t.Fatalf("Go %s\nPython %s", &got, out)
	}
	testsupport.HandOver(t, filepath.Join(state, "relay.sqlite3"), "go")
	binary := suiteBinary
	publishWorker(t, state, host.SocketPath, scope, filepath.Dir(binary))
	cliCmd := exec.Command(binary, "relay", "--state", state, "--socket", host.SocketPath, "managed-start", "--request", string(raw), "--marker-root", marker)
	cliCmd.Env = append(os.Environ(), "CODEX_SESSION_RELAY_SCOPE_DIR="+scope)
	built, err := cliCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("managed CLI %v %s", err, built)
	}
	if !bytes.Equal(built, out) {
		t.Fatalf("built managed receipt %s Python %s", built, out)
	}
}

// An interrupt reaches a running managed-start (backlog before todo 42): the built CLI's first
// SIGINT cancels the command's context (cmd/crw cancelOn), and managed-start hands that context
// to the host, so a business turn/start the host has not answered ends through decision 39's
// caller cancellation: nothing more is sent, the bridge ledger keeps an outcome_unknown receipt
// (Python's CancelledError receipt, bridge_adapter.py), and the process exits through its own
// cleanup with the cancellation as its host error. A context the command dropped left the first
// interrupt ignored until the host answered.
func Test28_ManagedStartEndsOnTheFirstInterrupt(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	scope := filepath.Join(root, "scopes")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", scope)
	workspace := filepath.Join(root, "work")
	marker := filepath.Join(root, "markers")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	policyRaw := []byte(`{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`)
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, policyRaw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, policyPath)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	settings := map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-interrupted", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": settings}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	host := fakehost.Start(t)
	response := func() map[string]any {
		r := map[string]any{}
		for k, v := range settings {
			if k != "environments" {
				r[k] = v
			}
		}
		r["thread"] = map[string]any{"id": "managed-child", "environments": settings["environments"]}
		return r
	}
	// The standby turn is answered; the business turn/start reaches the host and its answer
	// is held back (abandoned once the connection closes).
	var mu sync.Mutex
	turns := 0
	host.Respond("thread/start", fakehost.Reply{Result: response()})
	host.Respond("thread/name/set", fakehost.Reply{Result: map[string]any{}})
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		turns++
		if turns > 1 {
			return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "business"}}, Delay: time.Hour}
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "standby"}}}
	})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true, "model": "gpt-5.4", "reasoningEffort": "medium", "cwd": workspace}}})
	host.Respond("thread/resume", fakehost.Reply{Result: response()})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "standby", "status": "completed"}}, "nextCursor": nil}})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
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
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	publishWorker(t, state, host.SocketPath, scope, filepath.Dir(suiteBinary))
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(suiteBinary, "relay", "--state", state, "--socket", host.SocketPath, "managed-start", "--request", string(raw), "--marker-root", marker)
	cmd.Env = append(os.Environ(), "CODEX_SESSION_RELAY_SCOPE_DIR="+scope)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	business := make(chan error, 1)
	reached, stopWaiting := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopWaiting()
	go func() { business <- host.WaitCount(reached, "turn/start", 2) }()
	select {
	case err := <-business:
		if err != nil {
			_ = cmd.Process.Kill()
			<-exited
			t.Fatalf("the business turn/start never reached the host: %v %s %s", err, &stdout, &stderr)
		}
	case err := <-exited:
		t.Fatalf("managed-start ended before its business turn/start: %v %s %s", err, &stdout, &stderr)
	}
	sentBefore := len(host.Requests())
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	var waited error
	select {
	case waited = <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("managed-start outlived its first interrupt while turn/start was held: %s %s", &stdout, &stderr)
	}
	var exit *exec.ExitError
	if !errors.As(waited, &exit) || exit.ExitCode() != contract.ExitHost || stdout.String() != "{\n  \"error\": \"host\",\n  \"detail\": \"context canceled\"\n}\n" {
		t.Fatalf("interrupted managed-start: %v stdout %q stderr %q", waited, &stdout, &stderr)
	}
	if after := host.Requests(); len(after) != sentBefore {
		t.Fatalf("sent after the interrupt: %+v", after[sentBefore:])
	}
	ledgers, err := filepath.Glob(filepath.Join(state, "operations-*.sqlite3"))
	if err != nil || len(ledgers) != 1 {
		t.Fatalf("ledgers %v %v", ledgers, err)
	}
	db, err := sql.Open("sqlite", ledgers[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT receipt FROM operations ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var statuses []any
	for rows.Next() {
		var receipt string
		if err := rows.Scan(&receipt); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(receipt), &decoded); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, decoded["status"])
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The child's creation, then the business send the interrupt claimed.
	if !reflect.DeepEqual(statuses, []any{"accepted", "outcome_unknown"}) {
		t.Fatalf("ledger receipts %v", statuses)
	}
}
