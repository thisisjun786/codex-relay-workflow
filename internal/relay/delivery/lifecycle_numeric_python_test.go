package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test28LifecycleNumericSQLiteParity(t *testing.T) {
	raw := []string{"9223372036854775808", "-9223372036854775809", "1e300", "NaN", "Infinity", "-Infinity", "true", "false", "7", "1.5"}
	repo, _ := filepath.Abs("../../..")
	input, _ := json.Marshal(raw)
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/delivery/testdata/lifecycle_numeric.py"))
	cmd.Dir = repo
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python lifecycle oracle: %v\n%s", err, out)
	}
	var want []map[string]any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
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
				var host *hostError
				if !errors.As(err, &host) {
					t.Fatal(err)
				}
				got["error"], got["detail"] = host.kind, host.message
			} else {
				var stored any
				var kind string
				if err := s.DB.QueryRow("SELECT can_accept_input, typeof(can_accept_input) FROM recipient_lifecycle").Scan(&stored, &kind); err != nil {
					t.Fatal(err)
				}
				got["value"], got["kind"] = stored, kind
			}
			actual, _ := json.Marshal(got)
			expected, _ := json.Marshal(want[i])
			if !bytes.Equal(actual, expected) {
				t.Fatalf("Go %s Python %s", actual, expected)
			}
		})
	}
}
