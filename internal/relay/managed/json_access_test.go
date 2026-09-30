package managed

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Preserve the real registration/admission path; only the external transport
// receipt varies. Replaying it must retain the same Python status diagnostic.
type jsonReceiptHost struct {
	*managedFake
	value any
}

func (h *jsonReceiptHost) SendMessage(ctx context.Context, in SendRequest) (map[string]any, error) {
	receipt, err := h.managedFake.SendMessage(ctx, in)
	if err == nil {
		receipt["status"] = h.value
	}
	return receipt, err
}

// Each status value's state, stage and reason, over every JSON value shape, is the golden; it
// began as what Python's ManagedStart._business_result answered. Both runs of the start, the
// second a replay, must give the same result.
func Test24ManagedReceiptAccessorPython(t *testing.T) {
	values := evidence.Items(evidence.Decode(`[null, false, true, 0, 2, 1.5, "", "x", [], [1], {}, {"a": 1}]`))
	results := make([]any, len(values))
	defer func() { golden.CheckJSON(t, "business-results", results) }()
	for i, value := range values {
		t.Run(evidence.Repr(value), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			request := requestFixture(t)
			req, err := ParseRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			host := &jsonReceiptHost{managedFake: &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}, value: value}
			start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
			for range 2 {
				result, err := start.Run(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				got := evidence.Dict(result, false)
				answer := map[string]any{"value": evidence.Repr(value), "result": map[string]any{"state": got["state"], "stage": got["stage"], "reason": got["reason"]}}
				if results[i] == nil {
					results[i] = answer
				} else if !reflect.DeepEqual(results[i], answer) {
					t.Fatalf("the replay answered %v, the start %v", answer, results[i])
				}
			}
			if host.sent != 1 || host.created != 1 {
				t.Fatalf("replay repeated effects: %d/%d", host.sent, host.created)
			}
			turns, err := s.All(ctx, "SELECT * FROM generation_turns")
			if err != nil || len(turns) != 0 {
				t.Fatalf("malformed status admitted a turn: %v %v", turns, err)
			}
		})
	}
}
