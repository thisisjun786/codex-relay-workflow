//go:build dev

package laneparity

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The recorder installed as the runtime keeps every start (arguments, payload, answer, status, time)
// and hands the start to the build under test unchanged; the faults change only what it does with it.
func TestHostShim_recordsAStartAndHandsItToTheBuild(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "crw")
	// a build that answers with its payload and fails
	script := "#!/bin/sh\nprintf 'ran %s: ' \"$*\" > \"" + dir + "/ran\"\ncat >> \"" + dir + "/ran\"\nprintf 'answer\\n'\nexit 3\n"
	if err := os.WriteFile(build, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		fault     string
		runs      bool
		stdout    string
		rc        int
		recordedA string
	}{
		{"", true, "answer\n", 3, "answer\n"},
		{FaultNoop, false, "", 0, ""},
		{FaultDropStdout, true, "", 3, "answer\n"},
	} {
		rec := filepath.Join(dir, "rec-"+c.fault+"x")
		if err := os.Mkdir(rec, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(dir, "ran"))
		shim := filepath.Join(dir, "shim-"+c.fault+"x")
		if err := os.WriteFile(shim, []byte(hostShim(build, rec, c.fault)), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(shim, "hook", "session-start", "--leg", "a b")
		cmd.Stdin = strings.NewReader(`{"session_id":"s"}`)
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if string(out) != c.stdout || code != c.rc {
			t.Errorf("fault %q: the host got %q exit %d, want %q exit %d", c.fault, out, code, c.stdout, c.rc)
		}
		_, ranErr := os.Stat(filepath.Join(dir, "ran"))
		if (ranErr == nil) != c.runs {
			t.Errorf("fault %q: the build ran %v, want %v", c.fault, ranErr == nil, c.runs)
		}
		starts, _ := filepath.Glob(filepath.Join(rec, "r.*"))
		if len(starts) != 1 {
			t.Fatalf("fault %q: %d start records", c.fault, len(starts))
		}
		read := func(name string) string {
			raw, err := os.ReadFile(filepath.Join(starts[0], name))
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
		if read("argv") != "hook\nsession-start\n--leg\na b\n" || read("in") != `{"session_id":"s"}` || read("out") != c.recordedA ||
			strings.TrimSpace(read("rc")) != map[bool]string{true: "3", false: "0"}[c.runs] || strings.TrimSpace(read("at")) == "" {
			t.Errorf("fault %q: record argv %q in %q out %q rc %q", c.fault, read("argv"), read("in"), read("out"), read("rc"))
		}
	}
}

func TestLegOfArgv(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"hook", "session-start", "--leg", "x"}, "x"},
		{[]string{"hook", "stop", "--leg=y"}, "y"},
		{[]string{"hook", "--plugin-launch"}, CompletionLeg},
		{[]string{"hook", "stop"}, "?"},
		{[]string{"--leg"}, "?"},
	} {
		if got := legOfArgv(c.argv); got != c.want {
			t.Errorf("%v: %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestAnswerText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"  \n", ""},
		{`{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"  hello "}}`, "hello"},
		{`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`, ""},
		{`{"systemMessage":"x"}`, ""},
		{"plain context\n", "plain context"},
	} {
		if got := answerText(c.in); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatcherMatches(t *testing.T) {
	for _, c := range []struct {
		matcher, name string
		want          bool
	}{
		{"", "Bash", true}, {"*", "anything", true}, {".*", "worker", true},
		{"^Bash$", "Bash", true}, {"^Bash$", "exec_command", false},
		{"^(executor|worker)$", "worker", true}, {"^(executor|worker)$", "reviewer", false},
		{"^(collaboration[._]?)?spawn_agent$", "spawn_agent", true}, {"^create_goal$", "", false},
		{"(", "x", false},
	} {
		if got := matcherMatches(c.matcher, c.name); got != c.want {
			t.Errorf("%q against %q: %v, want %v", c.matcher, c.name, got, c.want)
		}
	}
}

func TestParseHostStream(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"i","type":"command_execution","command":"zsh -lc 'echo x'","status":"completed"}}
{"type":"item.completed","item":{"id":"j","type":"agent_message","text":"done"}}
not json
{"type":"turn.completed","usage":{}}
`
	thread, items := parseHostStream(out)
	if thread != "t1" || len(items) != 3 || items[0].Type != "command_execution" || items[0].Status != "completed" || items[1].Text != "done" || items[2].Type != "turn.completed" {
		t.Errorf("%q %+v", thread, items)
	}
}

// The environment of a host process holds nothing of this process's: its own HOME and CODEX_HOME, a
// PATH of system programs, and a proxy that refuses every connection but the loopback provider.
func TestHostEnviron_isOnlyTheIsolatedHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/real/codex")
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	env := hostEnviron("/h", "/c", "/t", "/bin/codex")
	joined := strings.Join(env, "\n")
	for _, want := range []string{"HOME=/h", "CODEX_HOME=/c", "TMPDIR=/t", "PATH=/usr/bin:/bin", "NO_PROXY=127.0.0.1,localhost", "HTTPS_PROXY=http://127.0.0.1:9"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %s in %v", want, env)
		}
	}
	if strings.Contains(joined, "/real/codex") || strings.Contains(joined, "must-not-leak") || strings.Contains(joined, "OPENAI") {
		t.Errorf("the environment carries this process's: %v", env)
	}
}

// No run uses a flag that skips trust, approvals or the sandbox, and the report says so.
func TestHostArgs_skipNothing(t *testing.T) {
	args := hostArgs("/work")
	for _, a := range args {
		if strings.Contains(a, "bypass") || strings.Contains(a, "dangerous") || a == "-c" || a == "--config" || a == "--full-auto" || a == "--sandbox" {
			t.Errorf("flag %q", a)
		}
	}
	if args[0] != "exec" || !slices.Contains(args, "--json") {
		t.Errorf("%v", args)
	}
}

// What is due is every registration whose event the turn has and whose matcher takes the subject; the
// completion Stop counts as a leg of its own, and a role is not part of the key.
func TestDueHooks_isTheDeclarationsTheEventsMatch(t *testing.T) {
	_, registered, err := ReadRegistered(filepath.Join(repoRoot(t), "plugins", "crw"))
	if err != nil {
		t.Fatal(err)
	}
	due := dueHooks(registered, turnEvents)
	total := 0
	for _, n := range due {
		total += n
	}
	// nine SessionStart, five UserPromptSubmit, three PreToolUse for the shell, one PostToolUse for the viewer, three Stop
	if total != 21 || due[hookKey("pre-tool-use-guarding-github-post", "PreToolUse", "Bash")] != 1 ||
		due[hookKey("post-tool-use-tracking-render-observations", "PostToolUse", "view_image")] != 1 ||
		due[hookKey(CompletionLeg, "Stop", "")] != 1 || due[hookKey("pre-tool-use-attaching-skills", "PreToolUse", "Bash")] != 0 {
		t.Errorf("%d due: %v", total, due)
	}
	spawned := dueHooks(registered, []hostEvent{{"SubagentStop", "worker"}})
	if len(spawned) != 2 || spawned[hookKey("subagent-stop-verifying-evidence", "SubagentStop", "worker")] != 1 || spawned[hookKey("subagent-stop-observing-review", "SubagentStop", "")] != 1 {
		t.Errorf("%v", spawned)
	}
	if other := dueHooks(registered, []hostEvent{{"SubagentStop", "reviewer"}}); len(other) != 1 {
		t.Errorf("a reviewer is not a worker: %v", other)
	}
}

// firingsDue is a complete set of starts for a turn: what the judgement takes for a host that did
// everything right, for the mutations below to spoil one thing each.
func firingsDue(t *testing.T, registered []Registered, events []hostEvent, thread string) []HostFiring {
	t.Helper()
	var out []HostFiring
	for _, ev := range events {
		for _, reg := range registered {
			if reg.Event == ev.Event && matcherMatches(reg.Matcher, ev.Tool) {
				leg := reg.Leg
				if leg == "" {
					leg = CompletionLeg
				}
				f := HostFiring{Leg: leg, Event: ev.Event, Session: thread, StdoutSHA: StdoutDigest("")}
				if ev.Event == "PreToolUse" || ev.Event == "PostToolUse" {
					f.ToolName, f.ToolUse = ev.Tool, "call-"+ev.Tool
				}
				if ev.Event != "SessionStart" {
					f.Turn = "turn-1"
				}
				out = append(out, f)
			}
		}
	}
	return out
}

func judgeFixture(t *testing.T) (*hostEnv, hostCellSpec) {
	t.Helper()
	_, registered, err := ReadRegistered(filepath.Join(repoRoot(t), "plugins", "crw"))
	if err != nil {
		t.Fatal(err)
	}
	return &hostEnv{registered: registered}, hostCellSpec{name: CellTurnOff, state: SwitchOff, trusted: true, events: turnEvents}
}

func judged(t *testing.T, mutate func(*HostCell)) []string {
	t.Helper()
	h, spec := judgeFixture(t)
	cell := HostCell{Name: spec.name, Switch: spec.state, Trusted: true, Thread: "thread-1", Events: map[string]int{}}
	cell.Firings = firingsDue(t, h.registered, spec.events, "thread-1")
	r := &hostRun{items: []hostItem{{Type: "command_execution", Command: "zsh -lc 'echo crw-real-host'", Status: "completed"}, {Type: "agent_message", Text: "crw real-host turn done"}, {Type: "turn.completed"}}}
	if mutate != nil {
		mutate(&cell)
	}
	h.judge(&cell, r, spec)
	return cell.Problems
}

// The judgement takes a complete run of the right hooks for the thread and finds nothing; each way a
// host's run can differ is a problem.
func TestJudge_hooksTheHostStartedAgainstTheDeclaredOnes(t *testing.T) {
	if p := judged(t, nil); len(p) != 0 {
		t.Fatalf("a complete run has problems: %q", p)
	}
	for name, c := range map[string]struct {
		mutate func(*HostCell)
		want   string
	}{
		"a hook not started":   {func(c *HostCell) { c.Firings = c.Firings[1:] }, "started 0 time(s), want 1"},
		"a hook started twice": {func(c *HostCell) { c.Firings = append(c.Firings, c.Firings[0]) }, "started 2 time(s), want 1"},
		"a hook not declared for the tool": {func(c *HostCell) {
			c.Firings = append(c.Firings, HostFiring{Leg: "pre-tool-use-attaching-skills", Event: "PreToolUse", ToolName: "Bash", ToolUse: "x", Session: "thread-1", Turn: "turn-1"})
		}, "no declared hook is due"},
		"another session": {func(c *HostCell) { c.Firings[2].Session = "thread-2" }, "was given session"},
		"a failing hook":  {func(c *HostCell) { c.Firings[3].Exit = 1 }, "exited 1"},
		"another turn":    {func(c *HostCell) { c.Firings[len(c.Firings)-1].Turn = "turn-2" }, "an earlier hook of the turn got"},
		"no turn id":      {func(c *HostCell) { c.Firings[len(c.Firings)-1].Turn = "" }, "was given no turn id"},
		"no tool call id": {func(c *HostCell) {
			for i := range c.Firings {
				if c.Firings[i].Event == "PreToolUse" {
					c.Firings[i].ToolUse = ""
				}
			}
		}, "no tool call id"},
		"a host that failed": {func(c *HostCell) { c.Exit = 1 }, "codex exec exited 1"},
		"no thread":          {func(c *HostCell) { c.Thread = "" }, "the host reported no thread"},
	} {
		got := judged(t, c.mutate)
		if !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s: problems %q, want one with %q", name, got, c.want)
		}
	}
	// a plugin with no trust runs no hook
	h, spec := judgeFixture(t)
	spec.trusted = false
	cell := HostCell{Thread: "t", Firings: []HostFiring{{Leg: "x", Event: "Stop"}}}
	h.judge(&cell, &hostRun{}, spec)
	if len(cell.Problems) == 0 || !strings.Contains(strings.Join(cell.Problems, "\n"), "no trust recorded") {
		t.Errorf("problems %q", cell.Problems)
	}
}

func TestCheckSilent_aSilentSwitchIsNotAnsweredOrRecorded(t *testing.T) {
	run := func(mutate func(*HostCell, *hostRun)) []string {
		cell := HostCell{Switch: SwitchCXC, Firings: []HostFiring{{Leg: "session-start-injecting-recall-context", Event: "SessionStart"}, {Leg: CompletionLeg, Event: "Stop", Answered: true}}}
		r := &hostRun{requests: []StubRequest{{Texts: []string{"say hi"}}},
			items: []hostItem{{Type: "command_execution", Command: "echo crw-real-host", Status: "completed"}, {Type: "agent_message", Text: "turn done"}, {Type: "turn.completed"}}}
		if mutate != nil {
			mutate(&cell, r)
		}
		checkSilent(&cell, r)
		return cell.Problems
	}
	if p := run(nil); len(p) != 0 {
		t.Fatalf("silent run: %q (the completion Stop is not behind the switch)", p)
	}
	for name, c := range map[string]struct {
		mutate func(*HostCell, *hostRun)
		want   string
	}{
		"an answer":     {func(c *HostCell, _ *hostRun) { c.Firings[0].Answered = true }, "answered with the switch at cxc"},
		"a record":      {func(c *HostCell, _ *hostRun) { c.Records = 2 }, "invocation record(s) were written"},
		"hook text":     {func(_ *HostCell, r *hostRun) { r.requests[0].Texts = append(r.requests[0].Texts, "[crw-recall] hello") }, "carries hook text"},
		"an unfinished": {func(_ *HostCell, r *hostRun) { r.items = nil }, "did not run to its end"},
	} {
		if got := run(c.mutate); !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s: problems %q, want one with %q", name, got, c.want)
		}
	}
}

func TestCheckOn_theAnswersMustReachTheModel(t *testing.T) {
	run := func(mutate func(*HostCell)) []string {
		cell := HostCell{Switch: SwitchOn, Records: 3, Answered: 1, Firings: []HostFiring{{Leg: "session-start-injecting-recall-context", Event: "SessionStart", Answered: true, Context: true, Reached: true}}}
		r := &hostRun{items: []hostItem{{Type: "command_execution", Command: "echo crw-real-host", Status: "completed"}, {Type: "agent_message", Text: "turn done"}, {Type: "turn.completed"}}}
		if mutate != nil {
			mutate(&cell)
		}
		checkOn(&cell, r)
		return cell.Problems
	}
	if p := run(nil); len(p) != 0 {
		t.Fatalf("%q", p)
	}
	for name, c := range map[string]struct {
		mutate func(*HostCell)
		want   string
	}{
		"nothing answered": {func(c *HostCell) { c.Answered, c.Firings[0].Answered, c.Firings[0].Context = 0, false, false }, "no hook answered"},
		"not in a request": {func(c *HostCell) { c.Firings[0].Reached = false }, "is in no model request"},
		"no record":        {func(c *HostCell) { c.Records = 0 }, "no hook invocation record"},
	} {
		if got := run(c.mutate); !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s: problems %q, want one with %q", name, got, c.want)
		}
	}
}

func TestCheckPermission_saysWhatWasMeasured(t *testing.T) {
	cell := HostCell{}
	checkPermission(&cell, &hostRun{requests: []StubRequest{{ToolOutputs: []string{"approval policy is Never; reject command"}}}})
	if cell.Unverified == "" || len(cell.Problems) != 0 || !strings.Contains(cell.Unverified, "approval policy is Never") {
		t.Errorf("%+v", cell)
	}
	cell = HostCell{}
	checkPermission(&cell, &hostRun{requests: []StubRequest{{ToolOutputs: []string{"hi"}}}})
	if cell.Unverified != "" || len(cell.Problems) != 1 {
		t.Errorf("an escalation that was neither refused nor asked for must fail the cell: %+v", cell)
	}
	cell = HostCell{Firings: []HostFiring{{Leg: "permission-request-allowing-agent-thread", Event: "PermissionRequest"}}}
	checkPermission(&cell, &hostRun{})
	if cell.Unverified != "" || len(cell.Problems) != 0 {
		t.Errorf("a host that asked is verified: %+v", cell)
	}
}

// Without a Codex binary no cell runs and every one is reported not verified with the reason; the
// run is not a failure and not a pass of the cells.
func TestRealHost_withoutCodexEveryCellIsNotVerified(t *testing.T) {
	root := repoRoot(t)
	rep, err := RealHost(RealHostOptions{Root: root, CRW: os.Args[0], Plugin: filepath.Join(root, "plugins", "crw"), Codex: filepath.Join(t.TempDir(), "no-codex"), Scratch: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped == "" || len(rep.Cells) != 0 || !rep.OK || len(rep.Unverified) != len(hostCellNames) || rep.Bypass {
		t.Fatalf("%+v", rep)
	}
	for _, n := range rep.Unverified {
		if !strings.Contains(n.Reason, "no-codex") || !strings.HasPrefix(n.Cell, "real host: ") {
			t.Errorf("%+v", n)
		}
	}
	// the list a run reports keeps the cells the real host would have covered, and adds why not
	base := NotVerifiedCells()
	merged := notVerifiedWithRealHost(base, rep)
	if len(merged) != len(base)+len(hostCellNames) {
		t.Errorf("%d cells unverified, want %d", len(merged), len(base)+len(hostCellNames))
	}
	// a run that did reach the host drops the cells it measured and keeps the others
	ran := notVerifiedWithRealHost(base, RealHostReport{Unverified: []NotVerified{{Cell: "real host: permission/crw"}}})
	for _, n := range ran {
		for _, covered := range realHostCells {
			if strings.HasPrefix(n.Cell, covered) {
				t.Errorf("%q stays unverified after a real-host run", n.Cell)
			}
		}
	}
	if len(ran) != len(base)-len(realHostCells)+1 {
		t.Errorf("%d cells, want %d", len(ran), len(base)-len(realHostCells)+1)
	}
}

// A plugin root whose commands do not start the installed runtime cannot be driven through the
// recorder, and the run says so before it starts anything.
func TestRealHost_refusesARootThatDoesNotStartTheInstalledRuntime(t *testing.T) {
	root := repoRoot(t)
	legs, err := ExpectedLegs(root)
	if err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(t.TempDir(), "crw")
	if err := GeneratePluginRoot(generated, filepath.Join(root, "plugins", "crw"), "/opt/build/crw", legs); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = RealHost(RealHostOptions{Root: root, CRW: os.Args[0], Plugin: generated, Codex: codex, Scratch: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "as it ships") {
		t.Errorf("%v", err)
	}
	if _, err = RealHost(RealHostOptions{Root: root, CRW: os.Args[0], Plugin: filepath.Join(root, "plugins", "crw"), Codex: codex, Scratch: t.TempDir(), Fault: FaultKill}); err == nil || !strings.Contains(err.Error(), "cannot be injected") {
		t.Errorf("%v", err)
	}
}

func TestFindCodex(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, reason := findCodex(codex); got != codex || reason != "" {
		t.Errorf("%q %q", got, reason)
	}
	if got, reason := findCodex(dir); got != "" || reason == "" {
		t.Errorf("a directory is no executable: %q %q", got, reason)
	}
	t.Setenv("PATH", dir)
	if got, _ := findCodex(""); got != codex {
		t.Errorf("codex on PATH: %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got, reason := findCodex(""); got != "" || !strings.Contains(reason, "no codex executable on PATH") {
		t.Errorf("%q %q", got, reason)
	}
}
