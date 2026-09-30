package managed

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
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

func Test24ManagedReceiptAccessorPython(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `import json
from codex_session_relay.managed import ManagedStart
m=object.__new__(ManagedStart)
m.result=lambda state,stage,reason: {"state":state,"stage":stage,"reason":reason}
print(json.dumps([{"value":v,"result":m._business_result({"status":v})} for v in [None,False,True,0,2,1.5,"","x",[],[1],{}, {"a":1}]]))`
	raw := pyoracle.Answer(t, "business-results", func() ([]byte, error) {
		raw, err := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("oracle: %v\n%s", err, raw)
		}
		return raw, nil
	})
	for _, one := range evidence.Items(evidence.Decode(string(raw))) {
		tc := evidence.Dict(one, false)
		t.Run(evidence.Repr(tc["value"]), func(t *testing.T) {
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
			host := &jsonReceiptHost{managedFake: &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}, value: tc["value"]}
			start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
			for range 2 {
				result, err := start.Run(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				got := evidence.Dict(result, false)
				want := evidence.Dict(tc["result"], false)
				for _, key := range []string{"state", "stage", "reason"} {
					if got[key] != want[key] {
						t.Fatalf("%s diff: Go=%v Python=%v", key, got[key], want[key])
					}
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
