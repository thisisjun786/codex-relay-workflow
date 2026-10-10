package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The recorded oracle grids keep what CXC v0.2.40 answered. A case whose answer the port changed on purpose is listed in
// testdata/<suite>/port-fixed.json by the case's key, with the port's answer in the grid's own shape, and the replay reads that answer
// in place of the recorded one. A key that names no case is a stale entry and fails the suite (portFixedUsed).

// portFixed reads testdata/<suite>/port-fixed.json: {"<case key>": <answer>}.
func portFixed(t *testing.T, suite string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", suite, "port-fixed.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// portFixedDump appends the answer of a case that differs from its recorded one to the file named by CRW_DUMP_FIXED, so that a reviewed
// change can regenerate the suite's port-fixed.json. It does nothing when the variable is unset.
func portFixedDump(suite, key string, answer any) {
	path := os.Getenv("CRW_DUMP_FIXED")
	if path == "" {
		return
	}
	b, err := json.Marshal(map[string]any{"suite": suite, "key": key, "answer": answer})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
