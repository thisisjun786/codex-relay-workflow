package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// verdictFixture is what a Python verdict scenario left: the scenarios it ran, the rows of the
// stores it backed up before each call, and each call to the verdict or outbox API (method,
// arguments, clock, token counter, the store before the call as its dump, and its tables then).
type verdictFixture struct {
	Scenarios []string        `json:"scenarios"`
	Rows      storePool       `json:"rows"`
	Calls     json.RawMessage `json:"calls"`
}

// verdictReplay makes each call of a Python verdict scenario on a copy of the store Python held
// before it, and checks Go's answer and every table it changed against the golden.
func verdictReplay(t *testing.T, names ...string) {
	t.Helper()
	// Python's temporary directories are this run's.
	pyDir := t.TempDir()
	var fixture verdictFixture
	if e := json.Unmarshal(relocated(readFixture(t, "verdict"), "<pytmp>", pyDir), &fixture); e != nil {
		t.Fatal(e)
	}
	sameScenarios(t, fixture.Scenarios, names)
	var stores []struct{ Database json.RawMessage }
	if e := json.Unmarshal(fixture.Calls, &stores); e != nil {
		t.Fatal(e)
	}
	d := json.NewDecoder(bytes.NewReader(fixture.Calls))
	d.UseNumber()
	value, e := decodeValue(d)
	if e != nil {
		t.Fatal(e)
	}
	if len(value.([]any)) == 0 {
		t.Fatal("no verdict/outbox calls")
	}
	for index, call := range value.([]any) {
		ctx := context.Background()
		// The store is a copy of Python's: it gets its own identity, and Go replays the call on it
		// after a takeover to Go.
		database := filepath.Join(t.TempDir(), "relay.sqlite3")
		restoreStore(t, fixture.Rows.dump(t, stores[index].Database), database)
		testsupport.Rehome(t, database)
		testsupport.HandOver(t, database, "go")
		s, e := store.Open(ctx, database, "")
		if e != nil {
			t.Fatal(e)
		}
		now, _ := reception.Get(call, "now").(json.Number).Float64()
		clock := &delivery.FakeClock{T: now}
		outbox := New(s, clock)
		counter, _ := reception.Get(call, "counter").(json.Number).Int64()
		outbox.Token = func() (string, error) { counter++; return fmt.Sprintf("%032x", counter), nil }
		args, _ := evidence.List(reception.Get(call, "args"))
		kwargs := reception.Get(call, "kwargs")
		kw := func(k string) any { return reception.Get(kwargs, k) }
		id := ""
		if len(args) > 0 {
			id = text(args[0])
		}
		instant := now
		if n, ok := kw("now").(json.Number); ok {
			instant, _ = n.Float64()
		}
		var got any
		var err error
		switch reception.Get(call, "method") {
		case "record_verdict":
			service := delivery.NewService(s, clock)
			ack := delivery.NewAck(service)
			ack.Sync = delivery.VerdictSync(s, clock)
			if reception.Get(call, "explode") == true {
				ack.Sync = func(context.Context, delivery.Relationship, delivery.Row, string, []any, delivery.Obj, any, int64) error {
					return errors.New("local sqlite failure")
				}
			}
			criteria, _ := evidence.List(kw("criteria"))
			findings, _ := evidence.List(kw("findings"))
			got, err = ack.RecordVerdict(ctx, id, text(kw("verdict")), text(kw("verdict_turn_id")), criteria, findings, kw("reason"), kw("expect_criteria_digest"))
		case "set_target":
			got, err = outbox.SetTarget(ctx, id, text(args[1]), text(args[2]))
		case "claim":
			got, err = outbox.Claim(ctx, id, text(kw("owner")), instant)
		case "operation":
			got, err = outbox.Operation(ctx, id)
		case "reconcile":
			got, err = outbox.Reconcile(ctx, id, text(args[1]))
		case "complete":
			var external *string
			if v, ok := kw("external_ref").(string); ok {
				external = &v
			}
			got, err = outbox.Complete(ctx, id, text(kw("claim_token")), text(kw("target_ref")), text(kw("readback")), external)
		case "fail":
			got, err = outbox.Fail(ctx, id, text(kw("claim_token")), text(kw("error")), instant)
		case "retry":
			got, err = outbox.Retry(ctx, id)
		case "snapshot":
			got, err = outbox.Snapshot(ctx, text(kw("relationship_id")))
		case "next":
			limit := 4
			if n, ok := kw("limit").(json.Number); ok {
				i, _ := n.Int64()
				limit = int(i)
			}
			got, err = outbox.Next(ctx, text(kw("target")), limit, instant)
		default:
			t.Fatal(reception.Get(call, "method"))
		}
		var actual any
		if err == nil {
			actual = obj("reply", got)
		} else {
			var refused *store.RefusedError
			if errors.As(err, &refused) {
				actual = obj("error", obj("reason", refused.Reason, "detail", refused.Detail))
			} else {
				actual = obj("runtimeError", err.Error())
				if err.Error() == "transaction body: local sqlite failure" {
					actual = obj("runtimeError", "local sqlite failure")
				}
			}
		}
		before, _ := evidence.Object(reception.Get(call, "before"))
		actualChanged := Obj{}
		for _, table := range before {
			rows, e := s.All(ctx, "SELECT * FROM "+table.Key)
			if e != nil {
				t.Fatal(e)
			}
			records := []any{}
			for _, row := range rows {
				records = append(records, rowObject(row))
			}
			if pyjson.Dumps(records, pyjson.Options{}) != pyjson.Dumps(table.Value, pyjson.Options{}) {
				actualChanged = append(actualChanged, struct {
					Key   string
					Value any
				}{table.Key, records})
			}
		}
		key := fmt.Sprintf("%03d %s", index, text(reception.Get(call, "method")))
		golden.Check(t, key+" answer", []byte(pyjson.Dumps(actual, pyjson.Options{})), golden.Substitute(pyDir, "<pytmp>"))
		golden.Check(t, key+" written tables", []byte(pyjson.Dumps(actualChanged, pyjson.Options{})), golden.Substitute(pyDir, "<pytmp>"))
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
