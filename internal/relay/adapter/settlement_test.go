package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

type cancelledDelivery struct {
	*Adapter
	err error
}

func (a cancelledDelivery) SendMessage(string, string, string, *delivery.TaskSettings) (delivery.Obj, error) {
	return nil, &delivery.HostError{Kind: "_ShutdownCancelled", Message: a.err.Error()}
}
func (a cancelledDelivery) IsArchived(string, any) (*bool, error) { value := false; return &value, nil }
func (a cancelledDelivery) ReadGoalStatus(string) (any, error)    { return nil, nil }
func (a cancelledDelivery) ReadThread(string) (delivery.ThreadFacts, error) {
	value := true
	return delivery.ThreadFacts{RuntimeStatus: "idle", CanAcceptInput: &value}, nil
}
func (a cancelledDelivery) ListTurnIDs(string, int) ([]any, error) { return []any{}, nil }

func allTables(t *testing.T, s *store.Store) map[string]any {
	t.Helper()
	ctx := context.Background()
	rows, err := s.Querier(ctx).QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	tables := map[string]any{}
	for _, name := range names {
		rows, err := s.Querier(ctx).QueryContext(ctx, `SELECT * FROM "`+name+`" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		all := []any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				if b, ok := value.([]byte); ok {
					values[i] = string(b)
				}
			}
			all = append(all, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		tables[name] = all
	}
	return tables
}
func Test28_BAD_14_ShutdownSettlesClaimedDelivery(t *testing.T) {
	root := t.TempDir()
	repo := pyRepo(t)
	script := filepath.Join(repo, "internal/relay/adapter/testdata/settlement_capture.py")
	// The oracle reads <pyDir>/python.sqlite3. Each runtime's store has a directory of its own:
	// a directory holds one store's takeover.json.
	pyDir, goPath := filepath.Join(root, "python"), filepath.Join(root, "go", "go.sqlite3")
	seed := copyDeliverySeed(t, root)
	oracle := func(args ...string) []byte {
		return pyOutput(t, "settlement_capture.py "+strings.Join(args[:1], " "), func() *exec.Cmd {
			// Both results include every table, schema_meta too, and the only runtime difference
			// that comparison admits is the owner. So each store is the seed as a pre-fence
			// Python wrote it, stamped for the runtime that runs on it: Python's by its fence,
			// Go's as Go stamps a store. (Copies of the fenced seed cannot serve: one runtime
			// would own it only after a takeover, at another epoch and with a takeover id.)
			preFenceFixture(t, filepath.Join(root, "python.sqlite3"), filepath.Join(pyDir, "python.sqlite3"), "")
			testsupport.Fence(t, filepath.Join(pyDir, "python.sqlite3"), "python")
			cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", script, pyDir}, args...)...)
			cmd.Dir = repo
			return cmd
		}, seed.derived()...)
	}
	preFenceFixture(t, filepath.Join(root, "python.sqlite3"), goPath, "")
	testsupport.Fence(t, goPath, "go")
	rpc := &heldRPC{make(chan struct{}), make(chan struct{})}
	l, err := ledger.OpenWithOptions(filepath.Join(root, "cancel.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: rpc, Ledger: l, Drain: -1})
	cancelled := make(chan error, 1)
	go func() {
		_, err := a.SendMessage("req-cancelled", "thread-a", "hello", &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)})
		cancelled <- err
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-rpc.entered:
	case <-timer.C:
		t.Fatal("send not reached")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var cancellation error
	select {
	case cancellation = <-cancelled:
	case <-timer.C:
		t.Fatal("send not settled")
	}
	if cancellation == nil {
		t.Fatal("shutdown accepted")
	}
	s, err := store.Open(context.Background(), goPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	service := delivery.NewService(s, delivery.NewFakeClock())
	result, err := service.Attempt(context.Background(), seed.Event, cancelledDelivery{Adapter: a, err: cancellation}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{"result": plain(result), "tables": allTables(t, s)}
	var want map[string]any
	if err := json.Unmarshal(oracle("run", seed.Event), &want); err != nil {
		t.Fatal(err)
	}
	ownerNeutral(t, testsupport.Go, got["tables"])
	ownerNeutral(t, testsupport.Python, want["tables"])
	actual, _ := json.Marshal(got)
	expected, _ := json.Marshal(want)
	if !bytes.Equal(actual, expected) {
		t.Fatalf("Go %s\nPython %s", actual, expected)
	}
}
