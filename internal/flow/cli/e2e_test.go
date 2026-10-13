package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestStartRunsARealDetachedProcess: `crw flow start` of the built binary starts a real `crw flow _run` process in a session of its
// own, which opens the relay store and the App Server (the fake host), holds the run lock, runs the script, writes its journal and
// leaves; `ps`, `show` and `watch` of the same binary then read the run from the store, and the process wrote its log under the run's
// directory. Nothing else is shared between the command and the process but the state directory.
func TestStartRunsARealDetachedProcess(t *testing.T) {
	w := newWorld(t)
	w.close() // the process opens the store for writing
	host := fakehost.Start(t)
	// a flow binding names a thread only by an id of a real host's form
	host.Threads().ThreadIDs(func(n int) string { return fmt.Sprintf("0190a000-0000-7000-8000-%012x", n) })
	repo := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	script := filepath.Join(t.TempDir(), "hello.js")
	if err := os.WriteFile(script, []byte("export const meta = { name: 'hello', phases: ['one'] }\nphase('one')\nconst a = await agent.call('hi', { agentType: 'explorer', label: 'hi' })\nreturn a.status\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	crw := testsupport.CRW(t)
	common := []string{"--state", w.state, "--socket", host.SocketPath}
	run := func(args ...string) (int, string, string) {
		cmd := exec.Command(crw, append([]string{"flow"}, args...)...)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		return code, out.String(), errOut.String()
	}
	// the store has to exist for the process to open it, with the relay's tables: a writable open creates them
	st := w
	st.open()
	st.close()

	code, out, errOut := run(append([]string{"start", script, "--thread", "thread-owner", "--repo", repo}, common...)...)
	if code != 0 {
		t.Fatalf("start: exit %d\n%s\n%s", code, out, errOut)
	}
	var id string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "run run-") {
			id = strings.Fields(line)[1]
		}
	}
	if id == "" {
		t.Fatalf("no run id in the answer:\n%s", out)
	}
	deadline := time.Now().Add(60 * time.Second)
	var show string
	for time.Now().Before(deadline) {
		_, show, _ = run(append([]string{"show", id}, common...)...)
		if strings.Contains(show, id+"  hello  completed") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(show, id+"  hello  completed") || !strings.Contains(show, "phase one") || !strings.Contains(show, "explorer") {
		log, _ := os.ReadFile(filepath.Join(RunDir(w.state, id), "process.log"))
		t.Fatalf("the run did not complete:\n%s\nlog:\n%s", show, log)
	}
	if LockHeld(w.state, id) {
		t.Error("the run lock is held after the process ended")
	}
	if res, ok := ReadResult(w.state, id); !ok || res.Status != "completed" || string(compactJSON(res.Value)) != `"completed"` {
		t.Errorf("the result: %+v %v", res, ok)
	}
	spec, err := ReadSpec(w.state, id)
	if err != nil || spec.Thread != "thread-owner" || spec.Script != script || spec.Socket != host.SocketPath || factNames(spec.Preflight) != "assignment_routed,delivery_ticking,thread_reachable,idle_wake_observed" {
		t.Errorf("the spec: %+v %v", spec, err)
	}
	if _, ps, _ := run(append([]string{"ps", "--all"}, common...)...); !strings.Contains(ps, id) || !strings.Contains(ps, "completed") {
		t.Errorf("ps:\n%s", ps)
	}
	if _, ps, _ := run(append([]string{"ps"}, common...)...); strings.Contains(ps, id) {
		t.Errorf("ps lists a run that is over:\n%s", ps)
	}
	if _, line, _ := run(append([]string{"status-line", "--thread", "thread-owner"}, common...)...); line != "" {
		t.Errorf("a status line for a run that is over: %q", line)
	}
	code, wout, _ := run(append([]string{"watch", id, "--after", "0", "--json", "--limit", "1000"}, common...)...)
	if code != 0 || !strings.Contains(wout, `"event":"finished"`) {
		t.Errorf("watch: %d %s", code, wout)
	}
}
