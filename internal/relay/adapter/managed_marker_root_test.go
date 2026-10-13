package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1309: the built relay CLI refuses a managed-start whose --marker-root is not the marker root the
// relay itself resolves (the root control.sock judges Stops under), at the start and naming both paths: the
// app-server is asked for no thread, and a root that names the relay's own root (here through a symlink)
// starts as before.
func TestManagedStartRefusesAMarkerRootTheDaemonDoesNotJudgeUnder(t *testing.T) {
	cli := newManagedCLI(t)
	cli.ownerMarker = filepath.Join(filepath.Dir(cli.marker), "owner", "markers")
	stdout, stderr, code := cli.start(cli.request(nil))
	if code != 2 || stderr != "" || !strings.Contains(stdout, `"reason": "marker_root_mismatch"`) {
		t.Fatalf("exit %d stdout %q stderr %q, want the marker_root_mismatch refusal", code, stdout, stderr)
	}
	if !strings.Contains(stdout, cli.marker) || !strings.Contains(stdout, cli.ownerMarker) {
		t.Fatalf("the refusal names neither the given root %s nor the relay's %s: %s", cli.marker, cli.ownerMarker, stdout)
	}
	if n := cli.host.Count("thread/start"); n != 0 {
		t.Fatalf("the refused request asked the app-server to start %d threads", n)
	}
	if n := cli.host.Count("turn/start"); n != 0 {
		t.Fatalf("the refused request dispatched %d turns", n)
	}
	// The relay's root through a symlink is the same root.
	if err := os.MkdirAll(cli.ownerMarker, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(cli.marker), "markers-link")
	if err := os.Symlink(cli.ownerMarker, link); err != nil {
		t.Fatal(err)
	}
	cli.marker = link
	stdout, stderr, code = cli.start(cli.request(nil))
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"state": "admitted"`) {
		t.Fatalf("exit %d stdout %q stderr %q, want the start admitted under the relay's own root", code, stdout, stderr)
	}
}
