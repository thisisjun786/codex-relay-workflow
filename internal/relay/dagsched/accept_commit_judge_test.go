package dagsched

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// CRW-965 (parent decision d1): the relay judges a record by its reuse keys only. The tool versions and the Go
// flags and environment are the writer's to judge (CRW-964 refuses a pin mismatch, and its --reuse compares the
// environment), so a record that names other values is still reusable here, while a record without the member is
// refused.
func TestVerificationRecordIsJudgedByItsKeysNotByEnvironmentValues(t *testing.T) {
	k := newCommitAcceptKit(t)
	ctx := context.Background()
	keys, err := CommitVerificationKeys(ctx, k.repo.path, k.head)
	if err != nil {
		t.Fatal(err)
	}
	want := VerificationKeys{Tree: k.treeOf(k.head), Base: k.base, CiDigest: keys.CiDigest, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH}
	record := VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: k.base, HeadCommit: k.head, TreeHash: want.Tree,
		CiDigest: keys.CiDigest, Tools: map[string]string{"go": "go0.0-other"}, Pins: map[string]string{}, GoFlags: "-mod=vendor", GoEnv: "GOOS=plan9",
		PinMismatch: []string{}, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", Jobs: []json.RawMessage{}}
	raw, err := SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JudgeVerificationRecord(raw, want); err != nil {
		t.Fatalf("a record that differs only in tools, goFlags and goEnv should be judged reusable by its keys: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	delete(members, "goFlags")
	without, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JudgeVerificationRecord(without, want); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "goFlags") {
		t.Fatalf("a record without its goFlags member should be refused under its name, got %v", err)
	}
}
