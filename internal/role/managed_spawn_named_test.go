package role

import (
	"os"
	"path/filepath"
	"testing"
)

// CRW-1260 (integration cc-q10120109): a V2 ciphertext call whose task_name names no record under the workspace or a parent runs
// no git: the CXC hook makes no external call on such a spawn (corpus case
// hook__pre-tool-use-attaching-skills__v2_ciphertext_preserved_with_routing). A record that is there still resolves the root.
func TestManagedSpawnNamedRunsNoGitWithoutARecord(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ws := t.TempDir()
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	sel, source, err := NewManagedSpawnResolver(sub).Named("s1", "t", nil, nil)
	if sel != nil || source != "" || err != nil {
		t.Fatalf("a task_name without a record named %v %q %v", sel, source, err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		data, _ := os.ReadFile(calls)
		t.Fatalf("git ran for a call that names no record: %q", data)
	}
	// A record in a parent of the workspace is a reason to resolve the root (the record itself is not valid, so nothing is bound).
	dir := filepath.Join(ws, ".crw", "dispatches", "s1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "t.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sel, _, _ := NewManagedSpawnResolver(sub).Named("s1", "t", nil, nil); sel != nil {
		t.Fatalf("an invalid record bound the call: %v", sel)
	}
	if _, err := os.Stat(calls); err != nil {
		t.Fatalf("a record under a parent did not resolve the root: %v", err)
	}
}
