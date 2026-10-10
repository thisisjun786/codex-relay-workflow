package hook

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

func Test33UnreachableErrnos(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{{syscall.ENOENT, "ENOENT"}, {syscall.ECONNREFUSED, "ECONNREFUSED"}, {syscall.EACCES, "EACCES"}, {context.DeadlineExceeded, "ETIMEDOUT"}} {
		record := unreachableRecord(Object{}, &net.OpError{Op: "dial", Net: "unix", Err: test.err}, 0)
		if record.Get("errno") != test.want || record.Get("processEnding") != "not_started" || record.Get("stdoutReading") != "said_nothing" {
			t.Fatal(record)
		}
	}
}
func Test33RefusedSocketJournal(t *testing.T) {
	home := hookHome(t, 5)
	path := filepath.Join(home, "state/control.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := hookCommand(t, home, `{}`)
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %s", err, out)
	}
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["errno"] != "ECONNREFUSED" || rows[0]["adapterOutcome"] != "guard_unreachable" || rows[0]["held"] != false {
		t.Fatal(rows)
	}
}
func Test33UnreachableNoJournalPolicy(t *testing.T) {
	home := hookHome(t, 5)
	path := filepath.Join(home, ConfigName)
	config, failure, _ := ReadSettings(context.Background(), path)
	if failure != "" {
		t.Fatal(failure)
	}
	config = config.Set("journalPolicy", "no_journal")
	writeTest(t, path, []byte(pyjson.Dumps(config, pyjson.Options{})))
	out, err := hookCommand(t, home, `{}`).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %s", err, out)
	}
	if len(rowsAt(t, home)) != 0 {
		t.Fatal("no_journal policy ignored")
	}
}
func Test33UnreachableJournalFailureReleases(t *testing.T) {
	home := hookHome(t, 5)
	writeTest(t, filepath.Join(home, "journal"), []byte("not a directory"))
	start := time.Now()
	out, err := hookCommand(t, home, `{}`).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %s", err, out)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("journal failure stalled hook")
	}
	_, err = os.Stat(filepath.Join(home, "journal/accepted"))
	if err == nil || !errors.Is(err, syscall.ENOTDIR) {
		t.Fatal(err)
	}
}
