package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

var scanNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func (h *host) scan(t *testing.T) record.Object {
	t.Helper()
	return doctor.RetentionScan(context.Background(), doctor.RetentionOptions{Env: h.env, Now: func() time.Time { return scanNow }})
}

// references is each reported reference as "row:field:value".
func references(report record.Object) []string {
	var out []string
	for _, raw := range golden.List(record.Get(report, "pythonReferences")) {
		r := golden.Obj(raw)
		out = append(out, scope.PyStr(record.Get(r, "row"))+":"+record.Text(r, "field")+":"+record.Text(r, "value"))
	}
	return out
}

func claim(t *testing.T, h *host, key string, outcome bool) string {
	t.Helper()
	journal := filepath.Join(h.codex, "crw-completion-hook", "journal")
	path := filepath.Join(h.codex, "crw-completion-hook", "stop-events", key+".json")
	write(t, path, `{"eventKey": "`+key+`", "claimedAt": "2026-09-29T11:59:00Z", "claimedBy": {"pid": 4242, "journalRoot": "`+journal+`", "attemptRow": "20260929/x.json"}, "ledgerVersion": 1}`, 0o600)
	if outcome {
		write(t, filepath.Join(journal, "accepted", key+".outcome.json"), `{"eventKey": "`+key+`"}`, 0o600)
	}
	return path
}

// The fixture the brief names: one Stop-event claim without an outcome and one Python
// relayExecutable, on a host whose pointer selects a Go runtime and whose other records are
// clean. Exactly those two are reported (the claim as a live hold, the executable as a Python
// reference); everything else - a claim with its outcome, an old journal row, settings that
// resolve through the pointer to the Go binary - is not, and the one row this command cannot
// read (resumable Codex threads) keeps the scan from being clear.
func TestRetentionScanReportsExactlyTheSeededReferences(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	dir, _ := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	h.settings(t, map[string]string{
		"relayExecutable":    filepath.Join(env, "bin", "codex-session-relay"),
		"adapterEntryPoint":  filepath.Join(h.current(), "bin", "crw-completion-hook"),
		"adapterInterpreter": filepath.Join(h.current(), "bin", "crw-completion-hook"),
		"journalRoot":        filepath.Join(h.codex, "crw-completion-hook", "journal"),
	}, map[string]string{"bridgeExecutable": filepath.Join(h.current(), "bin", "codex-thread-bridge")})
	held := claim(t, h, strings.Repeat("a", 64), false)
	claim(t, h, strings.Repeat("b", 64), true)
	write(t, filepath.Join(h.codex, "crw-completion-hook", "journal", "20260929", "old.json"), `{"at": "2026-09-29T11:00:00Z", "configuration": "x"}`, 0o600)
	before := snapshot(t, h.home)
	report := h.scan(t)
	if after := snapshot(t, h.home); after != before {
		t.Fatal("the scan changed the host")
	}
	if got := references(report); len(got) != 1 || got[0] != "4:relayExecutable:"+filepath.Join(env, "bin", "codex-session-relay") {
		t.Fatalf("python references %v", got)
	}
	holds := golden.List(record.Get(report, "liveHolds"))
	if len(holds) != 1 || record.Get(golden.Obj(holds[0]), "source") != held || record.Get(golden.Obj(holds[0]), "row") != int64(1) {
		t.Fatalf("live holds %s", golden.Canon(holds))
	}
	unscanned := golden.List(record.Get(report, "unscanned"))
	if len(unscanned) != 1 || !strings.HasPrefix(unscanned[0].(string), "row 7: ") || record.Get(report, "clear") != false {
		t.Fatalf("unscanned %s, clear %v", golden.Canon(unscanned), record.Get(report, "clear"))
	}
	if len(golden.List(record.Get(report, "surfaces"))) != len(doctor.Surfaces) || len(golden.List(record.Get(report, "unreadable"))) != 0 {
		t.Fatalf("surfaces %s", golden.Canon(record.Get(report, "surfaces")))
	}
}

// The relay host's own shape: `current` points at a venv, and the settings name
// current/bin/codex-session-relay and current/bin/python3. The first contains no "python" at
// all; resolving it through the pointer finds the Python console script it reaches, and the
// pointer target itself is reported as the venv it is.
func TestRetentionScanResolvesThroughThePointer(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	link(t, env, h.current())
	relay := filepath.Join(h.current(), "bin", "codex-session-relay")
	h.settings(t, map[string]string{
		"relayExecutable":    relay,
		"adapterEntryPoint":  filepath.Join(h.current(), "bin", "crw-completion-hook"),
		"adapterInterpreter": filepath.Join(h.current(), "bin", "python3"),
	}, nil)
	if strings.Contains(strings.ToLower(relay), "python") {
		t.Fatal("the fixture's relayExecutable must name no Python in its text")
	}
	report := h.scan(t)
	want := []string{
		"4:relayExecutable:" + relay,
		"4:adapterEntryPoint:" + filepath.Join(h.current(), "bin", "crw-completion-hook"),
		"4:adapterInterpreter:" + filepath.Join(h.current(), "bin", "python3"),
		"11:target:" + h.current(),
	}
	if got := references(report); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("python references\n got %v\nwant %v", got, want)
	}
	for _, raw := range golden.List(record.Get(report, "pythonReferences")) {
		if r := golden.Obj(raw); record.Get(r, "throughPointer") != true {
			t.Errorf("not resolved through the pointer: %s", golden.Canon(r))
		}
	}
}

// The wiring surfaces: the cached plugin's hook and MCP commands (the repository's own Python
// wiring), the launcher copy, a user hooks.json entry and a config.toml server naming Python
// are each reported with where they were found; a shell launcher that execs the Go binary
// through the pointer and a server that runs a native binary are not.
func TestRetentionScanReadsTheWiringSurfaces(t *testing.T) {
	h := newHost(t)
	dir, _ := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	cache := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.4.0+test")
	for _, name := range []string{"wiring/mcp.json", "wiring/hooks/stop-recording-completion.json"} {
		raw, err := os.ReadFile(filepath.Join(golden.Root(), "plugins", "crw", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(cache, filepath.FromSlash(name)), string(raw), 0o644)
	}
	write(t, filepath.Join(h.codex, "crw-stop-hook.py"), "#!/usr/bin/env python3\n", 0o600)
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "bash '/x/state.sh' session", "timeout": 10}]}],
 "Stop": [{"hooks": [{"type": "command", "command": "/usr/bin/python3 /checkout/scripts/completion_hook.py", "timeout": 30}]}]}}`, 0o600)
	write(t, filepath.Join(h.codex, "config.toml"), "model = \"x\"\n\n[mcp_servers.codex-thread-bridge]\ncommand = \""+filepath.Join(h.current(), "bin", "codex-thread-bridge")+"\"\n\n[mcp_servers.legacy]\ncommand = \"python3\"\nargs = [\"-m\", \"legacy_server\", \"/opt/legacy/server.py\"]\n", 0o600)
	report := h.scan(t)
	want := []string{
		"5:hooks.Stop[0].hooks[0].command:python3",
		"5:hooks.Stop[0].hooks[0].command:${PLUGIN_ROOT}/wiring/crw_stop_hook.py",
		"5:mcpServers.codex-thread-bridge.command:python3",
		"5:mcpServers.codex-thread-bridge.args[0]:./wiring/crw_bridge_mcp.py",
		"8:file:" + filepath.Join(h.codex, "crw-stop-hook.py"),
		"9:hooks.Stop[0].hooks[0].command:/usr/bin/python3",
		"9:hooks.Stop[0].hooks[0].command:/checkout/scripts/completion_hook.py",
		"10:mcp_servers.legacy.command:python3",
		"10:mcp_servers.legacy.args[2]:/opt/legacy/server.py",
	}
	got := references(report)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("python references\n got %v\nwant %v", got, want)
	}
	if record.Get(report, "longestHookTimeoutSeconds") != float64(30) {
		t.Fatalf("the longest Stop hook timeout is the user's 30 s: %v", record.Get(report, "longestHookTimeoutSeconds"))
	}
}

// A journal row inside twice the longest hook timeout is a live hold; one outside it is not.
func TestRetentionScanHoldsForARecentJournalRow(t *testing.T) {
	h := newHost(t)
	journal := filepath.Join(h.codex, "crw-completion-hook", "journal", "20260929")
	write(t, filepath.Join(journal, "recent.json"), `{"at": "2026-09-29T11:59:50Z", "configuration": "/c/crw-completion-hook.json"}`, 0o600)
	write(t, filepath.Join(journal, "old.json"), `{"at": "2026-09-29T11:59:00Z"}`, 0o600)
	holds := golden.List(record.Get(h.scan(t), "liveHolds"))
	if len(holds) != 1 || record.Get(golden.Obj(holds[0]), "source") != filepath.Join(journal, "recent.json") || record.Get(golden.Obj(holds[0]), "row") != int64(2) {
		t.Fatalf("holds %s", golden.Canon(holds))
	}
}

// Rows 3 and 6: an alive daemon.json pid whose process runs Python is a reference, a pid whose
// start time no longer matches is not alive, and the /proc/locks holder of a managed-start
// lock is judged the same way, less the pids row 3 already reported.
func TestRetentionScanJudgesLiveProcesses(t *testing.T) {
	h := newHost(t)
	stateDir := filepath.Join(h.state, "codex-session-relay", "e4f02742e7163a2a")
	mkdir(t, stateDir)
	pythonPid, _ := golden.Spawn(t, filepath.Join(t.TempDir(), "python3"), "")
	workerPid, _ := golden.Spawn(t, "", "")
	lockPath := filepath.Join(stateDir, "managed-start-"+strings.Repeat("c", 64)+".lock")
	holderPid, _ := golden.Spawn(t, "python3.13", lockPath)
	ticks := func(pid int) string { return scope.PyStr(service.StartTicks(pid)) }
	boot := scope.PyStr(service.BootID())
	write(t, filepath.Join(stateDir, "daemon.json"), `{"pid": `+strconv.Itoa(pythonPid)+`, "startTicks": `+ticks(pythonPid)+`, "bootId": "`+boot+`", "workerPid": `+strconv.Itoa(workerPid)+`, "workerStartTicks": `+ticks(workerPid)+`}`, 0o600)
	stale := filepath.Join(h.state, "codex-session-relay", "stale")
	write(t, filepath.Join(stale, "daemon.json"), `{"pid": `+strconv.Itoa(holderPid)+`, "startTicks": 1, "bootId": "`+boot+`"}`, 0o600)
	report := h.scan(t)
	var rows []string
	for _, raw := range golden.List(record.Get(report, "pythonReferences")) {
		r := golden.Obj(raw)
		rows = append(rows, scope.PyStr(record.Get(r, "row"))+":"+scope.PyStr(record.Get(r, "pid")))
	}
	want := []string{"3:" + strconv.Itoa(pythonPid), "6:" + strconv.Itoa(holderPid)}
	if strings.Join(rows, ",") != strings.Join(want, ",") {
		t.Fatalf("process references %v, want %v\n%s", rows, want, golden.Canon(record.Get(report, "pythonReferences")))
	}
}

func TestShellWordsSplitsLikeSh(t *testing.T) {
	for line, want := range map[string]string{
		`python3 -c "a b" "${PLUGIN_ROOT}/x.py"`:                        `python3|-c|a b|${PLUGIN_ROOT}/x.py`,
		`bash '/x/state sh' session`:                                    `bash|/x/state sh|session`,
		`"$HOME/.local/share/crw-runtime/current/bin/crw" hook; exit 0`: `$HOME/.local/share/crw-runtime/current/bin/crw|hook|exit|0`,
		`a\ b "c\"d" 'e'f`:                                              `a b|c"d|ef`,
	} {
		if got := strings.Join(doctor.ShellWords(line), "|"); got != want {
			t.Errorf("%s: %s", line, got)
		}
	}
}
