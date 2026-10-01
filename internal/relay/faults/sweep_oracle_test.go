package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Compare the complete presence verdict and memo table on a seeded disposable store with the
// golden, which began as the Python source's answer.
func Test22_OvertakenPresenceWholePythonPage(t *testing.T) {
	goldenParent(t)
	for _, total := range []int{1, presentChecks + 1} {
		t.Run(fmt.Sprint(total), func(t *testing.T) { testOvertakenPresenceWholePythonPage(t, total) })
	}
}

func testOvertakenPresenceWholePythonPage(t *testing.T, total int) {
	home := t.TempDir()
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
	checkGolden(t, "presence and memo", nil, runPathsOf(t, home), got)
}
