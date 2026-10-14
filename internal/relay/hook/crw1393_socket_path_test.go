package hook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1393: a relay state directory whose control.sock path is longer than a unix socket address holds (the Stop adapter dials the
// path itself) is never a refusal: the hook releases the Stop as for any unreachable guard, and the journal row says, readably, that the
// path is too long and how long it is, beside the system's own "invalid argument".
func TestAControlSocketPathOverTheLimitIsAReadableUnreachableRow(t *testing.T) {
	home := hookHome(t, 5)
	state := filepath.Join(home, strings.Repeat("s", 120))
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ConfigName)
	config, failure, _ := ReadSettings(context.Background(), path)
	if failure != "" {
		t.Fatal(failure)
	}
	writeTest(t, path, []byte(pyjson.Dumps(config.Set("dbPath", filepath.Join(state, "relay.sqlite3")), pyjson.Options{})))
	socket := filepath.Join(state, "control.sock")
	out, err := hookCommand(t, home, `{}`).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("the hook refused or spoke: %v %s", err, out)
	}
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_unreachable" || rows[0]["held"] != false {
		t.Fatalf("rows %v; want one guard_unreachable row that holds nothing", rows)
	}
	detail, _ := rows[0]["detail"].(string)
	if len(socket) < 108 {
		t.Fatalf("the socket path is %d bytes; the test needs one over the limit", len(socket))
	}
	for _, want := range []string{"Invalid argument", "control socket path is longer than a unix socket address holds", "at most 103 bytes on darwin and 107 on Linux", "shorter directory"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
}
