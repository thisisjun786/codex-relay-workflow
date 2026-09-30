package doctor_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// registrations is what RegisteredMatching finds of the host's registrations inside dir (as
// written, or once resolved), each as "row:field:names", and what it lists unreadable. dir ""
// counts nothing.
func (h *host) registrations(t *testing.T, dir string) ([]string, []string) {
	t.Helper()
	found, unreadable := doctor.RegisteredMatching(context.Background(), doctor.ScanOptions{Env: h.env}, func(path, resolves string) string {
		switch {
		case dir == "":
		case record.Within(filepath.Clean(path), dir):
			return filepath.Clean(path)
		case resolves != "" && record.Within(resolves, dir):
			return resolves
		}
		return ""
	})
	var out []string
	for _, raw := range found {
		one := golden.Obj(raw)
		if record.Get(one, "surface") == nil {
			t.Errorf("a registration names no surface: %s", golden.Canon(one))
		}
		out = append(out, scope.PyStr(record.Get(one, "row"))+":"+record.Text(one, "field")+":"+record.Text(one, "names"))
	}
	sort.Strings(out)
	return out, unreadable
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

// rowEntries is the unreadable entries that name one row.
func rowEntries(entries []string, row int) []string {
	var out []string
	for _, entry := range entries {
		if strings.Contains(entry, ": row "+strconv.Itoa(row)+" ") {
			out = append(out, entry)
		}
	}
	return out
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads through any mode, so EACCES cannot be produced")
	}
}

// stopHooks writes each command as a Stop hook of <CODEX_HOME>/hooks.json.
func stopHooks(t *testing.T, h *host, commands ...string) {
	t.Helper()
	var hooks []string
	for _, command := range commands {
		hooks = append(hooks, `{"type": "command", "command": `+strconv.Quote(command)+`, "timeout": 10}`)
	}
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [`+strings.Join(hooks, ", ")+`]}]}}`, 0o600)
}

// hookOutcome is whether the reading listed one hook command of <CODEX_HOME>/hooks.json as
// unreadable ("unreadable") or not ("clean").
func hookOutcome(entries []string, field string) string {
	if listed(entries, "row 9 "+field+" ") {
		return "unreadable"
	}
	return "clean"
}

// Every registration the host reads is followed to what it runs: the Stop settings' relay
// through the owned pointer (row 4), a cached plugin command through ${PLUGIN_ROOT} (row 5), the
// launcher copy (row 8), a user Stop command through $HOME (row 9) and an MCP server whose command
// holds a space (row 10), each named with its row and field when it reaches the runtime directory.
// A word the reading cannot expand ($RELAY_HOME, ${PLUGIN_ROOT} outside the cache) is unreadable,
// never skipped, and nothing is written.
func TestRegisteredMatchingFollowsEveryRegistrationIntoTheRuntime(t *testing.T) {
	h := newHost(t)
	dir, _ := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	crw := filepath.Join(dir, "bin", "crw")
	h.settings(t, map[string]string{"relayExecutable": filepath.Join(h.current(), "bin", "codex-session-relay")}, nil)
	link(t, dir, filepath.Join(h.home, "rt"))
	cache := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.4.0+test")
	link(t, crw, filepath.Join(cache, "wiring", "relay"))
	plugin := `"${PLUGIN_ROOT}/wiring/relay" hook`
	write(t, filepath.Join(cache, "wiring", "hooks", "stop.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": `+strconv.Quote(plugin)+`, "timeout": 10}]}]}}`, 0o644)
	stopHooks(t, h, `"$HOME/rt/bin/crw" hook --plugin-launch; exit 0`, `"$RELAY_HOME/bin/relay" hook`, plugin)
	spaced := filepath.Join(h.home, "my tools", "bridge")
	link(t, crw, spaced)
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.spaced]\ncommand = \""+spaced+"\"\nargs = [\"bridge\"]\n", 0o600)
	link(t, crw, filepath.Join(h.codex, "crw-stop-hook.py"))
	before := snapshot(t, h.home)
	found, unreadable := h.registrations(t, dir)
	if after := snapshot(t, h.home); after != before {
		t.Fatal("the reading changed the host")
	}
	want := []string{
		"10:mcp_servers.spaced:" + spaced,
		"4:relayExecutable:" + filepath.Join(h.current(), "bin", "codex-session-relay"),
		"5:hooks.Stop[0].hooks[0].command:" + filepath.Join(cache, "wiring", "relay"),
		"8:file:" + filepath.Join(h.codex, "crw-stop-hook.py"),
		"9:hooks.Stop[0].hooks[0].command:" + filepath.Join(h.home, "rt", "bin", "crw"),
	}
	for _, w := range want {
		if !slices.Contains(found, w) {
			t.Errorf("not found: %s\nfound %v", w, found)
		}
	}
	if got := rowEntries(unreadable, 9); len(got) != 2 || !listed(got, "hooks.Stop[0].hooks[1].command", "$RELAY_HOME/bin/relay", "RELAY_HOME") || !listed(got, "hooks.Stop[0].hooks[2].command", "${PLUGIN_ROOT}") {
		t.Fatalf("unreadable %v", unreadable)
	}
	if found, _ := h.registrations(t, filepath.Join(h.dest, "bin-0.2.0-bbbbbbbbbbbb")); len(found) != 0 {
		t.Fatalf("another runtime directory: %v", found)
	}
}

// A reference the reading resolves but cannot read is never dropped: a relayExecutable the
// reading may execute but not read (mode 0111), a bridgeExecutable behind a directory it may not
// search (mode 000), a bare command no PATH directory holds (a hook command, row 9, and an MCP
// server's, row 10), and a crw-*.json value that is not an absolute path - never resolved against
// the reading's own working directory, and a PATH whose relative directory comes first settles no
// bare name - are each listed with the row, file and field that named them.
func TestRegisteredMatchingListsWhatItCannotRead(t *testing.T) {
	skipAsRoot(t)
	h := newHost(t)
	secret := filepath.Join(h.home, "exec-only", "relay")
	write(t, secret, "#!/bin/sh\n", 0o111)
	locked := filepath.Join(h.home, "locked")
	write(t, filepath.Join(locked, "bin", "bridge"), "#!/bin/sh\n", 0o755)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	h.settings(t, map[string]string{"relayExecutable": secret}, map[string]string{"bridgeExecutable": filepath.Join(locked, "bin", "bridge")})
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=" + filepath.Join(h.home, "bin")}
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "crw-relay-not-installed stop", "timeout": 10}]}]}}`, 0o600)
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.gone]\ncommand = \"crw-bridge-not-installed\"\n", 0o600)
	_, got := h.registrations(t, "")
	for _, want := range [][]string{
		{"row 4", filepath.Join(h.codex, "crw-completion-hook.json"), "relayExecutable", secret},
		{"row 4", filepath.Join(h.codex, "crw-bridge-mcp.json"), "bridgeExecutable", filepath.Join(locked, "bin", "bridge")},
		{"row 9", filepath.Join(h.codex, "hooks.json"), "hooks.Stop[0].hooks[0].command", `"crw-relay-not-installed"`, filepath.Join(h.home, "bin")},
		{"row 10", filepath.Join(h.codex, "config.toml"), "mcp_servers.gone", `"crw-bridge-not-installed"`, filepath.Join(h.home, "bin")},
		{filepath.Join(h.codex, "hooks.json"), "which settings this Stop command reads could not be established", "crw-relay-not-installed"},
	} {
		if !listed(got, want...) {
			t.Errorf("no unreadable entry names %v: %v", want, got)
		}
	}
	if len(got) != 5 {
		t.Fatalf("unreadable %v", got)
	}

	h = newHost(t)
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, "codex-session-relay"), fakeCrw, 0o755)
	write(t, filepath.Join(cwd, "bin", "codex-thread-bridge"), fakeCrw, 0o755)
	write(t, filepath.Join(cwd, "cwd-relay"), fakeCrw, 0o755)
	t.Chdir(cwd)
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=.:/usr/bin:/bin"}
	write(t, filepath.Join(h.codex, "crw-completion-hook.json"), `{"relayExecutable": "codex-session-relay"}`, 0o600)
	write(t, filepath.Join(h.codex, "crw-bridge-mcp.json"), `{"bridgeExecutable": "bin/codex-thread-bridge", "args": ["serve", "--stdio"]}`, 0o600)
	stopHooks(t, h, "cwd-relay stop")
	_, got = h.registrations(t, "")
	for _, want := range [][]string{
		{"row 4", filepath.Join(h.codex, "crw-completion-hook.json"), "relayExecutable", `"codex-session-relay"`, "a relative path"},
		{"row 4", filepath.Join(h.codex, "crw-bridge-mcp.json"), "bridgeExecutable", `"bin/codex-thread-bridge"`, "a relative path"},
		{"row 4", filepath.Join(h.codex, "crw-bridge-mcp.json"), "args[0]", `"serve"`, "relative or empty directory comes first"},
		{"row 9", filepath.Join(h.codex, "hooks.json"), `"cwd-relay"`, "relative or empty directory comes first"},
	} {
		if !listed(got, want...) {
			t.Errorf("no unreadable entry names %v: %v", want, got)
		}
	}
	if len(rowEntries(got, 4))+len(rowEntries(got, 9)) != 4 {
		t.Errorf("unreadable %v", got)
	}
}

// A hook command is judged only as far as it is written in the grammar the reading reads: the
// native Stop command and the pre-native python3 -c bootstrap are read, and every construct whose
// meaning would have to be emulated is unreadable rather than guessed at: a function shadowing a
// PATH program, a PATH assignment in any form, a glob or brace pattern, a runner handing a shell
// or program text on, a file with no #!, a relative command or script, and a PATH whose relative
// directory comes first.
func TestRegisteredMatchingJudgesOnlyWhatItsGrammarReads(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	dir, _ := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	bin := filepath.Join(h.home, "bin")
	write(t, filepath.Join(bin, "codex-session-relay"), fakeCrw, 0o755)
	write(t, filepath.Join(bin, "runner"), fakeCrw+"runner", 0o755)
	link(t, "/bin/sh", filepath.Join(bin, "python3"))
	write(t, filepath.Join(bin, "no-hash-bang"), "python3 -m codex_session_relay hook\n", 0o755)
	odd := filepath.Join(h.home, "odd\ndir", "relay")
	link(t, filepath.Join(env, "bin", "codex-session-relay"), odd)
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=" + bin + ":/usr/bin:/bin"}
	cases := []struct{ command, want string }{
		{`"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0`, "clean"},
		{"python3 -c \"\nimport os, sys\nraise SystemExit(0)\n\" \"${PLUGIN_ROOT}/wiring/crw_stop_hook.py\"", "clean"},
		{`codex-session-relay hook`, "clean"},
		{`codex-session-relay() { command codex-session-relay hook stop; }; codex-session-relay`, "unreadable"},
		{`python3() { :; }; env python3 -m codex_session_relay hook`, "unreadable"},
		{`sh -c 'python3() { :; }'; python3 -m codex_session_relay`, "unreadable"},
		{`PATH=` + env + `/bin codex-session-relay hook stop`, "unreadable"},
		{`env PATH=` + env + `/bin codex-session-relay hook stop`, "unreadable"},
		{`export PATH=` + env + `/bin; codex-session-relay hook stop`, "unreadable"},
		{`PATH=` + env + `/bin; codex-session-relay hook stop`, "unreadable"},
		{filepath.Join(h.dest, "env-1-*", "bin", "codex-session-relay") + ` hook`, "unreadable"},
		{env + `/bin/{codex-session-relay,x} hook`, "unreadable"},
		{`runner -c 1 sh -c 'python3 -m codex_session_relay hook'`, "unreadable"},
		{`runner /tmp/x -c 'python3 -m codex_session_relay hook'`, "unreadable"},
		{`runner sh -c codex-session-relay`, "unreadable"},
		{`'` + odd + `' hook`, "clean"},
		{`no-hash-bang`, "unreadable"},
		{`scripts/stop-hook`, "unreadable"},
		{`sh scripts/stop-hook.sh`, "unreadable"},
	}
	var commands []string
	for _, c := range cases {
		commands = append(commands, c.command)
	}
	stopHooks(t, h, commands...)
	_, unreadable := h.registrations(t, "")
	for i, c := range cases {
		if got := hookOutcome(unreadable, "hooks.Stop[0].hooks["+strconv.Itoa(i)+"].command"); got != c.want {
			t.Errorf("%q: %s, want %s", c.command, got, c.want)
		}
	}
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=relbin:" + bin}
	stopHooks(t, h, `codex-session-relay hook`)
	if _, unreadable := h.registrations(t, ""); hookOutcome(unreadable, "hooks.Stop[0].hooks[0].command") != "unreadable" {
		t.Errorf("a PATH whose relative directory comes first: %v", unreadable)
	}
}

// A word given to a program the reading does not model is something that program may run only if
// it can be executed: a regular file with an execute bit. A socket or a FIFO cannot be, whatever
// its mode bits, so the App Server socket a bridge record passes with --socket leaves the command
// judged, where it made every registration naming it unreadable and every crw install remove
// refuse (todo 40).
func TestRegisteredMatchingDoesNotTakeWhatCannotBeExecutedForAProgram(t *testing.T) {
	h := newHost(t)
	bin := filepath.Join(h.home, "bin")
	write(t, filepath.Join(bin, "runner"), fakeCrw+"runner", 0o755)
	write(t, filepath.Join(bin, "no-hash-bang"), "echo hello\n", 0o755)
	short, err := os.MkdirTemp("", "crw-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	socket := filepath.Join(short, "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	fifo := filepath.Join(short, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{socket, fifo} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=" + bin + ":/usr/bin:/bin"}
	cases := []struct{ command, want string }{
		{"runner --socket " + socket, "clean"},
		{"runner " + fifo, "clean"},
		{"runner " + filepath.Join(bin, "no-hash-bang"), "unreadable"},
	}
	var commands []string
	for _, c := range cases {
		commands = append(commands, c.command)
	}
	stopHooks(t, h, commands...)
	_, unreadable := h.registrations(t, "")
	for i, c := range cases {
		if got := hookOutcome(unreadable, "hooks.Stop[0].hooks["+strconv.Itoa(i)+"].command"); got != c.want {
			t.Errorf("%q: %s, want %s", c.command, got, c.want)
		}
	}
}

// An MCP server is judged as Codex starts it: command and args exec'd with no shell, a shell's -c
// program read as a program (a login shell's being unreadable), a relative command resolved
// against the declared cwd and a bare one on the declared env's PATH, a path holding a newline
// resolved, and a relative command with no cwd unreadable. The bridge launcher (sh
// ./wiring/crw-bridge.sh from the version directory) is read and found to run the runtime through
// the pointer.
func TestRegisteredMatchingJudgesAnMCPServerAsCodexStartsIt(t *testing.T) {
	h := newHost(t)
	dir, _ := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	bin := filepath.Join(h.home, "bin")
	write(t, filepath.Join(bin, "codex-session-relay"), fakeCrw, 0o755)
	odd := filepath.Join(h.home, "odd\ndir", "relay")
	link(t, filepath.Join(dir, "bin", "crw"), odd)
	h.env = scope.Env{"HOME=" + h.home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=" + bin + ":/usr/bin:/bin"}
	servers := map[string]string{
		"login":   `command = "bash"` + "\n" + `args = ["-lc", "codex-session-relay mcp"]`,
		"program": `command = "sh"` + "\n" + `args = ["-c", "exec ` + dir + `/bin/codex-session-relay mcp"]`,
		"cwd":     `command = "./bin/codex-session-relay"` + "\n" + `cwd = "` + dir + `"`,
		"envpath": `command = "codex-session-relay"` + "\n" + `env = { PATH = "` + dir + `/bin" }`,
		"nocwd":   `command = "./bin/codex-session-relay"`,
		"newline": `command = ` + strconv.Quote(odd),
		"native":  `command = "codex-session-relay"` + "\n" + `args = ["mcp"]`,
	}
	var toml []string
	for name, body := range servers {
		toml = append(toml, "[mcp_servers."+name+"]\n"+body+"\n")
	}
	write(t, filepath.Join(h.codex, "config.toml"), strings.Join(toml, "\n"), 0o600)
	cache := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.5.0")
	write(t, filepath.Join(cache, "wiring", "crw-bridge.sh"), "#!/bin/sh\n# The task bridge the plugin declares.\nexec \"$HOME/.local/share/crw-runtime/current/bin/codex-thread-bridge\" --plugin-launch \"$@\"\n", 0o755)
	write(t, filepath.Join(cache, "wiring", "mcp.json"), `{"mcpServers": {"bridge": {"command": "sh", "args": ["./wiring/crw-bridge.sh"], "cwd": "."}}}`, 0o644)
	found, unreadable := h.registrations(t, dir)
	for field, want := range map[string]string{
		"mcp_servers.login": "unreadable", "mcp_servers.program": "found", "mcp_servers.cwd": "found", "mcp_servers.envpath": "found",
		"mcp_servers.nocwd": "unreadable", "mcp_servers.newline": "found", "mcp_servers.native": "clean",
	} {
		got := "clean"
		switch {
		case listed(unreadable, "row 10 "+field+" "):
			got = "unreadable"
		case slices.ContainsFunc(found, func(f string) bool { return strings.HasPrefix(f, "10:"+field+":") }):
			got = "found"
		}
		if got != want {
			t.Errorf("%s: %s, want %s", field, got, want)
		}
	}
	if !slices.ContainsFunc(found, func(f string) bool { return strings.HasPrefix(f, "5:mcpServers.bridge:") }) || len(rowEntries(unreadable, 5)) != 0 {
		t.Errorf("the bridge launcher: found %v, unreadable %v", found, unreadable)
	}
}

// A declaration the host reads is validated at every level: a hooks document, event, group or
// hook that is not what Codex reads, a command that is not a string, a type other than command, a
// timeout that is not a number, an MCP server table, entry or field of the wrong kind, and a
// crw-*.json value that is neither a string nor a list of strings are each unreadable; a
// malformed Stop entry leaves the settings it reads unknown as well.
func TestRegisteredMatchingListsAMalformedDeclaration(t *testing.T) {
	for _, c := range []struct {
		name, file, text string
		row              int64
		stop             bool
	}{
		{"command not a string", "hooks.json", `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": 42}]}]}}`, 9, true},
		{"hooks not an object", "hooks.json", `{"hooks": ["true"]}`, 9, true},
		{"event not a list", "hooks.json", `{"hooks": {"Stop": {"hooks": []}}}`, 9, true},
		{"group hooks not a list", "hooks.json", `{"hooks": {"Stop": [{"hooks": "true"}]}}`, 9, true},
		{"hook not an object", "hooks.json", `{"hooks": {"Stop": [{"hooks": ["true"]}]}}`, 9, true},
		{"another type", "hooks.json", `{"hooks": {"Stop": [{"hooks": [{"type": "prompt", "command": "true"}]}]}}`, 9, true},
		{"timeout not a number", "hooks.json", `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "true", "timeout": "10"}]}]}}`, 9, true},
		{"another event", "hooks.json", `{"hooks": {"SessionStart": [{"hooks": [{"command": null}]}]}}`, 9, false},
		{"cached hook", "plugins/cache/crw/crw/0.5.0/wiring/hooks/stop.json", `{"hooks": {"Stop": [{"hooks": [{"command": 42}]}]}}`, 5, true},
		{"cached servers not an object", "plugins/cache/crw/crw/0.5.0/wiring/mcp.json", `{"mcpServers": ["x"]}`, 5, false},
		{"cached server command", "plugins/cache/crw/crw/0.5.0/wiring/mcp.json", `{"mcpServers": {"x": {"command": 42}}}`, 5, false},
		{"cached server args", "plugins/cache/crw/crw/0.5.0/.mcp.json", `{"mcpServers": {"x": {"command": "true", "args": "--x"}}}`, 5, false},
		{"server env", "config.toml", "[mcp_servers.x]\ncommand = \"true\"\nenv = { PATH = 1 }\n", 10, false},
		{"server cwd", "config.toml", "[mcp_servers.x]\ncommand = \"true\"\ncwd = 1\n", 10, false},
		{"servers not a table", "config.toml", "mcp_servers = 1\n", 10, false},
		{"setting not a string", "crw-completion-hook.json", `{"relayExecutable": 42}`, 4, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHost(t)
			write(t, filepath.Join(h.codex, filepath.FromSlash(c.file)), c.text, 0o600)
			_, got := h.registrations(t, "")
			if !listed(got, filepath.Join(h.codex, filepath.FromSlash(c.file))+": row "+strconv.FormatInt(c.row, 10)+" ") {
				t.Fatalf("row %d: unreadable %v", c.row, got)
			}
			if listed(got, "which settings this Stop command reads could not be established") != c.stop {
				t.Fatalf("the Stop settings unknown %v, want %v: %v", !c.stop, c.stop, got)
			}
		})
	}
	h := newHost(t)
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.remote]\nurl = \"https://example.invalid/mcp\"\n", 0o600)
	if _, got := h.registrations(t, ""); len(got) != 0 {
		t.Fatalf("a server reached over HTTP is not malformed: %v", got)
	}
}

// The reading never ends on a hook or MCP text: an empty quoted word, an empty program and a lone
// separator are read, and so is an MCP server with an empty argument.
func TestRegisteredMatchingReadsEmptyWordsAndPrograms(t *testing.T) {
	h := newHost(t)
	stopHooks(t, h, `""`, `true ""`, `"" x`, `$''`, ``, `;`, `sh -c ""`, `exec "" "$@"`)
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.x]\ncommand = \"true\"\nargs = [\"\"]\n", 0o600)
	_, unreadable := h.registrations(t, "")
	for _, entry := range unreadable {
		if strings.Contains(entry, "reading of it failed") {
			t.Errorf("a panic was recovered: %s", entry)
		}
	}
	for i, want := range []string{"unreadable", "clean", "unreadable", "unreadable", "clean", "unreadable", "clean", "unreadable"} {
		if got := hookOutcome(unreadable, "hooks.Stop[0].hooks["+strconv.Itoa(i)+"].command"); got != want {
			t.Errorf("hook %d: %s, want %s", i, got, want)
		}
	}
}

// A Stop command's settings are read as row 4 reads a crw-*.json record: the document its
// argument names, the one CRW_COMPLETION_HOOK_CONFIG names when it names none, and the default
// settings for crw hook --plugin-launch whatever the variable says. A command that assigns the
// variable itself, or names its settings through an expansion the reading does not make, leaves
// them unknown.
func TestRegisteredMatchingReadsTheSettingsAStopCommandReads(t *testing.T) {
	h := newHost(t)
	dir, _ := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	crw := filepath.Join(h.current(), "bin", "crw")
	settings := filepath.Join(h.home, "other", "s.json")
	relay := filepath.Join(dir, "bin", "codex-session-relay")
	write(t, settings, `{"relayExecutable": "`+relay+`"}`, 0o600)
	names := func(found []string) bool { return slices.Contains(found, "4:relayExecutable:"+relay) }
	stopHooks(t, h, crw+` hook `+settings)
	if found, unreadable := h.registrations(t, dir); !names(found) || len(unreadable) != 0 {
		t.Errorf("a settings argument: found %v, unreadable %v", found, unreadable)
	}
	stopHooks(t, h, `CRW_COMPLETION_HOOK_CONFIG=`+settings+` `+crw+` hook`, `env CRW_COMPLETION_HOOK_CONFIG=`+settings+` `+crw+` hook`, crw+` hook "$UNKNOWN/s.json"`)
	_, unreadable := h.registrations(t, dir)
	for i := range 3 {
		if !listed(unreadable, "hooks.Stop[0].hooks["+strconv.Itoa(i)+"].command: which settings this Stop command reads could not be established") {
			t.Errorf("hook %d: %v", i, unreadable)
		}
	}
	stopHooks(t, h, crw+` hook`)
	h.env = h.env.With("CRW_COMPLETION_HOOK_CONFIG", settings)
	if found, unreadable := h.registrations(t, dir); !names(found) || len(unreadable) != 0 {
		t.Errorf("the environment's settings: found %v, unreadable %v", found, unreadable)
	}
	h.env = h.env.With("CRW_COMPLETION_HOOK_CONFIG", "other/s.json")
	if _, unreadable := h.registrations(t, dir); !listed(unreadable, "CRW_COMPLETION_HOOK_CONFIG", "not an absolute path") {
		t.Errorf("a relative settings path: %v", unreadable)
	}
	stopHooks(t, h, crw+` hook --plugin-launch`)
	h.env = h.env.With("CRW_COMPLETION_HOOK_CONFIG", settings)
	if found, unreadable := h.registrations(t, dir); names(found) || len(unreadable) != 0 {
		t.Errorf("--plugin-launch: found %v, unreadable %v", found, unreadable)
	}
}

// A directory the reading enumerates is listed explicitly: the Codex home (row 4) and a cached
// plugin version's hook declarations (row 5) that cannot be listed are unreadable, never read as
// empty. A stray file in the plugin cache is not a version directory: Codex loads nothing from it.
func TestRegisteredMatchingDoesNotReadAnUnlistableDirectoryAsEmpty(t *testing.T) {
	h := newHost(t)
	write(t, filepath.Join(h.codex, "plugins", "cache", "crw", "crw", ".DS_Store"), "x", 0o644)
	write(t, filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.4.0", "wiring", "hooks", "stop.json"), `{"hooks": {}}`, 0o644)
	if _, unreadable := h.registrations(t, ""); len(unreadable) != 0 {
		t.Fatalf("a stray file in the cache: %v", unreadable)
	}
	skipAsRoot(t)
	hooks := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.4.0", "wiring", "hooks")
	for _, directory := range []string{h.codex, hooks} {
		if err := os.Chmod(directory, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(directory, 0o755) })
	}
	_, unreadable := h.registrations(t, "")
	for _, directory := range []string{h.codex, hooks} {
		if !listed(unreadable, directory+": PermissionError") {
			t.Errorf("%s unlistable: %v", directory, unreadable)
		}
	}
}

// fakeProc is a procfs root (the Proc seam) holding one process whose start time is ticks; the
// caller lays out its exe and cmdline.
func fakeProc(t *testing.T, pid int, ticks int64) string {
	t.Helper()
	proc := t.TempDir()
	mkdir(t, filepath.Join(proc, "self"))
	write(t, filepath.Join(proc, strconv.Itoa(pid), "stat"), strconv.Itoa(pid)+" (relay worker) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 "+strconv.FormatInt(ticks, 10)+" 0 0\n", 0o644)
	return proc
}

// aliveProc is a procfs root holding one alive, readable process (pid 4242, start time 777).
func aliveProc(t *testing.T) string {
	t.Helper()
	proc := fakeProc(t, 4242, 777)
	link(t, "/bin/sh", filepath.Join(proc, "4242", "exe"))
	write(t, filepath.Join(proc, "4242", "cmdline"), "codex-session-relay\x00service\x00", 0o644)
	return proc
}

func (h *host) daemons(proc string) doctor.DaemonRecords {
	return doctor.RecordedDaemons(doctor.ScanOptions{Env: h.env, Proc: proc, ScopeRegistry: filepath.Join(h.home, "scopes")})
}

func (h *host) daemon(t *testing.T, pid int, ticks int64) string {
	t.Helper()
	path := filepath.Join(h.state, "codex-session-relay", "scope-1", "daemon.json")
	write(t, path, `{"pid": `+strconv.Itoa(pid)+`, "startTicks": `+strconv.FormatInt(ticks, 10)+`}`, 0o600)
	return path
}

// An alive recorded pid whose executable or command line cannot be read is listed as
// unreadable: what it runs is unknown. A pid whose process is gone, or is a zombie, is not alive
// and is not listed. Where there is no procfs (darwin), whether a recorded pid is alive cannot be
// read, so it is unreadable rather than read as gone.
func TestRecordedDaemonsListAnAliveProcessTheyCannotRead(t *testing.T) {
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
			if got := h.daemons(proc); !listed(got.Unreadable, "row 3", path, "pid", "4242") || !slices.Equal(got.Alive, []int{4242}) {
				t.Fatalf("alive %v, unreadable %v", got.Alive, got.Unreadable)
			}
		})
	}
	h := newHost(t)
	proc := fakeProc(t, 4242, 777)
	h.daemon(t, 4243, 777) // gone: no /proc/4243 while /proc/self exists
	write(t, filepath.Join(proc, "4242", "stat"), "4242 (relay) Z 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777 0 0\n", 0o644)
	write(t, filepath.Join(h.state, "codex-session-relay", "scope-2", "daemon.json"), `{"pid": 4242, "startTicks": 777}`, 0o600)
	if got := h.daemons(proc); len(got.Unreadable) != 0 || len(got.Alive) != 0 {
		t.Fatalf("a gone or zombie pid: alive %v, unreadable %v", got.Alive, got.Unreadable)
	}
	h = newHost(t)
	path := h.daemon(t, os.Getpid(), 777)
	if got := h.daemons(t.TempDir()); !listed(got.Unreadable, path, "row 3 pid", "no process table") {
		t.Fatalf("without a process table: %v", got.Unreadable)
	}
}

// A daemon record that names its boot is alive only in that boot, so a boot id that cannot be
// read leaves the pid unreadable rather than accepting the pid and start time alone. With the
// boot id readable, the same boot is alive and another boot is not; a record naming no boot
// needs no boot id.
func TestRecordedDaemonsDoNotDropTheBootCheckTheyCannotMake(t *testing.T) {
	h := newHost(t)
	proc := aliveProc(t)
	path := filepath.Join(h.state, "codex-session-relay", "scope-1", "daemon.json")
	write(t, path, `{"pid": 4242, "startTicks": 777, "bootId": "boot-1"}`, 0o600)
	if got := h.daemons(proc); !listed(got.Unreadable, path, "row 3 pid 4242", "boot id") || len(got.Alive) != 0 {
		t.Errorf("an unreadable boot id: alive %v, unreadable %v", got.Alive, got.Unreadable)
	}
	bootID := filepath.Join(proc, "sys", "kernel", "random", "boot_id")
	for boot, alive := range map[string]bool{"boot-1\n": true, "boot-2\n": false} {
		write(t, bootID, boot, 0o644)
		if got := h.daemons(proc); len(got.Unreadable) != 0 || (len(got.Alive) == 1) != alive {
			t.Errorf("boot id %q: alive %v, unreadable %v", boot, got.Alive, got.Unreadable)
		}
	}
	if err := os.Remove(bootID); err != nil {
		t.Fatal(err)
	}
	write(t, path, `{"pid": 4242, "startTicks": 777}`, 0o600)
	if got := h.daemons(proc); len(got.Unreadable) != 0 || len(got.Alive) != 1 {
		t.Errorf("a record naming no boot: alive %v, unreadable %v", got.Alive, got.Unreadable)
	}
}

// Every relay state directory the relay uses is read, not only those the reading's own
// environment names: CODEX_SESSION_RELAY_STATE with ~ expanded as the relay expands it, a scope
// directory reached through a link, and each stateDir the relay's scope registry records, whose
// own pids are judged too. A state directory that cannot be established (a relative
// CODEX_SESSION_RELAY_STATE, a registry that cannot be listed) is unreadable.
func TestRecordedDaemonsReadEveryRelayStateDirectory(t *testing.T) {
	daemon := `{"pid": 4242, "startTicks": 777}`
	for name, layout := range map[string]func(h *host) string{
		"tilde": func(h *host) string {
			h.env = h.env.With("CODEX_SESSION_RELAY_STATE", "~/rs")
			write(t, filepath.Join(h.home, "rs", "daemon.json"), daemon, 0o600)
			return filepath.Join(h.home, "rs")
		},
		"linked scope": func(h *host) string {
			write(t, filepath.Join(h.home, "elsewhere", "scope-1", "daemon.json"), daemon, 0o600)
			link(t, filepath.Join(h.home, "elsewhere", "scope-1"), filepath.Join(h.state, "codex-session-relay", "scope-1"))
			return filepath.Join(h.state, "codex-session-relay", "scope-1")
		},
		"registry": func(h *host) string {
			write(t, filepath.Join(h.home, "srv", "daemon.json"), `{"pid": 1, "startTicks": 1}`, 0o600)
			write(t, filepath.Join(h.home, "scopes", "abcd.json"), `{"stateDir": "`+filepath.Join(h.home, "srv")+`", "pid": 4242, "startTicks": 777}`, 0o600)
			return filepath.Join(h.home, "srv")
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			state := layout(h)
			if got := h.daemons(aliveProc(t)); !slices.Contains(got.States, state) || !slices.Equal(got.Alive, []int{4242}) || len(got.Unreadable) != 0 {
				t.Fatalf("states %v, alive %v, unreadable %v; want %s and pid 4242", got.States, got.Alive, got.Unreadable, state)
			}
		})
	}
	h := newHost(t)
	h.env = h.env.With("CODEX_SESSION_RELAY_STATE", "rs")
	if got := h.daemons(aliveProc(t)); !listed(got.Unreadable, "CODEX_SESSION_RELAY_STATE", "rows 3 and 6") {
		t.Errorf("a relative state directory: %v", got.Unreadable)
	}
	skipAsRoot(t)
	h = newHost(t)
	mkdir(t, filepath.Join(h.home, "scopes"))
	if err := os.Chmod(filepath.Join(h.home, "scopes"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.home, "scopes"), 0o755) })
	if got := h.daemons(aliveProc(t)); !listed(got.Unreadable, filepath.Join(h.home, "scopes"), "PermissionError") {
		t.Errorf("an unlistable scope registry: %v", got.Unreadable)
	}
}

// A daemon.json or scope claim whose pid or workerPid is present but not a pid (a string, a
// float, a boolean, zero, a negative or oversized number) names a process nothing can find: the
// record is unreadable. An absent or null workerPid, or a null pid, records no process.
func TestRecordedDaemonsListARecordWhosePidIsNotAPid(t *testing.T) {
	for _, pid := range []string{`"4242"`, `4242.0`, `true`, `0`, `-1`, `99999999999`, `[4242]`} {
		for _, key := range []string{"pid", "workerPid"} {
			t.Run(key+"="+pid, func(t *testing.T) {
				h := newHost(t)
				path := filepath.Join(h.state, "codex-session-relay", "scope-1", "daemon.json")
				write(t, path, `{"pid": 4242, "startTicks": 777, "`+key+`": `+pid+`}`, 0o600)
				if got := h.daemons(aliveProc(t)); !listed(got.Unreadable, path+": row 3 "+key+": ", "is not a pid") {
					t.Fatalf("unreadable %v", got.Unreadable)
				}
			})
		}
	}
	for _, document := range []string{`{"pid": 4242, "startTicks": 777}`, `{"pid": 4242, "startTicks": 777, "workerPid": null}`, `{"pid": null, "workerPid": null}`} {
		h := newHost(t)
		write(t, filepath.Join(h.state, "codex-session-relay", "scope-1", "daemon.json"), document, 0o600)
		if got := h.daemons(aliveProc(t)); len(got.Unreadable) != 0 {
			t.Errorf("%s: unreadable %v", document, got.Unreadable)
		}
	}
}

// A caller that removes a runtime reads the relay records of its own environment: with
// CODEX_SESSION_RELAY_SCOPE_DIR set, that registry alone, the only one a relay started there reads
// and claims its scope in; without it, the production one. (todo 40 review: `crw install remove`
// in the isolated home read this machine's live registry, and one malformed claim there refused
// the remove.)
func TestRecordedDaemonsReadTheRegistryTheRelayResolves(t *testing.T) {
	h := newHost(t)
	production := filepath.Join(h.home, "scopes")
	malformed := filepath.Join(production, "0000000000000000.json")
	write(t, malformed, "{not json", 0o600)
	isolated := filepath.Join(h.home, "isolated-scopes")
	served := filepath.Join(h.home, "srv")
	write(t, filepath.Join(served, "daemon.json"), `{"pid": 4242, "startTicks": 777}`, 0o600)
	write(t, filepath.Join(isolated, "abcd.json"), `{"stateDir": "`+served+`"}`, 0o600)
	proc := aliveProc(t)
	read := func(env scope.Env) doctor.DaemonRecords {
		return doctor.RecordedDaemons(doctor.ScanOptions{Env: env, Proc: proc, ScopeRegistry: production})
	}
	got := read(h.env)
	if strings.Join(got.Registries, "|") != production || !listed(got.Unreadable, malformed) {
		t.Errorf("no override: registries %q, unreadable %q; want the production registry and its malformed claim", got.Registries, got.Unreadable)
	}
	for _, spelled := range []string{isolated, "~/isolated-scopes"} {
		got := read(h.env.With("CODEX_SESSION_RELAY_SCOPE_DIR", spelled))
		if strings.Join(got.Registries, "|") != isolated || len(got.Unreadable) != 0 || !slices.Contains(got.States, served) || !slices.Equal(got.Alive, []int{4242}) {
			t.Errorf("override %q: registries %q, states %q, alive %v, unreadable %q; want %s alone and the daemon its claim names", spelled, got.Registries, got.States, got.Alive, got.Unreadable, isolated)
		}
	}
	// An override that cannot be made absolute is unknown, never replaced by the production one.
	got = read(h.env.With("CODEX_SESSION_RELAY_SCOPE_DIR", "isolated-scopes"))
	if len(got.Registries) != 0 || !listed(got.Unreadable, "$CODEX_SESSION_RELAY_SCOPE_DIR", "rows 3 and 6") {
		t.Errorf("a relative override: registries %q, unreadable %q", got.Registries, got.Unreadable)
	}
}
