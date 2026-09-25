package contracttest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func runLedger(t *testing.T, scenario Scenario) (map[string]any, error) {
	t.Helper()
	root, err := Root()
	if err != nil {
		return nil, err
	}
	name, ok := scenario.Given["fixture"].(string)
	if !ok {
		return nil, fmt.Errorf("%s: missing fixture", scenario.ID)
	}
	raw, err := os.ReadFile(filepath.Join(root, "contract", "fixtures", scenario.Domain, name))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(t.TempDir(), "operations.sqlite3")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return nil, err
	}
	l, err := ledger.Open(path)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	id, ok := scenario.Run["request_id"].(string)
	if !ok {
		return nil, fmt.Errorf("missing request_id")
	}
	method, ok := scenario.Run["method"].(string)
	if !ok {
		return nil, fmt.Errorf("missing method")
	}
	params, ok := scenario.Run["params"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing params")
	}
	fresh, receipt, err := l.Begin(context.Background(), id, method, params, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"exit": float64(0), "fresh": fresh, "receipt": map[string]any(receipt)}, nil
}
