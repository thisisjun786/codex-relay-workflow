package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// freshRecord writes a PASS record for tree with one job whose timings differ, so its bytes differ from record's.
func (k *commitAcceptKit) freshRecord(tree, job string) string {
	k.t.Helper()
	keys, err := CommitVerificationKeys(context.Background(), k.repo.path, k.head)
	if err != nil {
		k.t.Fatal(err)
	}
	record := VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: k.base, HeadCommit: k.head, TreeHash: tree,
		CiDigest: keys.CiDigest, Tools: map[string]string{"go": "go1.27.1"}, Pins: map[string]string{}, GoFlags: "", GoEnv: "",
		PinMismatch: []string{}, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass",
		Jobs: []json.RawMessage{json.RawMessage(job)}}
	raw, err := SealVerificationRecord(record)
	if err != nil {
		k.t.Fatal(err)
	}
	path := filepath.Join(k.t.TempDir(), "fresh-record.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		k.t.Fatal(err)
	}
	return "@" + path
}

// CRW-965 (parent decision, D3): a fresh record for the same head and tree is the same output. The replay proceeds on the
// head and tree the verification row stores; the file bytes are not compared.
func TestCommitAcceptanceReplaysOnAFreshRecordOfTheSameTree(t *testing.T) {
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	if _, err := k.acceptCommit(k.record(k.treeOf(k.head), "pass", nil)); err != nil {
		t.Fatalf("first acceptance: %v", err)
	}
	out, err := k.acceptCommit(k.freshRecord(k.treeOf(k.head), `{"name":"go test","seconds":"312"}`))
	if err != nil {
		t.Fatalf("a fresh record of the same tree should replay the acceptance: %v", err)
	}
	if !out.Replayed {
		t.Fatalf("want a replay, got %+v", out)
	}
}
