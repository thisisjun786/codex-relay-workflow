package adapter

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
)

// daemon runs the relay's control socket in this process over the CLI's state, with root as the marker root
// the daemon resolves for itself (its environment). The CLI under test is a separate process whose own
// environment is set by ownerMarker, so the two can differ as a parent's and a service's do (CRW-1309, d1).
func (c *managedCLI) daemon(root string) {
	c.t.Helper()
	c.t.Setenv(delivery.MarkerEnv, root)
	control, err := service.ListenControl(context.Background(), c.state)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = control.Close() })
}

func (c *managedCLI) refusedAtStart(stdout, stderr string, code int) {
	c.t.Helper()
	if code != 2 || stderr != "" || !strings.Contains(stdout, `"reason": "marker_root_mismatch"`) {
		c.t.Fatalf("exit %d stdout %q stderr %q, want the marker_root_mismatch refusal", code, stdout, stderr)
	}
	if n := c.host.Count("thread/start"); n != 0 {
		c.t.Fatalf("the refused request asked the app-server to start %d threads", n)
	}
	if n := c.host.Count("turn/start"); n != 0 {
		c.t.Fatalf("the refused request dispatched %d turns", n)
	}
}

// CRW-1309 (d1): the root a new start is judged against is the one the relay daemon answers over control.sock,
// not the one managed-start's own environment resolves. A service environment that names the root and a parent
// that does not export it: the correct --marker-root starts (it was refused, naming the parent's own default).
func TestManagedStartAdmitsTheDaemonsRootWhenTheCallersEnvironmentDiffers(t *testing.T) {
	cli := newManagedCLI(t)
	daemonRoot := filepath.Join(filepath.Dir(cli.marker), "rx", "crw-completion-hook", "markers")
	cli.daemon(daemonRoot)
	cli.ownerMarker = "-"
	cli.markerFlag = daemonRoot
	stdout, stderr, code := cli.start(cli.request(nil))
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"state": "admitted"`) {
		t.Fatalf("exit %d stdout %q stderr %q, want the daemon's own root admitted", code, stdout, stderr)
	}
}

// The same split the other way: the parent exports CODEX_SESSION_RELAY_MARKER_ROOT equal to its per-case
// --marker-root (as a harness does) while the daemon judges another root. The start is refused, with the
// daemon's root and the one named in the reason, and nothing is created or dispatched.
func TestManagedStartRefusesARootTheDaemonDoesNotJudgeEvenWhenTheCallersEnvironmentNamesIt(t *testing.T) {
	cli := newManagedCLI(t)
	daemonRoot := filepath.Join(filepath.Dir(cli.marker), "rx", "crw-completion-hook", "markers")
	cli.daemon(daemonRoot)
	cli.ownerMarker = cli.marker // the parent's environment agrees with its --marker-root
	stdout, stderr, code := cli.start(cli.request(nil))
	cli.refusedAtStart(stdout, stderr, code)
	if !strings.Contains(stdout, cli.marker) || !strings.Contains(stdout, daemonRoot) {
		t.Fatalf("the refusal names neither the given root %s nor the daemon's %s: %s", cli.marker, daemonRoot, stdout)
	}
	if !strings.Contains(stdout, "the relay daemon") {
		t.Fatalf("the refusal does not say the daemon named its root: %s", stdout)
	}
}

// A parent that follows an earlier refusal's wording must be told the daemon's root, not its own default: the
// root named for the correction is the one the daemon answered, so the second start is admitted.
func TestManagedStartRefusalNamesTheDaemonsRootAndThatRootStarts(t *testing.T) {
	cli := newManagedCLI(t)
	daemonRoot := filepath.Join(filepath.Dir(cli.marker), "rx", "markers")
	cli.daemon(daemonRoot)
	cli.ownerMarker = "-"
	stdout, stderr, code := cli.start(cli.request(nil))
	cli.refusedAtStart(stdout, stderr, code)
	if !strings.Contains(stdout, daemonRoot) {
		t.Fatalf("the refusal does not name the daemon's root %s: %s", daemonRoot, stdout)
	}
	cli.markerFlag = daemonRoot
	stdout, stderr, code = cli.start(cli.request(nil))
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"state": "admitted"`) {
		t.Fatalf("exit %d stdout %q stderr %q, want the root the refusal named admitted", code, stdout, stderr)
	}
}

// A replay of a started request is not judged by the check, whatever the daemon answers afterwards.
func TestManagedStartReplayIsNotRefusedWhenTheDaemonsRootDiffers(t *testing.T) {
	cli := newManagedCLI(t)
	daemonRoot := filepath.Join(filepath.Dir(cli.marker), "rx", "markers")
	cli.daemon(daemonRoot)
	cli.ownerMarker = "-"
	cli.markerFlag = daemonRoot
	raw := cli.request(nil)
	if stdout, stderr, code := cli.start(raw); code != 0 || !strings.Contains(stdout, `"state": "admitted"`) {
		t.Fatalf("first start: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The daemon's environment changes; the same request id and the same bytes are replayed.
	other := filepath.Join(filepath.Dir(cli.marker), "rx2", "markers")
	t.Setenv(delivery.MarkerEnv, other)
	stdout, stderr, code := cli.start(raw)
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"state": "admitted"`) || strings.Contains(stdout, "marker_root_mismatch") {
		t.Fatalf("the replay was judged by the root check: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// With no daemon to ask, the root managed-start's own environment resolves is the best answer there is, and the
// refusal says so: it names that root as this process's, and that the daemon could not be asked and why.
func TestManagedStartWithoutADaemonFallsBackToItsOwnEnvironmentAndSaysSo(t *testing.T) {
	cli := newManagedCLI(t)
	cli.ownerMarker = filepath.Join(filepath.Dir(cli.marker), "owner", "markers")
	stdout, stderr, code := cli.start(cli.request(nil))
	cli.refusedAtStart(stdout, stderr, code)
	for _, want := range []string{cli.marker, cli.ownerMarker, "could not be asked", "control.sock"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the refusal lacks %q: %s", want, stdout)
		}
	}
}

// A daemon that does not know the question (an older one answers requestRejected) is the same case.
func TestManagedStartWithADaemonThatRejectsTheQuestionFallsBackAndSaysSo(t *testing.T) {
	cli := newManagedCLI(t)
	address, release, err := hook.ControlAddress(filepath.Join(cli.state, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	release()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte("{\"protocol\":1,\"requestRejected\":true}\n"))
			_ = conn.Close()
		}
	}()
	cli.ownerMarker = filepath.Join(filepath.Dir(cli.marker), "owner", "markers")
	stdout, stderr, code := cli.start(cli.request(nil))
	cli.refusedAtStart(stdout, stderr, code)
	for _, want := range []string{cli.marker, cli.ownerMarker, "could not be asked"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the refusal lacks %q: %s", want, stdout)
		}
	}
}

// A relative --marker-root is refused by the absolute-path rule of the request identity, before the root check
// and before anything is created; the reason names the path and the rule (d3).
func TestManagedStartRefusesARelativeMarkerRootWithItsReason(t *testing.T) {
	cli := newManagedCLI(t)
	cli.markerFlag = "markers"
	stdout, stderr, code := cli.start(cli.request(nil))
	if code == 0 || !strings.Contains(stdout+stderr, "absolute") {
		t.Fatalf("exit %d stdout %q stderr %q, want a refusal that says the root must be absolute", code, stdout, stderr)
	}
	if n := cli.host.Count("thread/start"); n != 0 {
		t.Fatalf("the refused request asked the app-server to start %d threads", n)
	}
	if _, err := os.Stat(filepath.Join(cli.state, "control.sock")); err == nil {
		t.Fatal("no daemon was expected in this test")
	}
}
