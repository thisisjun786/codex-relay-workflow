package testsupport

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"sync"
	"testing"
)

// ChildOwner stands in for the relay daemon of state while a fixture's child command (emit, intent-claim,
// intent-disposition) reports: the command reaches the daemon over <state>/control.sock and runs there, on that state
// (CRW-1286, CRW-1289). The stand-in is a crw built with -tags childownerprobe, started in dir with env, the child's
// own, so the command answers what the built CLI answered when the child ran it itself. The returned func stops it;
// so does the end of the test. A state that is not a directory the stand-in can read has no daemon, as on a host: then
// nothing starts and the child finds no daemon.
func ChildOwner(t testing.TB, state, dir string, env []string) func() {
	t.Helper()
	if _, err := os.ReadDir(state); err != nil {
		return func() {}
	}
	command := exec.Command(BuildCRW(t, "-tags", "childownerprobe"), "child-owner-probe", state)
	command.Dir, command.Env = dir, env
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = stdin.Close(); _ = command.Wait() }) }
	t.Cleanup(stop)
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		stop()
		t.Fatalf("the relay daemon stand-in for %s did not start: %q %s", state, line, stderr.String())
	}
	return stop
}
