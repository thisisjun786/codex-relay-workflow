//go:build linux

package service

import (
	"bytes"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// controlAcceptWithin is how long a daemon subtest waits for its daemon to accept on control.sock.
const controlAcceptWithin = 10 * time.Second

// controlDaemonDeadline is the --deadline of the daemon the subtests start. The daemon counts it from
// clock.Now() in runDaemon, before it takes its locks, builds its adapter and listens on control.sock, and these
// subtests wait for it to be spent: a stop request is read only between ticks, one poll interval (20 s) apart,
// so it would end the segment later than the deadline does, and a SIGINT while the daemon waits for the next
// tick makes it exit 3. A deadline of 2 s was spent by a runner that needed longer than that to start, and the
// daemon then exited bound_already_spent without serving anything. Two seconds past the wait for the socket, a
// daemon too slow to start fails that wait, whose message carries the daemon's own output, and not the bound.
// Each subtest lasts this long.
const controlDaemonDeadline = controlAcceptWithin + 2*time.Second

// controlDaemon is a daemon segment in an isolated home, started by startControlDaemon.
type controlDaemon struct {
	t              *testing.T
	cmd            *exec.Cmd
	path           string
	started        time.Time
	accepted       time.Duration
	stdout, stderr bytes.Buffer
	waited         bool
}

// startControlDaemon starts a daemon segment in home and returns once control.sock accepts. The test then
// probes the socket and calls Wait; a test that ends before it has the daemon killed, and shown if the test failed.
func startControlDaemon(t *testing.T, home string) *controlDaemon {
	t.Helper()
	d := &controlDaemon{t: t, path: controlPath(home + "/state")}
	d.cmd = exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "daemon", "--deadline", strconv.Itoa(int(controlDaemonDeadline/time.Second)), "--allow-isolated-scope")
	d.cmd.Env = environment(home)
	d.cmd.Stdout, d.cmd.Stderr = &d.stdout, &d.stderr
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d.started = time.Now()
	t.Cleanup(func() {
		if !d.waited {
			d.waited = true
			_ = d.cmd.Process.Kill()
			_ = d.cmd.Wait()
			if t.Failed() {
				t.Logf("the daemon, killed after the test failed, had accepted after %v and written: %s %s", d.accepted, d.stdout.String(), d.stderr.String())
			}
		}
	})
	if err := awaitControlAccepting(d.path, controlAcceptWithin); err != nil {
		d.waited = true
		_ = d.cmd.Process.Kill()
		_ = d.cmd.Wait()
		t.Fatalf("the daemon never accepted on control.sock: %v: %s %s", err, d.stdout.String(), d.stderr.String())
	}
	d.accepted = time.Since(d.started)
	return d
}

// Wait is the daemon's end by its deadline: nil when it exited 0. It logs how much of the deadline the start
// and the probes used.
func (d *controlDaemon) Wait() error {
	if !d.waited {
		d.t.Logf("daemon accepted %v and was probed %v after it started; its deadline is %v", d.accepted.Round(time.Millisecond), time.Since(d.started).Round(time.Millisecond), controlDaemonDeadline)
	}
	d.waited = true
	return d.cmd.Wait()
}
