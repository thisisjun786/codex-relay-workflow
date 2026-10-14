package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// bridgeProcess is `crw bridge` as Codex starts it for a child: outside the child's sandbox, in an environment of its own
// whose relay state is the one the operator configured, and spoken to over MCP stdio.
type bridgeProcess struct {
	session *sdk.ClientSession
	home    string
}

func startBridge(t *testing.T, relayState, appServerSocket string) *bridgeProcess {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(suiteBinary, "bridge", "--socket", appServerSocket, "--state-dir", filepath.Join(home, "ledger"))
	cmd.Dir = home
	var stderr bytes.Buffer // read after the session closed and the process exited
	cmd.Stderr = &stderr
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "CRW_READONLY_CHILD") || strings.HasPrefix(entry, store.StateEnv+"=") || strings.HasPrefix(entry, "XDG_STATE_HOME=")
	}), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg-state"), store.StateEnv+"="+relayState)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "bridge-report-test", Version: "0"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to crw bridge: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		if t.Failed() {
			t.Logf("crw bridge stderr:\n%s", stderr.String())
		}
	})
	return &bridgeProcess{session: session, home: home}
}

// report calls report_child and returns the result's text and whether it is an error result.
func (b *bridgeProcess) report(t *testing.T, command string, arguments ...string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := b.session.CallTool(ctx, &sdk.CallToolParams{Name: "report_child", Arguments: map[string]any{"command": command, "arguments": arguments}})
	if err != nil {
		t.Fatalf("report_child: %v", err)
	}
	text := ""
	for _, content := range result.Content {
		if raw, ok := content.(*sdk.TextContent); ok {
			text += raw.Text
		}
	}
	return text, result.IsError
}

// ownsNoStore is whether the bridge's own directories hold no relay store: the daemon records, the bridge never does.
func (b *bridgeProcess) ownsNoStore() bool {
	found := false
	_ = filepath.WalkDir(b.home, func(path string, _ os.DirEntry, _ error) error {
		found = found || strings.HasPrefix(filepath.Base(path), "relay.sqlite3")
		return nil
	})
	return !found
}

// CRW-1297: a relay child whose sandbox refuses the connect to control.sock (read-only, or workspace-write without
// network access) still reports, through the bridge's report_child tool: the bridge runs outside the child's sandbox and
// hands the command to the relay daemon, which records it in its own store. The child's own command ends with the
// explicit exit 3 before and after, so the records in the parent's store are the tool's.
func TestABridgeToolCarriesANetworklessChildsReceiptAndBlockedReportToTheDaemon(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	artifact := filepath.Join(filepath.Dir(state), "out.txt")
	if err := os.WriteFile(artifact, []byte("the delivered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_READONLY_CHILD_NO_NETWORK", "1")
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	directlyRefused := func(when string) {
		t.Helper()
		result, err := readonlyEmit(t, state, host)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(fmt.Sprint(result["detail"]), "operation not permitted") {
			t.Fatalf("%s: the child's own emit must be refused by its sandbox (exit 3), got %v %v", when, result, err)
		}
	}
	directlyRefused("before")
	bridge := startBridge(t, state, host.SocketPath)
	receipt := append(slices.Clone(blockedEmit[1:]), "--artifact", artifact)
	receipt[slices.Index(receipt, "blocked_needs_input")] = "ready_for_review"
	for _, arguments := range [][]string{receipt, blockedEmit[1:]} {
		if text, failed := bridge.report(t, "emit", arguments...); failed {
			t.Fatalf("%v was not reported: %s", arguments, text)
		}
	}
	for _, outcome := range []string{"ready_for_review", "blocked_needs_input"} {
		if n := countRows(t, database, "SELECT COUNT(*) FROM events WHERE producer='child' AND relationship_id='rel-1' AND outcome='"+outcome+"'"); n != 1 {
			t.Errorf("the parent's store holds %d %s events from the child, want 1", n, outcome)
		}
	}
	if !bridge.ownsNoStore() {
		t.Errorf("the bridge process opened a relay store of its own")
	}
	directlyRefused("after")
}

// CRW-1297: the daemon alone decides where the record goes. Nothing the call names (a --state of another initialized store,
// a socket, a kind-module, a path relative to the child) moves it: the bridge's own configuration finds the daemon, the
// arguments are read as the command's own options, and the daemon's checks (CRW-1299) answer the rest, recording nothing.
func TestABridgeReportCannotRedirectTheDaemonsRecord(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	otherRoot := t.TempDir()
	other := seedIntake(t, filepath.Join(otherRoot, "state", "relay.sqlite3"), otherRoot)
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	bridge := startBridge(t, state, host.SocketPath)
	events := func() int64 {
		return countRows(t, database, "SELECT COUNT(*) FROM events") + countRows(t, other, "SELECT COUNT(*) FROM events")
	}
	for name, refused := range map[string]struct {
		mention   string
		arguments []string
	}{
		"state-flag":                  {"--state", append([]string{"--state", filepath.Dir(other.Path)}, blockedEmit[1:]...)},
		"state-flag-last":             {"--state", append(slices.Clone(blockedEmit[1:]), "--state", filepath.Dir(other.Path))},
		"socket-flag":                 {"--socket", append([]string{"--socket", host.SocketPath}, blockedEmit[1:]...)},
		"kind-module-that-installs":   {"--kind-module", append([]string{"--kind-module", "codex_session_relay.projects"}, blockedEmit[1:]...)},
		"relative-independent-review": {"--independent-review", append(slices.Clone(blockedEmit[1:]), "--independent-review", "review.json")},
	} {
		text, failed := bridge.report(t, "emit", refused.arguments...)
		if !failed || !strings.Contains(text, refused.mention) || events() != 0 {
			t.Errorf("%s: expected an error naming %q and nothing recorded, got failed=%v events=%d %s", name, refused.mention, failed, events(), text)
		}
	}
	if text, failed := bridge.report(t, "emit", blockedEmit[1:]...); failed || events() != 1 || countRows(t, database, "SELECT COUNT(*) FROM events") != 1 {
		t.Fatalf("the plain report is recorded in the daemon's store only: failed=%v %s", failed, text)
	}
	// a bridge configured with another state finds no daemon there; the record is never moved to the daemon it cannot find
	elsewhere := startBridge(t, filepath.Join(t.TempDir(), "no-daemon"), host.SocketPath)
	if text, failed := elsewhere.report(t, "emit", blockedEmit[1:]...); !failed || !strings.Contains(text, "relay daemon unavailable") || events() != 1 {
		t.Fatalf("a bridge whose own state has no daemon must fail explicitly: failed=%v %s", failed, text)
	}
}

// CRW-1297: with no daemon the report ends with an explicit error; the bridge opens no store and the state holds nothing new.
func TestABridgeReportWithoutADaemonIsAnExplicitErrorWithNoLocalStore(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge := startBridge(t, state, host.SocketPath)
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	text, failed := bridge.report(t, "emit", blockedEmit[1:]...)
	if !failed || !strings.Contains(text, "relay daemon unavailable") || !strings.Contains(text, "child report was not submitted") {
		t.Fatalf("expected the explicit daemon-absent error, got failed=%v %s", failed, text)
	}
	if countRows(t, database, "SELECT COUNT(*) FROM events") != 0 || !bridge.ownsNoStore() {
		t.Fatal("a report with no daemon was recorded or opened a local store")
	}
}
