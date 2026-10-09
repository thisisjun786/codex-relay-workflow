//go:build dev

package laneparity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandExecutable(t *testing.T) {
	const crw = "/opt/build/crw"
	for _, c := range []struct {
		command string
		want    string
		ok      bool
	}{
		{`"/opt/build/crw" hook session-start --leg x`, "/opt/build/crw", true},
		{`/usr/bin/true hook session-start --leg x`, "/usr/bin/true", true},
		{`crw hook session-start --leg x`, crw, true},
		{`"$CRW_BIN" hook session-start --leg x`, crw, true},
		{`${CRW_BIN} hook stop --leg x`, crw, true},
		{`"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0`, "", false},
		{`exit 0`, "", false},
		{`./crw hook x --leg y`, "", false},
		{`true`, "", false},
		{``, "", false},
	} {
		got, ok := CommandExecutable(c.command, crw)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q %v, want %q %v", c.command, got, ok, c.want, c.ok)
		}
	}
}

// A report whose --crw is not the executable the declared commands start must not pass: the
// receipts name the build the commands started, not the file the flag named.
func TestFire_aDeclaredCommandThatStartsAnotherBuildFails(t *testing.T) {
	o := fireFixture(t)
	other, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true on PATH")
	}
	o.CRW = other
	o.Only = regexpOf(`^hook__session-start-announcing-map-affordance__`)
	rep, err := Fire(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatalf("a plugin root that starts another build passed: %+v", rep.ReceiptProblems)
	}
	if !strings.Contains(strings.Join(rep.ReceiptProblems, "\n"), "another crw build") {
		t.Errorf("receipt problems %q", rep.ReceiptProblems)
	}
}

func TestFire_receiptsNameTheBuildTheCommandStarted(t *testing.T) {
	o := fireFixture(t)
	o.Only = regexpOf(`^hook__session-start-announcing-map-affordance__`)
	rep, err := Fire(o)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := FileDigest(o.CRW)
	if err != nil || len(rep.Receipts) == 0 {
		t.Fatalf("%v, %d receipts", err, len(rep.Receipts))
	}
	for _, r := range rep.Receipts {
		if r.Binary != digest {
			t.Errorf("receipt binary %s, the started build is %s", r.Binary, digest)
		}
	}
	// the same build reached through a second path is the same build
	link := filepath.Join(t.TempDir(), "crw-link")
	if err := os.Symlink(o.CRW, link); err != nil {
		t.Fatal(err)
	}
	if d, err := FileDigest(link); err != nil || d != digest {
		t.Errorf("link digest %s %v", d, err)
	}
}
