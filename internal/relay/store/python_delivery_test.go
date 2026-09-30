package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// pythonDeliveryScript drives the real Python DeliveryService over a fresh store under
// TMPDIR (a t.TempDir) and prints the store path plus what Python's own inspection reports.
const pythonDeliveryScript = `
import json, sys
from tests.support import DeliveryTestCase, PARENT
from codex_session_relay.delivery import DeliveryService
class C(DeliveryTestCase):
    def runTest(self): pass
c = C(); c.setUp()
_r, event = c.queued_event()
scenario = sys.argv[1]
records = []
if scenario == "busy_retry":
    c.adapter.script("busy")
    records.append(c.attempt(event))
    records.append(c.attempt(event, now=c.clock.now() + 10_000))
elif scenario == "unknown":
    c.adapter.script("transport_unknown")
    records.append(c.attempt(event))
elif scenario == "interleave":
    earlier = DeliveryService(c.store, c.registry, c.intake, c.clock)
    original = c.adapter.list_turn_ids
    fired = []
    def settle_first(thread_id, limit=20):
        ids = original(thread_id, limit=limit)
        if not fired:
            fired.append(True)
            c.adapter.script("busy")
            earlier.attempt(event, c.adapter, now=c.clock.now() - 60, owner="earlier")
        return ids
    c.adapter.list_turn_ids = settle_first
    c.attempt(event)
    assert fired
elif scenario == "legacy":
    record = c.attempt(event)
    with c.store.transaction() as db:
        db.execute("DELETE FROM attempt_messages WHERE request_id = ?", (record["requestId"],))
elif scenario == "none":
    pass
else:
    records.append(c.attempt(event))
print(json.dumps({"db": str(c.store.path), "event": event,
                  "returned": [r["requestId"] for r in records],
                  "messages": c.delivery.attempt_messages(event),
                  "sends": [[r, m] for r, _t, m, _o in c.adapter.sends],
                  "deliveryState": c.delivery.get(event)["state"]}))
c.store.close()
`

type pythonAttempt struct {
	RequestID     string  `json:"requestId"`
	AttemptNo     int64   `json:"attemptNo"`
	Status        string  `json:"status"`
	DeliveryState *string `json:"deliveryState"`
	Message       *string `json:"message"`
}

type pythonDelivery struct {
	DB            string          `json:"db"`
	Event         string          `json:"event"`
	Returned      []string        `json:"returned"`
	Messages      []pythonAttempt `json:"messages"`
	Sends         [][2]string     `json:"sends"`
	DeliveryState string          `json:"deliveryState"`
}

// pythonDeliveryStore returns what Python recorded and a Go store holding the rows Python's
// store held (restoreStore). Both are recorded (pythonOracle).
func pythonDeliveryStore(t *testing.T, scenario string) (pythonDelivery, *Store) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("TMPDIR", root)
	package_ := filepath.Join(repositoryRoot(t), "packages/codex-session-relay")
	isolated := isolatedEnv(t)
	var answer struct {
		Delivery pythonDelivery `json:"delivery"`
		Rows     storeRows      `json:"rows"`
	}
	parts := append([]string{"delivery-store", package_, pythonDeliveryScript, scenario}, keptEnvironment()...)
	jsonAnswer(t, parts, &answer, func() (any, error) {
		raw, err := runPythonStore(t, isolated, package_, pythonDeliveryScript, scenario)
		if err != nil {
			return nil, err
		}
		out := strings.TrimSpace(string(raw))
		var got pythonDelivery
		if err := json.Unmarshal([]byte(out[strings.LastIndex(out, "\n")+1:]), &got); err != nil {
			return nil, fmt.Errorf("python output %q: %v", out, err)
		}
		rows, err := dumpStore(got.DB)
		// Python's store is a temporary directory of its own; the rows stand for it.
		got.DB = ""
		return map[string]any{"delivery": got, "rows": rows}, err
	}, sameUpToNoise)
	got := answer.Delivery
	got.DB = filepath.Join(root, "restored", "relay.sqlite3")
	return got, restoreStore(t, got.DB, answer.Rows)
}

func requestToken(message string) string {
	for line := range strings.SplitSeq(message, "\n") {
		if value, ok := strings.CutPrefix(line, "requestId: "); ok {
			return value
		}
	}
	return ""
}

// requireSameHistory asserts Go reads the Python-written history exactly as Python reports it.
func requireSameHistory(t *testing.T, python pythonDelivery, got []FrozenAttempt) {
	t.Helper()
	if len(got) != len(python.Messages) {
		t.Fatalf("go %+v python %+v", got, python.Messages)
	}
	for i, want := range python.Messages {
		row := got[i]
		same := row.RequestID == want.RequestID && row.Number == want.AttemptNo && row.Status == want.Status &&
			row.Message.Valid == (want.Message != nil) && (want.Message == nil || row.Message.String == *want.Message) &&
			row.DeliveryState.Valid == (want.DeliveryState != nil) && (want.DeliveryState == nil || row.DeliveryState.String == *want.DeliveryState)
		if !same {
			t.Fatalf("row %d: go %+v python %+v", i, row, want)
		}
	}
}
