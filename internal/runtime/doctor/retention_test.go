package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
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

// unreadable is the report's unreadable list.
func unreadable(report record.Object) []string {
	var out []string
	for _, raw := range golden.List(record.Get(report, "unreadable")) {
		out = append(out, raw.(string))
	}
	return out
}

// listed reports whether one unreadable entry names every part.
func listed(entries []string, parts ...string) bool {
	for _, entry := range entries {
		all := true
		for _, part := range parts {
			all = all && strings.Contains(entry, part)
		}
		if all {
			return true
		}
	}
	return false
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads through any mode, so EACCES cannot be produced")
	}
}

// A reference the scan resolves but cannot read is never dropped: a relayExecutable that is a
// Python script the scan may execute but not read (mode 0111), and a bridgeExecutable behind a
// directory it may not search (mode 000), are each listed as unreadable with the row, file and
// field that named them, so the scan is not clear.
func TestRetentionScanListsReferencesItCannotRead(t *testing.T) {
	skipAsRoot(t)
	h := newHost(t)
	env := h.pythonVenv(t)
	secret := filepath.Join(h.home, "exec-only", "relay")
	write(t, secret, "#!"+filepath.Join(env, "bin", "python3")+"\n", 0o111)
	locked := filepath.Join(h.home, "locked")
	write(t, filepath.Join(locked, "bin", "bridge"), "#!/bin/sh\n", 0o755)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	h.settings(t, map[string]string{"relayExecutable": secret}, map[string]string{"bridgeExecutable": filepath.Join(locked, "bin", "bridge")})
	report := h.scan(t)
	got := unreadable(report)
	for _, want := range [][]string{
		{"row 4", filepath.Join(h.codex, "crw-completion-hook.json"), "relayExecutable", secret},
		{"row 4", filepath.Join(h.codex, "crw-bridge-mcp.json"), "bridgeExecutable", filepath.Join(locked, "bin", "bridge")},
	} {
		if !listed(got, want...) {
			t.Errorf("no unreadable entry names %v: %v", want, got)
		}
	}
	if record.Get(report, "clear") != false {
		t.Fatal("a scan with unreadable references is clear")
	}
}

// A hook command names its executable through $HOME, ${HOME} or ~, which the shell running it
// expands against HOME (not CODEX_HOME): the scan expands them the same way and finds the venv
// console script each one reaches. A variable the scan cannot expand is listed as unreadable,
// never skipped.
func TestRetentionScanExpandsWhatAHookCommandNames(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	link(t, env, filepath.Join(h.home, "venv"))
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [
 {"type": "command", "command": "\"$HOME/venv/bin/codex-session-relay\" hook", "timeout": 10},
 {"type": "command", "command": "${HOME}/venv/bin/codex-thread-bridge serve", "timeout": 10},
 {"type": "command", "command": "~/venv/bin/crw-completion-hook", "timeout": 10},
 {"type": "command", "command": "\"$CODEX_HOME/relay\" hook", "timeout": 10},
 {"type": "command", "command": "\"$RELAY_HOME/bin/relay\" hook", "timeout": 10}]}]}}`, 0o600)
	link(t, filepath.Join(env, "bin", "codex-session-relay"), filepath.Join(h.codex, "relay"))
	report := h.scan(t)
	want := []string{
		"9:hooks.Stop[0].hooks[0].command:$HOME/venv/bin/codex-session-relay",
		"9:hooks.Stop[0].hooks[1].command:${HOME}/venv/bin/codex-thread-bridge",
		"9:hooks.Stop[0].hooks[2].command:~/venv/bin/crw-completion-hook",
		"9:hooks.Stop[0].hooks[3].command:$CODEX_HOME/relay",
	}
	if got := references(report); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("python references\n got %v\nwant %v", got, want)
	}
	if got := unreadable(report); len(got) != 1 || !listed(got, "row 9", "hooks.Stop[0].hooks[4].command", "$RELAY_HOME/bin/relay", "RELAY_HOME") {
		t.Fatalf("unreadable %v", got)
	}
}

// fakeProc is a procfs root (the scan's Proc seam) holding one process whose start time is
// ticks; the caller lays out its exe and cmdline.
func fakeProc(t *testing.T, pid int, ticks int64) string {
	t.Helper()
	proc := t.TempDir()
	mkdir(t, filepath.Join(proc, "self"))
	write(t, filepath.Join(proc, strconv.Itoa(pid), "stat"), strconv.Itoa(pid)+" (relay worker) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 "+strconv.FormatInt(ticks, 10)+" 0 0\n", 0o644)
	return proc
}

func (h *host) scanProc(t *testing.T, proc string) record.Object {
	t.Helper()
	return doctor.RetentionScan(context.Background(), doctor.RetentionOptions{Env: h.env, Now: func() time.Time { return scanNow }, Proc: proc})
}

func (h *host) daemon(t *testing.T, pid int, ticks int64) string {
	t.Helper()
	path := filepath.Join(h.state, "codex-session-relay", "scope-1", "daemon.json")
	write(t, path, `{"pid": `+strconv.Itoa(pid)+`, "startTicks": `+strconv.FormatInt(ticks, 10)+`}`, 0o600)
	return path
}

// An alive recorded pid whose executable or command line cannot be read is listed as
// unreadable: what it runs is unknown, so it may be Python. A pid whose process is gone, or
// is a zombie, is not alive and is not listed.
func TestRetentionScanListsAnAliveProcessItCannotRead(t *testing.T) {
	for name, layout := range map[string]func(t *testing.T, dir string){
		"exe": func(t *testing.T, dir string) { write(t, filepath.Join(dir, "exe"), "not a link", 0o644) },
		"cmdline": func(t *testing.T, dir string) {
			link(t, "/usr/bin/true", filepath.Join(dir, "exe"))
			mkdir(t, filepath.Join(dir, "cmdline"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			proc := fakeProc(t, 4242, 777)
			layout(t, filepath.Join(proc, "4242"))
			path := h.daemon(t, 4242, 777)
			report := h.scanProc(t, proc)
			if got := unreadable(report); !listed(got, "row 3", path, "pid", "4242") {
				t.Fatalf("unreadable %v", got)
			}
			if len(references(report)) != 0 || record.Get(report, "clear") != false {
				t.Fatalf("references %v clear %v", references(report), record.Get(report, "clear"))
			}
		})
	}
	h := newHost(t)
	proc := fakeProc(t, 4242, 777)
	h.daemon(t, 4243, 777) // gone: no /proc/4243 while /proc/self exists
	zombie := filepath.Join(proc, "4242", "stat")
	write(t, zombie, "4242 (relay) Z 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777 0 0\n", 0o644)
	write(t, filepath.Join(h.state, "codex-session-relay", "scope-2", "daemon.json"), `{"pid": 4242, "startTicks": 777}`, 0o600)
	if got := unreadable(h.scanProc(t, proc)); len(got) != 0 {
		t.Fatalf("a gone or zombie pid is unreadable: %v", got)
	}
}

// Where there is no procfs (darwin), whether a recorded pid is alive cannot be read, so row 3
// is unscanned rather than scanned with nothing found.
func TestRetentionScanWithoutProcfsLeavesDaemonsUnscanned(t *testing.T) {
	h := newHost(t)
	h.daemon(t, os.Getpid(), 777)
	report := h.scanProc(t, t.TempDir())
	for _, raw := range golden.List(record.Get(report, "surfaces")) {
		if one := golden.Obj(raw); record.Get(one, "row") == int64(3) && record.Get(one, "scanned") != false {
			t.Fatalf("row 3 scanned without a process table: %s", golden.Canon(one))
		}
	}
	unscanned := golden.List(record.Get(report, "unscanned"))
	if len(unscanned) != 2 || !strings.HasPrefix(unscanned[0].(string), "row 3: ") {
		t.Fatalf("unscanned %s", golden.Canon(unscanned))
	}
}

// Row 2 reads every day directory the window reaches back to, not only its first and last:
// with a 100000 s timeout (a 200000 s window) a row written 24 hours ago, in the day between,
// is a hold.
func TestRetentionScanReadsEveryDayTheJournalWindowReaches(t *testing.T) {
	h := newHost(t)
	h.settings(t, map[string]string{"journalRoot": filepath.Join(h.codex, "crw-completion-hook", "journal")}, nil)
	raw, _ := os.ReadFile(filepath.Join(h.codex, "crw-completion-hook.json"))
	write(t, filepath.Join(h.codex, "crw-completion-hook.json"), strings.Replace(string(raw), "{", `{"timeoutSeconds": 100000,`, 1), 0o600)
	journal := filepath.Join(h.codex, "crw-completion-hook", "journal")
	write(t, filepath.Join(journal, "20260928", "yesterday.json"), `{"at": "2026-09-28T12:00:00Z"}`, 0o600)
	write(t, filepath.Join(journal, "20260920", "old.json"), `{"at": "2026-09-20T12:00:00Z"}`, 0o600)
	report := h.scan(t)
	holds := golden.List(record.Get(report, "liveHolds"))
	if len(holds) != 1 || record.Get(golden.Obj(holds[0]), "source") != filepath.Join(journal, "20260928", "yesterday.json") {
		t.Fatalf("holds %s", golden.Canon(holds))
	}
}

// A timeout too large for a duration holds every row, however old; a NaN timeout gives no
// window at all, so row 2 is unscanned. Neither reads as a clear journal.
func TestRetentionScanHoldsEveryRowForAnUnboundedWindow(t *testing.T) {
	for _, timeout := range []string{"9.3e9", "1e300", "Infinity"} {
		t.Run(timeout, func(t *testing.T) {
			h := newHost(t)
			journal := filepath.Join(h.codex, "crw-completion-hook", "journal")
			write(t, filepath.Join(journal, "20260929", "now.json"), `{"at": "2026-09-29T12:00:00Z"}`, 0o600)
			write(t, filepath.Join(journal, "20190101", "old.json"), `{"at": "2019-01-01T00:00:00Z"}`, 0o600)
			write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "true", "timeout": `+timeout+`}]}]}}`, 0o600)
			if holds := golden.List(record.Get(h.scan(t), "liveHolds")); len(holds) != 2 {
				t.Fatalf("holds %s", golden.Canon(holds))
			}
		})
	}
	h := newHost(t)
	write(t, filepath.Join(h.codex, "crw-completion-hook", "journal", "20260929", "now.json"), `{"at": "2026-09-29T12:00:00Z"}`, 0o600)
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "true", "timeout": NaN}]}]}}`, 0o600)
	report := h.scan(t)
	unscanned := golden.List(record.Get(report, "unscanned"))
	if len(unscanned) != 2 || !strings.HasPrefix(unscanned[0].(string), "row 2: ") || record.Get(report, "clear") != false {
		t.Fatalf("unscanned %s", golden.Canon(unscanned))
	}
}

// ${PLUGIN_ROOT} is the cached version's own directory in a cached declaration (row 5), so a
// word through it that names a console script (no .py to go by) is resolved and reported. The
// same word in the user's hooks.json (row 9) has no plugin root to expand, so it is unreadable.
func TestRetentionScanExpandsThePluginRootInTheCacheOnly(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	cache := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.4.0+test")
	link(t, filepath.Join(env, "bin", "codex-session-relay"), filepath.Join(cache, "wiring", "relay"))
	command := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "\"${PLUGIN_ROOT}/wiring/relay\" stop", "timeout": 10}]}]}}`
	write(t, filepath.Join(cache, "wiring", "hooks", "stop.json"), command, 0o644)
	write(t, filepath.Join(h.codex, "hooks.json"), command, 0o600)
	report := h.scan(t)
	refs := golden.List(record.Get(report, "pythonReferences"))
	if got := references(report); len(got) != 1 || got[0] != "5:hooks.Stop[0].hooks[0].command:${PLUGIN_ROOT}/wiring/relay" || record.Get(golden.Obj(refs[0]), "resolves") != filepath.Join(env, "bin", "codex-session-relay") {
		t.Fatalf("python references %s", golden.Canon(refs))
	}
	if got := unreadable(report); len(got) != 1 || !listed(got, "row 9", filepath.Join(h.codex, "hooks.json"), "${PLUGIN_ROOT}") {
		t.Fatalf("unreadable %v", got)
	}
}

// surfaceRow is one row's entry in the report's surfaces.
func surfaceRow(t *testing.T, report record.Object, row int64) record.Object {
	t.Helper()
	for _, raw := range golden.List(record.Get(report, "surfaces")) {
		if one := golden.Obj(raw); record.Get(one, "row") == row {
			return one
		}
	}
	t.Fatalf("no surface row %d", row)
	return nil
}

// Row 6 reads the kernel's lock table to its end or says it did not: a lock table whose read
// fails part way (a line longer than the reader takes, or a table that cannot be read at all
// once open) leaves row 6 unscanned and the table listed as unreadable, never scanned with no
// holder found.
func TestRetentionScanLeavesLockHoldersUnscannedWhenTheLockTableFailsPartWay(t *testing.T) {
	for name, layout := range map[string]func(t *testing.T, locks string){
		"overlong line": func(t *testing.T, locks string) {
			write(t, locks, "1: FLOCK  ADVISORY  WRITE 1 00:00:1 0 EOF\n2: "+strings.Repeat("x", 70000)+"\n3: FLOCK  ADVISORY  WRITE 2 00:00:2 0 EOF\n", 0o644)
		},
		"unreadable once open": func(t *testing.T, locks string) { mkdir(t, locks) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			write(t, filepath.Join(h.state, "codex-session-relay", "scope-1", "managed-start-"+strings.Repeat("c", 64)+".lock"), "", 0o600)
			proc := t.TempDir()
			mkdir(t, filepath.Join(proc, "self"))
			locks := filepath.Join(proc, "locks")
			layout(t, locks)
			report := h.scanProc(t, proc)
			if row := surfaceRow(t, report, 6); record.Get(row, "scanned") != false {
				t.Errorf("row 6 scanned from a lock table read part way: %s", golden.Canon(row))
			}
			if got := unreadable(report); !listed(got, locks) {
				t.Errorf("the lock table is not listed as unreadable: %v", got)
			}
			if record.Get(report, "clear") != false {
				t.Error("a scan whose lock table was read part way is clear")
			}
		})
	}
}

// An MCP server's command is one executable path, not a command line: Codex execs it with args
// as a separate list. A path holding a space, here a link to a venv console script, is resolved
// whole and reported, in config.toml (row 10) and in a cached plugin's MCP declaration (row 5).
func TestRetentionScanResolvesAnMCPCommandAsOnePath(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	spaced := filepath.Join(h.home, "my tools", "relay")
	link(t, filepath.Join(env, "bin", "codex-session-relay"), spaced)
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.spaced]\ncommand = \""+spaced+"\"\nargs = [\"serve\"]\n", 0o600)
	cache := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.4.0+test")
	write(t, filepath.Join(cache, "wiring", "mcp.json"), `{"mcpServers": {"spaced": {"command": "`+spaced+`", "args": ["serve"]}}}`, 0o644)
	report := h.scan(t)
	want := []string{
		"5:mcpServers.spaced.command:" + spaced,
		"10:mcp_servers.spaced.command:" + spaced,
	}
	if got := references(report); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("python references\n got %v\nwant %v", got, want)
	}
	for _, raw := range golden.List(record.Get(report, "pythonReferences")) {
		if r := golden.Obj(raw); record.Get(r, "resolves") != filepath.Join(env, "bin", "codex-session-relay") {
			t.Errorf("not resolved to the console script: %s", golden.Canon(r))
		}
	}
	if got := unreadable(report); len(got) != 0 {
		t.Fatalf("unreadable %v", got)
	}
}

// A bare command word that no PATH directory holds as an executable file runs something the
// scan cannot see (another PATH, a shell function): it is listed as unreadable with its row,
// source and field, for a hook command (row 9) and an MCP server's command (row 10), and the
// scan is not clear.
func TestRetentionScanListsACommandNotFoundOnPath(t *testing.T) {
	h := newHost(t)
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=" + filepath.Join(h.home, "bin")}
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "crw-relay-not-installed stop", "timeout": 10}]}]}}`, 0o600)
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.gone]\ncommand = \"crw-bridge-not-installed\"\n", 0o600)
	report := h.scan(t)
	got := unreadable(report)
	for _, want := range [][]string{
		{"row 9", filepath.Join(h.codex, "hooks.json"), "hooks.Stop[0].hooks[0].command", `"crw-relay-not-installed"`, filepath.Join(h.home, "bin")},
		{"row 10", filepath.Join(h.codex, "config.toml"), "mcp_servers.gone.command", `"crw-bridge-not-installed"`, filepath.Join(h.home, "bin")},
	} {
		if !listed(got, want...) {
			t.Errorf("no unreadable entry names %v: %v", want, got)
		}
	}
	if len(got) != 2 || record.Get(report, "clear") != false {
		t.Fatalf("unreadable %v, clear %v", got, record.Get(report, "clear"))
	}
}

// A hook command is read as the shell reads it: every command position is judged, not only the
// line's first word. A bare command there that no PATH directory holds is unreadable after
// cd ... &&, exec, env, ;, ||, |, inside sh -c and inside a function body, and a builtin (cd,
// true), a prefix (exec, env) and a function the command defines are not themselves listed. A
// command word PATH resolves to a venv console script after cd ... && is a reference.
func TestRetentionScanJudgesEveryCommandPositionOfAHookCommand(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	bin := filepath.Join(h.home, "bin")
	link(t, filepath.Join(env, "bin", "codex-session-relay"), filepath.Join(bin, "relay"))
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=" + bin + ":/usr/bin:/bin"}
	commands := []string{
		`cd /tmp && gone-a stop`,
		`exec gone-b`,
		`env A=1 gone-c`,
		`true; gone-d || gone-e | gone-f`,
		`sh -c 'gone-g'`,
		`cd /tmp && relay hook`,
		`f() { gone-h; }; f`,
	}
	var hooks []string
	for _, command := range commands {
		hooks = append(hooks, `{"type": "command", "command": `+strconv.Quote(command)+`, "timeout": 10}`)
	}
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [`+strings.Join(hooks, ", ")+`]}]}}`, 0o600)
	report := h.scan(t)
	got := unreadable(report)
	for i, word := range []string{"gone-a", "gone-b", "gone-c", "gone-d", "gone-e", "gone-f", "gone-g", "gone-h"} {
		hook := map[int]int{0: 0, 1: 1, 2: 2, 3: 3, 4: 3, 5: 3, 6: 4, 7: 6}[i]
		if !listed(got, "row 9", "hooks.Stop[0].hooks["+strconv.Itoa(hook)+"].command", strconv.Quote(word)) {
			t.Errorf("no unreadable entry names %s in hook %d: %v", word, hook, got)
		}
	}
	for _, word := range []string{"cd", "true", "exec", "env", "sh", "f"} {
		if listed(got, " "+strconv.Quote(word)+": ") {
			t.Errorf("%s is listed as unreadable: %v", word, got)
		}
	}
	if len(got) != 8 {
		t.Errorf("unreadable %v", got)
	}
	if refs := references(report); strings.Join(refs, "|") != "9:hooks.Stop[0].hooks[5].command:relay" {
		t.Errorf("python references %v", refs)
	}
}

// A crw-*.json value that is not an absolute path is not resolved against the scan's own
// working directory: the Stop and bridge launchers accept only absolute paths, and a relative
// one would resolve wherever they run. It is a Python reference when it names Python
// (adapterInterpreter python3) and unreadable otherwise, even with a console script of that name
// in the scan's directory; plain args are arguments. A PATH directory that is relative is not
// searched either, so a hook command only such a directory holds is unreadable.
func TestRetentionScanDoesNotResolveRelativeValuesAgainstItsOwnDirectory(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	cwd := t.TempDir()
	link(t, filepath.Join(env, "bin", "codex-session-relay"), filepath.Join(cwd, "codex-session-relay"))
	link(t, filepath.Join(env, "bin", "codex-thread-bridge"), filepath.Join(cwd, "bin", "codex-thread-bridge"))
	link(t, filepath.Join(env, "bin", "crw-completion-hook"), filepath.Join(cwd, "cwd-relay"))
	t.Chdir(cwd)
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=.:/usr/bin:/bin"}
	write(t, filepath.Join(h.codex, "crw-completion-hook.json"), `{"relayExecutable": "codex-session-relay", "adapterInterpreter": "python3"}`, 0o600)
	write(t, filepath.Join(h.codex, "crw-bridge-mcp.json"), `{"bridgeExecutable": "bin/codex-thread-bridge", "args": ["serve", "--stdio"]}`, 0o600)
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "cwd-relay stop", "timeout": 10}]}]}}`, 0o600)
	report := h.scan(t)
	if refs := references(report); strings.Join(refs, "|") != "4:adapterInterpreter:python3" {
		t.Errorf("python references %v", refs)
	}
	got := unreadable(report)
	for _, want := range [][]string{
		{"row 4", filepath.Join(h.codex, "crw-completion-hook.json"), "relayExecutable", `"codex-session-relay"`, "not an absolute path"},
		{"row 4", filepath.Join(h.codex, "crw-bridge-mcp.json"), "bridgeExecutable", `"bin/codex-thread-bridge"`, "not an absolute path"},
		{"row 9", filepath.Join(h.codex, "hooks.json"), `"cwd-relay"`},
	} {
		if !listed(got, want...) {
			t.Errorf("no unreadable entry names %v: %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("unreadable %v", got)
	}
}

// A journal row is judged by the at its hook wrote, never by the file's modification time: a
// row that is not JSON, not an object, or has no valid at is listed as unreadable with its path,
// however old the file looks, and the scan is not clear. A valid old row is still not a hold.
func TestRetentionScanListsAJournalRowWhoseTimeCannotBeEstablished(t *testing.T) {
	h := newHost(t)
	journal := filepath.Join(h.codex, "crw-completion-hook", "journal", "20260929")
	rows := map[string]string{
		"torn.json":    `{"at": "2026-09-29T11:59:5`,
		"list.json":    `[1, 2]`,
		"no-at.json":   `{"configuration": "/c/crw-completion-hook.json"}`,
		"bad-at.json":  `{"at": "yesterday"}`,
		"valid.json":   `{"at": "2026-09-29T11:00:00Z"}`,
		"number.json":  `{"at": 1790000000}`,
		"offset.json":  `{"at": "2026-09-29T11:59:50+00:00"}`,
		"spaces.json":  `{"at": " 2026-09-29T11:59:50Z"}`,
		"control.json": `{"at": "2026-09-29T11:59:50Z", "configuration": "/c/x"}`,
	}
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for name, content := range rows {
		path := filepath.Join(journal, name)
		write(t, path, content, 0o600)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	report := h.scan(t)
	got := unreadable(report)
	for _, name := range []string{"torn.json", "list.json", "no-at.json", "bad-at.json", "number.json", "spaces.json"} {
		if !listed(got, filepath.Join(journal, name)+": row 2 journal row: ") {
			t.Errorf("%s was judged by its modification time: %v", name, got)
		}
	}
	if len(got) != 6 || record.Get(report, "clear") != false {
		t.Errorf("unreadable %v", got)
	}
	var held []string
	for _, raw := range golden.List(record.Get(report, "liveHolds")) {
		held = append(held, filepath.Base(record.Get(golden.Obj(raw), "source").(string)))
	}
	sort.Strings(held)
	if strings.Join(held, ",") != "control.json,offset.json" {
		t.Errorf("holds %v: an at within the window holds, whatever the file's modification time", held)
	}
}

// A daemon record that names its boot is alive only in that boot, so a boot id the scan cannot
// read leaves row 3 unscanned and the pid unreadable rather than accepting the pid and start
// time alone. With the boot id readable, the same boot is alive (its Python process is a
// reference) and another boot is not; a record naming no boot needs no boot id.
func TestRetentionScanDoesNotDropTheBootCheckItCannotMake(t *testing.T) {
	h := newHost(t)
	proc := fakeProc(t, 4242, 777)
	link(t, "/usr/bin/python3", filepath.Join(proc, "4242", "exe"))
	write(t, filepath.Join(proc, "4242", "cmdline"), "python3\x00-m\x00relay\x00", 0o644)
	path := filepath.Join(h.state, "codex-session-relay", "scope-1", "daemon.json")
	write(t, path, `{"pid": 4242, "startTicks": 777, "bootId": "boot-1"}`, 0o600)
	row3 := func(report record.Object) any { return record.Get(surfaceRow(t, report, 3), "scanned") }
	report := h.scanProc(t, proc)
	if got := unreadable(report); row3(report) != false || !listed(got, path, "row 3 pid 4242", "boot id") || len(references(report)) != 0 {
		t.Errorf("an unreadable boot id: row 3 scanned %v, unreadable %v, references %v", row3(report), got, references(report))
	}
	bootID := filepath.Join(proc, "sys", "kernel", "random", "boot_id")
	for boot, alive := range map[string]bool{"boot-1\n": true, "boot-2\n": false} {
		write(t, bootID, boot, 0o644)
		report := h.scanProc(t, proc)
		if row3(report) != true || len(unreadable(report)) != 0 || (len(references(report)) == 1) != alive {
			t.Errorf("boot id %q: row 3 scanned %v, unreadable %v, references %v", boot, row3(report), unreadable(report), references(report))
		}
	}
	if err := os.Remove(bootID); err != nil {
		t.Fatal(err)
	}
	write(t, path, `{"pid": 4242, "startTicks": 777}`, 0o600)
	if report := h.scanProc(t, proc); row3(report) != true || len(unreadable(report)) != 0 || len(references(report)) != 1 {
		t.Errorf("a record naming no boot: row 3 scanned %v, unreadable %v, references %v", row3(report), unreadable(report), references(report))
	}
}

// Row 2 reads every journal root a retained Stop registration can write to, not only the one
// the default settings name: the default settings' root, the root of a settings document a
// hooks.json registration names as its argument, the default root for settings naming none, and
// a root a Stop-event claim names. A registration whose settings cannot be established is
// unreadable.
func TestRetentionScanReadsEveryJournalRootAStopRegistrationReaches(t *testing.T) {
	h := newHost(t)
	rootA := filepath.Join(h.home, "journal-a")
	rootB := filepath.Join(h.home, "journal-b")
	rootE := filepath.Join(h.home, "journal-e")
	rootD := filepath.Join(h.codex, "crw-completion-hook", "journal")
	settingsB := filepath.Join(h.home, "other", "settings-b.json")
	settingsC := filepath.Join(h.home, "other", "settings-c.json")
	write(t, filepath.Join(h.codex, "crw-completion-hook.json"), `{"journalRoot": "`+rootA+`"}`, 0o600)
	write(t, settingsB, `{"journalRoot": "`+rootB+`"}`, 0o600)
	write(t, settingsC, `{"configVersion": 1}`, 0o600)
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [
 {"type": "command", "command": "/usr/bin/true /opt/relay/completion_hook.py `+settingsB+`", "timeout": 10},
 {"type": "command", "command": "/usr/bin/true /opt/relay/crw-completion-hook `+settingsC+`", "timeout": 10},
 {"type": "command", "command": "/usr/bin/true /opt/relay/completion_hook.py \"$UNKNOWN/settings.json\"", "timeout": 10}]}]}}`, 0o600)
	key := strings.Repeat("e", 64)
	write(t, filepath.Join(h.codex, "crw-completion-hook", "stop-events", key+".json"), `{"eventKey": "`+key+`", "claimedAt": "2026-09-29T11:59:00Z", "claimedBy": {"pid": 1, "journalRoot": "`+rootE+`"}}`, 0o600)
	write(t, filepath.Join(rootE, "accepted", key+".outcome.json"), `{}`, 0o600)
	for _, root := range []string{rootA, rootB, rootD, rootE} {
		write(t, filepath.Join(root, "20260929", "recent.json"), `{"at": "2026-09-29T11:59:55Z"}`, 0o600)
	}
	report := h.scan(t)
	var held []string
	for _, raw := range golden.List(record.Get(report, "liveHolds")) {
		if hold := golden.Obj(raw); record.Get(hold, "row") == int64(2) {
			held = append(held, filepath.Dir(filepath.Dir(record.Get(hold, "source").(string))))
		}
	}
	sort.Strings(held)
	want := []string{rootA, rootB, rootD, rootE}
	sort.Strings(want)
	if strings.Join(held, ",") != strings.Join(want, ",") {
		t.Errorf("held journal roots %v, want %v", held, want)
	}
	if got := unreadable(report); !listed(got, "row 2 hooks.Stop[0].hooks[2].command", "$UNKNOWN") {
		t.Errorf("unreadable %v", got)
	}
}

// A directory the scan enumerates is listed explicitly: the Codex home (row 4), a cached plugin
// version's hook declarations (row 5) and a relay state directory's managed-start locks (row 6)
// that cannot be listed leave their row unscanned and the directory unreadable, never a
// completed scan that found nothing.
func TestRetentionScanDoesNotReadAnUnlistableDirectoryAsEmpty(t *testing.T) {
	skipAsRoot(t)
	h := newHost(t)
	hooks := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.4.0+test", "wiring", "hooks")
	scope := filepath.Join(h.state, "codex-session-relay", "scope-1")
	write(t, filepath.Join(hooks, "stop.json"), `{"hooks": {}}`, 0o644)
	write(t, filepath.Join(scope, "managed-start-"+strings.Repeat("c", 64)+".lock"), "", 0o600)
	for _, directory := range []string{h.codex, hooks, scope} {
		if err := os.Chmod(directory, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(directory, 0o755) })
	}
	report := h.scan(t)
	got := unreadable(report)
	for row, directory := range map[int64]string{4: h.codex, 5: hooks, 6: scope} {
		if record.Get(surfaceRow(t, report, row), "scanned") != false || !listed(got, directory+": PermissionError") {
			t.Errorf("row %d with %s unlistable: %s, unreadable %v", row, directory, golden.Canon(surfaceRow(t, report, row)), got)
		}
	}
}
