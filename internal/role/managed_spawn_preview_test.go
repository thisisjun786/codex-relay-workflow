package role

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1124: the issuance works on the preview's root and attempt, and still re-reads the record under its lock: a record that
// changed after the preview (another attempt, another candidate, an issuance to another call) is refused.
func TestManagedSpawnIssuanceRechecksTheRecordUnderTheLock(t *testing.T) {
	const ledger = `{"version":1,"sessionId":"s-1","id":"one","role":"executor","candidates":[{"model":"m-primary","effort":"high"}],"attempts":[{"id":"att-1","candidate":{"model":"m-primary","effort":"high"},"claimed":true,"agentId":null,"observedModel":null,"code":null,"taskFailure":null,"status":"claimed","reconciliation":null,"spawnIssued":false,"toolUseId":null}],"status":"active"}`
	for _, c := range []struct{ name, from, to string }{
		{"another attempt", `"id":"att-1"`, `"id":"att-2"`},
		{"another candidate", `"candidate":{"model":"m-primary"`, `"candidate":{"model":"m-other"`},
		{"issued to another call", `"spawnIssued":false,"toolUseId":null`, `"spawnIssued":true,"toolUseId":"other-call"`},
		{"no longer active", `"status":"active"`, `"status":"exhausted"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			path := filepath.Join(ws, ".crw", "dispatches", "s-1", "one.json")
			check(t, os.MkdirAll(filepath.Dir(path), 0o700))
			check(t, os.WriteFile(path, []byte(ledger), 0o600))
			preview, err := ManagedSpawn(ws, "s-1", "[CRW-DISPATCH:one:att-1]\nTASK: x")
			check(t, err)
			if preview == nil || preview.Candidate.Model == nil || *preview.Candidate.Model != "m-primary" {
				t.Fatalf("preview = %+v", preview)
			}
			changed := strings.Replace(ledger, c.from, c.to, 1)
			check(t, os.WriteFile(path, []byte(changed), 0o600))
			call := "this-call"
			if _, err := IssueManagedSpawnSelection(preview, &call, nil); err == nil {
				t.Fatal("a record changed after the preview was issued")
			}
			if got := string(must(os.ReadFile(path))); got != changed {
				t.Fatalf("a refused issuance wrote the record: %s", got)
			}
		})
	}
	// The unchanged record is issued, and the same call again is accepted.
	ws := t.TempDir()
	path := filepath.Join(ws, ".crw", "dispatches", "s-1", "one.json")
	check(t, os.MkdirAll(filepath.Dir(path), 0o700))
	check(t, os.WriteFile(path, []byte(ledger), 0o600))
	preview, err := ManagedSpawn(ws, "s-1", "[CRW-DISPATCH:one:att-1]\nTASK: x")
	check(t, err)
	call := "this-call"
	for i := 0; i < 2; i++ {
		if _, err := IssueManagedSpawnSelection(preview, &call, nil); err != nil {
			t.Fatalf("issuance %d: %v", i, err)
		}
	}
}
