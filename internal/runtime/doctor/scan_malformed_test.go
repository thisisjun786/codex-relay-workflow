package doctor_test

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// malformedFixture is a host with a Go runtime in the destination and the files a malformed
// registration can lean on: links and wrappers that run the runtime's crw, and their innocent
// counterparts.
type malformedFixture struct {
	h                                        *host
	dir, crw, tools, work, other, cwd1, cwd2 string
	substitute                               *strings.Replacer
}

func newMalformedFixture(t *testing.T) *malformedFixture {
	t.Helper()
	h := newHost(t)
	dir, _ := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	f := &malformedFixture{h: h, dir: dir, crw: filepath.Join(dir, "bin", "crw"), tools: filepath.Join(h.home, "tools"),
		work: filepath.Join(h.home, "work"), other: filepath.Join(h.home, "other"), cwd1: filepath.Join(h.home, "cwd1"), cwd2: filepath.Join(h.home, "cwd2")}
	link(t, f.crw, filepath.Join(f.tools, "launch"))
	link(t, f.crw, filepath.Join(h.home, "tool box", "launch"))
	link(t, f.crw, filepath.Join(f.other, "env-tools", "launch"))
	link(t, f.crw, filepath.Join(f.work, "args"))
	link(t, "/usr/bin/true", filepath.Join(f.cwd1, "launch"))
	link(t, f.crw, filepath.Join(f.cwd2, "launch"))
	write(t, filepath.Join(f.tools, "wrapped"), "#!/bin/sh\nexec "+f.crw+" bridge\n", 0o755)
	write(t, filepath.Join(f.tools, "innocent"), "#!/bin/sh\nexec /usr/bin/true\n", 0o755)
	write(t, filepath.Join(f.tools, "homewrap"), "#!/bin/sh\nexec \"$HOME/env-tools/launch\" bridge\n", 0o755)
	f.substitute = strings.NewReplacer("{crw}", f.crw, "{dir}", dir, "{tools}", f.tools, "{home}", h.home, "{work}", f.work, "{other}", f.other, "{cwd1}", f.cwd1, "{cwd2}", f.cwd2)
	return f
}

// A hooks.json hook or config.toml MCP server that is not what Codex reads is CRW's, and holds a
// removal back, only when the reading finds CRW in it (decision 68, as CRW-251 amended it): a
// malformed entry is read by the reader that reads a well-formed one, on every string it holds,
// before it is listed, so a foreign entry with a typo no longer refuses a removal while one that
// names or runs CRW - by its name, a path, a link, a wrapper script, a quoted path, an sh -c
// program, in any table of an array - still does. The same input read with ScanOptions.Foreign
// lists every one, so the difference is the ownership rule and nothing else.
func TestRegisteredMatchingHoldsARemovalBackForAMalformedEntryOnlyWhenItCouldBeCRWs(t *testing.T) {
	const config, hooks = "config.toml", "hooks.json"
	for _, c := range []struct {
		name, file, text, field string
		ours                    bool
	}{
		// Another program's registrations: nothing in them is CRW's.
		{"a server whose args are 42", config, "[mcp_servers.other]\ncommand = \"python3\"\nargs = 42\n", "mcp_servers.other", false},
		{"a server whose command is 5", config, "[mcp_servers.third]\ncommand = 5\n", "mcp_servers.third", false},
		{"a server whose env is not strings", config, "[mcp_servers.fourth]\ncommand = \"node\"\nargs = [\"x\"]\nenv = { PATH = 1 }\n", "mcp_servers.fourth", false},
		{"a server that is not a table", config, "[mcp_servers]\nlone = \"x\"\n", "mcp_servers.lone", false},
		{"a wrapper that runs only true", config, "[mcp_servers.five]\ncommand = \"{tools}/innocent\"\nargs = 42\n", "mcp_servers.five", false},
		{"a key is not a path: cwd holds a link named args", config, "[mcp_servers.six]\ncommand = \"/usr/bin/true\"\nargs = 42\ncwd = \"{work}\"\n", "mcp_servers.six", false},
		{"an array of tables of another program", config, "[[mcp_servers.seven]]\ncommand = \"python3\"\nargs = [\"x\"]\n", "mcp_servers.seven", false},
		{"an array of tables of another program that has a url", config, "[[mcp_servers.k]]\nurl = \"https://example.invalid/mcp\"\n", "mcp_servers.k", false},
		{"an inline array of tables of another program", config, "[mcp_servers]\nk = [{ command = \"./launch\", cwd = \"{cwd1}\" }]\n", "mcp_servers.k", false},
		{"a SessionStart hook whose command is 42", hooks, `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": 42}]}]}}`, "hooks.SessionStart[0].hooks[0].command", false},
		{"a hook whose timeout is a string", hooks, `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "python3 x.py", "timeout": "10"}]}]}}`, "hooks.SessionStart[0].hooks[0].command", false},
		{"an event that is not a list", hooks, `{"hooks": {"SessionStart": {"hooks": []}}}`, "hooks.SessionStart", false},
		{"a group whose hooks are a string", hooks, `{"hooks": {"Stop": [{"hooks": "true"}]}}`, "hooks.Stop[0].hooks", false},
		{"a hook that is a string", hooks, `{"hooks": {"Stop": [{"hooks": ["python3 x.py"]}]}}`, "hooks.Stop[0].hooks[0].command", false},
		{"a Stop hook of another type", hooks, `{"hooks": {"Stop": [{"hooks": [{"type": "prompt", "command": "python3 x.py"}]}]}}`, "hooks.Stop[0].hooks[0].command", false},

		// A malformed entry that could be CRW's.
		{"crw as the command, args 42", config, "[mcp_servers.a]\ncommand = \"{crw}\"\nargs = 42\n", "mcp_servers.a", true},
		{"the name CRW registers the bridge under", config, "[mcp_servers.codex-thread-bridge]\ncommand = 5\n", "mcp_servers.codex-thread-bridge", true},
		{"args that are one string naming the bridge", config, "[mcp_servers.c]\ncommand = \"sh\"\nargs = \"-c codex-thread-bridge\"\n", "mcp_servers.c", true},
		{"a link to crw as the command", config, "[mcp_servers.d]\ncommand = \"{tools}/launch\"\nargs = 42\n", "mcp_servers.d", true},
		{"a link under $HOME in a directory with a space", config, "[mcp_servers.e]\ncommand = \"$HOME/tool box/launch\"\nargs = 42\n", "mcp_servers.e", true},
		{"a link under ~ in a directory with a space", config, "[mcp_servers.f]\ncommand = \"~/tool box/launch\"\nargs = 42\n", "mcp_servers.f", true},
		{"a wrapper script that execs crw", config, "[mcp_servers.g]\ncommand = \"{tools}/wrapped\"\nargs = 42\n", "mcp_servers.g", true},
		{"a bare command on the table's env.PATH", config, "[mcp_servers.h]\ncommand = \"launch\"\nargs = 42\nenv = { PATH = \"{tools}\" }\n", "mcp_servers.h", true},
		{"a relative command in the table's cwd", config, "[mcp_servers.i]\ncommand = \"./launch\"\nargs = 42\ncwd = \"{tools}\"\n", "mcp_servers.i", true},
		{"a server that is not a table but names crw", config, "[mcp_servers]\nj = \"{crw}\"\n", "mcp_servers.j", true},
		{"an inline array of tables whose own cwd holds a link to crw", config, "[mcp_servers]\nk = [{ command = \"./launch\", cwd = \"{cwd2}\" }]\n", "mcp_servers.k", true},
		{"an inline array of tables found on its own env.PATH", config, "[mcp_servers]\nk = [{ command = \"launch\", env = { PATH = \"{tools}\" } }]\n", "mcp_servers.k", true},
		{"an array of tables with a url that still names crw in its args", config, "[[mcp_servers.k]]\nurl = \"https://example.invalid/mcp\"\nargs = [\"{crw}\"]\n", "mcp_servers.k", true},
		{"an array of tables that names crw", config, "[[mcp_servers.k]]\ncommand = \"{crw}\"\n", "mcp_servers.k", true},
		{"an array of tables where only the second table's cwd holds a link to crw", config, "[[mcp_servers.k]]\ncommand = \"./launch\"\ncwd = \"{cwd1}\"\n\n[[mcp_servers.k]]\ncommand = \"./launch\"\ncwd = \"{cwd2}\"\n", "mcp_servers.k", true},
		{"crw's path as an env key", config, "[mcp_servers.l]\ncommand = \"python3\"\nargs = 42\n[mcp_servers.l.env]\n\"{crw}\" = \"1\"\n", "mcp_servers.l", true},
		{"a wrapper that execs under the table's own env.HOME", config, "[mcp_servers.m]\ncommand = \"{tools}/homewrap\"\nargs = 42\nenv = { HOME = \"{other}\" }\n", "mcp_servers.m", true},
		{"a hook whose command is a list naming crw", hooks, `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": ["{crw}", "hook"]}]}]}}`, "hooks.Stop[0].hooks[0].command", true},
		{"a hook of another type running crw", hooks, `{"hooks": {"Stop": [{"hooks": [{"type": "prompt", "command": "{crw} hook"}]}]}}`, "hooks.Stop[0].hooks[0].command", true},
		{"a hook with a string timeout running crw", hooks, `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "{crw} hook", "timeout": "10"}]}]}}`, "hooks.Stop[0].hooks[0].command", true},
		{"a hook running a link in a quoted path with a space", hooks, `{"hooks": {"Stop": [{"hooks": [{"type": "prompt", "command": "\"{home}/tool box/launch\" hook"}]}]}}`, "hooks.Stop[0].hooks[0].command", true},
		{"a hook running the same under sh -c", hooks, `{"hooks": {"Stop": [{"hooks": [{"type": "prompt", "command": "sh -c '\"{home}/tool box/launch\" hook'"}]}]}}`, "hooks.Stop[0].hooks[0].command", true},
		{"a hook running a wrapper that execs crw", hooks, `{"hooks": {"Stop": [{"hooks": [{"type": "prompt", "command": "{tools}/wrapped"}]}]}}`, "hooks.Stop[0].hooks[0].command", true},
		{"an event that is not a list but names crw", hooks, `{"hooks": {"Stop": {"command": "{crw} hook"}}}`, "hooks.Stop", true},
		{"a group whose hooks are a string naming crw", hooks, `{"hooks": {"Stop": [{"hooks": "{crw} hook"}]}}`, "hooks.Stop[0].hooks", true},
		{"a hook that is a string naming crw", hooks, `{"hooks": {"Stop": [{"hooks": ["{crw} hook"]}]}}`, "hooks.Stop[0].hooks[0].command", true},

		// CRW's own records and plugin cache refuse on anything they hold that is malformed.
		{"a cached MCP server of the plugin whose args are 42", "plugins/cache/crw/crw/0.5.0/wiring/mcp.json", `{"mcpServers": {"x": {"command": "python3", "args": 42}}}`, "mcpServers.x", true},
		{"a cached hook of the plugin whose command is 42", "plugins/cache/crw/crw/0.5.0/wiring/hooks/stop.json", `{"hooks": {"Stop": [{"hooks": [{"command": 42}]}]}}`, "hooks.Stop[0].hooks[0].command", true},
		{"a CRW settings record whose value is 42", "crw-completion-hook.json", `{"relayExecutable": 42}`, "relayExecutable", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMalformedFixture(t)
			path := filepath.Join(f.h.codex, filepath.FromSlash(c.file))
			write(t, path, f.substitute.Replace(c.text), 0o600)
			row := "4"
			switch {
			case c.file == config:
				row = "10"
			case c.file == hooks:
				row = "9"
			case strings.HasPrefix(c.file, "plugins/"):
				row = "5"
			}
			note := path + ": row " + row + " " + c.field + ": "
			if _, every := f.h.registrationsAs(t, f.dir, true); !listed(every, note) {
				t.Fatalf("read as a grammar test reads it, the reading does not list the malformed entry %q:\n%v", note, every)
			}
			if _, got := f.h.registrationsAs(t, f.dir, false); listed(got, note) != c.ours {
				t.Fatalf("listed %v, want %v (an entry that names nothing of CRW's is dropped, one that could be CRW's is kept):\n%v", !c.ours, c.ours, got)
			}
		})
	}
}

// A file that cannot be read as a whole, or whose top-level structure is not what the host reads,
// holds a removal back whoever wrote it: the reading cannot tell which entries it holds.
func TestRegisteredMatchingHoldsARemovalBackWhenAFileOrItsStructureCannotBeRead(t *testing.T) {
	for _, c := range []struct{ name, file, text string }{
		{"a config.toml that is not TOML", "config.toml", "[mcp_servers.x\ncommand = \"python3\"\n"},
		{"mcp_servers that is not a table", "config.toml", "mcp_servers = 1\n"},
		{"a hooks.json that is not JSON", "hooks.json", `{"hooks": `},
		{"a hooks.json whose root is not an object", "hooks.json", `[]`},
		{"hooks that is not an object", "hooks.json", `{"hooks": ["x"]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMalformedFixture(t)
			path := filepath.Join(f.h.codex, c.file)
			write(t, path, c.text, 0o600)
			for _, foreign := range []bool{false, true} {
				if _, got := f.h.registrationsAs(t, f.dir, foreign); !listed(got, path) {
					t.Fatalf("Foreign %v: the file is not listed unreadable: %v", foreign, got)
				}
			}
		})
	}
}

// The reading follows a malformed entry's directory into the runtime layout (a cwd that holds
// bin/crw), and must not open anything there that is not a regular file: a FIFO where the binary
// belongs would block it forever.
func TestRegisteredMatchingDoesNotBlockOnAFifoWhereARuntimeBinaryBelongs(t *testing.T) {
	f := newMalformedFixture(t)
	fifos := filepath.Join(f.h.home, "fifos")
	mkdir(t, filepath.Join(fifos, "bin"))
	if err := syscall.Mkfifo(filepath.Join(fifos, "bin", "crw"), 0o600); err != nil {
		t.Skip("cannot make a FIFO here: ", err)
	}
	write(t, filepath.Join(f.h.codex, "config.toml"), "[mcp_servers.x]\ncommand = \"python3\"\nargs = 42\ncwd = \""+fifos+"\"\n", 0o600)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.registrationsAs(t, f.dir, false)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the reading blocked opening a FIFO at bin/crw")
	}
}
