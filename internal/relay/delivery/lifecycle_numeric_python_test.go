package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func Test28LifecycleNumericSQLiteParity(t *testing.T) {
	raw := []string{"9223372036854775808", "-9223372036854775809", "1e300", "NaN", "Infinity", "-Infinity", "true", "false", "7", "1.5"}
	values := []any{json.Number(raw[0]), json.Number(raw[1]), json.Number(raw[2]), math.NaN(), math.Inf(1), math.Inf(-1), true, false, json.Number(raw[8]), json.Number(raw[9])}
	for i, value := range values {
		t.Run(raw[i], func(t *testing.T) {
			s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			err = RecordLifecycle(context.Background(), s, &FakeClock{T: 1700000000}, Lifecycle{TaskID: raw[i], RuntimeStatus: "idle", Archived: new(bool), CanAcceptInput: value, Deliverable: "yes"})
			got := map[string]any{}
			if err != nil {
				var host *dispatch.HostError
				if !errors.As(err, &host) {
					t.Fatal(err)
				}
				got["error"], got["detail"] = host.Class, host.Detail
			} else {
				var stored any
				var kind string
				if err := s.DB.QueryRow("SELECT can_accept_input, typeof(can_accept_input) FROM recipient_lifecycle").Scan(&stored, &kind); err != nil {
					t.Fatal(err)
				}
				got["value"], got["kind"] = stored, kind
			}
			actual, _ := json.Marshal(got)
			golden.Check(t, "lifecycle", actual)
		})
	}
}
