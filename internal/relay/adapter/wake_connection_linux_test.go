//go:build linux

package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
)

func TestWakeConnectionReplacementHelper(t *testing.T) {
	socket := os.Getenv("CRW1664_REPLACEMENT_SOCKET")
	if socket == "" {
		return
	}
	tmploctest.CheckSocket(t, filepath.Dir(socket), socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("ready")
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

func TestWakeEvidenceUsesTheSendingConnectionAfterSocketReplacement(t *testing.T) {
	w := newMgmtWorld(t)
	policy := filepath.Join(t.TempDir(), "roles.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"supervisor":{"expectation":"record"},"parent":{"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	w.open(mgmtA, ownerA)
	w.flowRun("P-A")
	// Establish the production adapter's actual WebSocket before the path moves.
	if _, err := w.a.ReadThread(w.ctx, ownerA); err != nil {
		t.Fatal(err)
	}
	original, err := delivery.ObserveWakeConfiguration(w.ctx, w.s, w.host.SocketPath, ownerA, supervisor.KindSessionStepWaiting, os.Getpid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(w.host.SocketPath, w.host.SocketPath+".old"); err != nil {
		t.Fatal(err)
	}
	helperCtx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	defer cancel()
	child := exec.CommandContext(helperCtx, os.Args[0], "-test.run=^TestWakeConnectionReplacementHelper$")
	child.Env = append(os.Environ(), "CRW1664_REPLACEMENT_SOCKET="+w.host.SocketPath)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("replacement listener: %q %v", ready, err)
	}
	replacement, err := delivery.ObserveWakeConfiguration(w.ctx, w.s, w.host.SocketPath, ownerA, supervisor.KindSessionStepWaiting, os.Getpid(), nil)
	if err != nil || replacement.Identity.AppServerInstance == original.Identity.AppServerInstance {
		t.Fatalf("replacement was not another process: %+v %v", replacement, err)
	}
	staged, err := w.c.StageDirect(w.ctx, supervisor.DirectMessage{Kind: supervisor.KindSessionStepWaiting, Relationship: "run:" + flowRun, Subject: supervisor.FlowSubject(flowRun, "connection-proof"), Summary: "ready", Text: "continue", At: delivery.ISOOf(w.clock.T)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := staged["messageId"].(string)
	if _, err := w.c.AutoSend(w.ctx, w.a, w.clock.T, 0, 1, "", "", ""); err != nil {
		t.Fatal(err)
	}
	attempt, err := w.s.LatestSupervisorAttempt(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := w.s.DB.QueryRowContext(w.ctx, `SELECT detail FROM journal WHERE kind=? AND subject=?`, delivery.WakeObserved, attempt.RequestID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var wake delivery.WakeAttempt
	if err := json.Unmarshal([]byte(raw), &wake); err != nil {
		t.Fatal(err)
	}
	if w.host.Count("turn/start") != 1 || wake.DeliveryState != delivery.Dispatched || wake.Identity.AppServerInstance != original.Identity.AppServerInstance {
		t.Fatalf("turn went to original host but journal names another connection: old=%s replacement=%s calls=%d record=%s", original.Identity.AppServerInstance, replacement.Identity.AppServerInstance, w.host.Count("turn/start"), raw)
	}
}

func TestWakeConnectionFollowsClientGeneration(t *testing.T) {
	host := fakehost.Start(t)
	client, err := appserver.Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a := New(Options{RPC: client})
	before := a.WakeConnection()
	if before.Conn == nil || before.ID == 0 || before.Socket != host.SocketPath {
		t.Fatalf("missing connection: %+v", before)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if closed := a.WakeConnection(); closed.Conn != nil || closed.ID != 0 {
		t.Fatalf("closed connection exposed: %+v", closed)
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := a.WakeConnection()
	if after.Conn == nil || after.ID <= before.ID || after.Conn == before.Conn {
		t.Fatalf("reconnect reused generation: before=%+v after=%+v", before, after)
	}
	if unknown := (&Adapter{}).WakeConnection(); unknown.Conn != nil || unknown.ID != 0 {
		t.Fatalf("unknown RPC invented identity: %+v", unknown)
	}
}
