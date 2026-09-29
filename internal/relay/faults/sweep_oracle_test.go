package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Compare the complete presence verdict and memo table with the live Python source
// on separately seeded disposable stores.
func Test22_OvertakenPresenceWholePythonPage(t *testing.T) {
	for _, total := range []int{1, presentChecks + 1} {
		t.Run(fmt.Sprint(total), func(t *testing.T) { testOvertakenPresenceWholePythonPage(t, total) })
	}
}

func testOvertakenPresenceWholePythonPage(t *testing.T, total int) {
	home := t.TempDir()
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	seed := func(path string) *store.Store {
		s, e := store.Open(context.Background(), filepath.Join(path, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		for i := 0; i < total; i++ {
			_, e = s.Q(context.Background()).ExecContext(context.Background(), "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,hold_reason,created_at,updated_at) VALUES(?,'rel','completion','parent','thread','withheld_pre_send','host_lost_turn','now','now')", fmt.Sprintf("evt%04d", i))
			if e != nil {
				t.Fatal(e)
			}
		}
		return s
	}
	goStore := seed(filepath.Join(home, "go"))
	defer goStore.Close()
	pyStore := seed(filepath.Join(home, "py"))
	pyStore.Close()
	// Go seeded Python's store as well; Python reads it after a takeover.
	testsupport.HandOver(t, filepath.Join(home, "py", "relay.sqlite3"), "python")
	script := `import json,sys
from codex_session_relay.store import Store
from codex_session_relay import faultsweep, delivery
s=Store(sys.argv[1]); old=delivery.supersession_reason; old_now=faultsweep._now; faultsweep._now=lambda store:'now'
delivery.supersession_reason=lambda db,event: 'overtaken by test'
try:
 print(json.dumps({'present':faultsweep.still_present(s,'delivery_stalled',{'recipient':'parent','attemptState':None}), 'memo':[dict(r) for r in s.all('SELECT * FROM fault_overtaken_deliveries ORDER BY event_id')]},sort_keys=True))
finally: delivery.supersession_reason=old;faultsweep._now=old_now;s.close()`
	cmd := exec.Command("uv", "run", "--no-sync", "python", "-c", script, filepath.Join(home, "py", "relay.sqlite3"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("python: %v %s", e, out)
	}
	var want map[string]any
	if e = json.Unmarshal(out, &want); e != nil {
		t.Fatal(e)
	}
	sw := &Sweeper{Store: goStore, Now: func() string { return "now" }, SupersessionReason: func(context.Context, string) (string, error) { return "overtaken by test", nil }}
	present, undetermined, e := sw.stillPresent(context.Background(), map[string]any{"recipient": "parent", "attemptState": nil})
	if e != nil {
		t.Fatal(e)
	}
	rows, e := goStore.All(context.Background(), "SELECT * FROM fault_overtaken_deliveries ORDER BY event_id")
	if e != nil {
		t.Fatal(e)
	}
	memo := []any{}
	for _, r := range rows {
		m := map[string]any{}
		for _, c := range r {
			m[c.Name] = c.Value
		}
		memo = append(memo, m)
	}
	raw, e := json.Marshal(map[string]any{"present": present || undetermined, "memo": memo})
	if e != nil {
		t.Fatal(e)
	}
	var got map[string]any
	if e = json.Unmarshal(raw, &got); e != nil {
		t.Fatal(e)
	}
	if want["present"] == nil {
		want["present"] = false
	} else if _, ok := want["present"].(map[string]any); ok {
		want["present"] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Go=%v Python=%v", got, want)
	}
}
