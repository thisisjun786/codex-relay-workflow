package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func verdictReplay(t *testing.T, names ...string) {
	t.Helper()
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(names)
	if e != nil {
		t.Fatal(e)
	}
	keep := t.TempDir()
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/sync/testdata/verdict_capture.py"), keep, string(raw))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+keep+"/uv")
	output, e := cmd.Output()
	if e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			t.Fatalf("Python verdict scenarios: %v\n%s", e, exit.Stderr)
		}
		t.Fatal(e)
	}
	d := json.NewDecoder(bytes.NewReader(output))
	d.UseNumber()
	value, e := decodeValue(d)
	if e != nil {
		t.Fatal(e)
	}
	calls := value.([]any)
	if len(calls) == 0 {
		t.Fatal("no verdict/outbox calls")
	}
	for index, call := range calls {
		ctx := context.Background()
		s, e := store.Open(ctx, text(reception.Get(call, "database")), "")
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
		var actual, expected any
		if err == nil {
			actual = obj("reply", got)
		} else {
			var refused *store.RefusedError
			if errors.As(err, &refused) {
				actual = obj("error", obj("reason", refused.Reason, "detail", refused.Detail))
			} else if reception.Has(call, "runtimeError") {
				actual = obj("runtimeError", err.Error())
				if err.Error() == "transaction body: local sqlite failure" {
					actual = obj("runtimeError", "local sqlite failure")
				}
			} else {
				t.Fatal(err)
			}
		}
		if reception.Has(call, "error") {
			expected = obj("error", reception.Get(call, "error"))
		} else if reception.Has(call, "runtimeError") {
			expected = obj("runtimeError", reception.Get(call, "runtimeError"))
		} else {
			expected = obj("reply", reception.Get(call, "reply"))
		}
		if want, got := evidence.Dumps(expected, false, false, true), evidence.Dumps(actual, false, false, true); got != want {
			t.Fatalf("call %d %s\nPython:%s\nGo:%s", index, reception.Get(call, "method"), want, got)
		}
		before, _ := evidence.Object(reception.Get(call, "before"))
		wantChanged := reception.Get(call, "tables")
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
			if evidence.Dumps(records, false, false, true) != evidence.Dumps(table.Value, false, false, true) {
				actualChanged = append(actualChanged, struct {
					Key   string
					Value any
				}{table.Key, records})
			}
		}
		if got, want := evidence.Dumps(actualChanged, false, false, true), evidence.Dumps(wantChanged, false, false, true); got != want {
			t.Fatalf("call %d %s all written tables differ\nPython:%s\nGo:%s", index, reception.Get(call, "method"), want, got)
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
