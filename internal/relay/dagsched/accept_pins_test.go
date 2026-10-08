package dagsched

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// CRW-965 (parent decision, D1 refined): a record whose tool values contradict its own pins is internally inconsistent and
// is refused. A tool without a pin is not judged, and goFlags and goEnv stay the writer's.
func TestVerificationRecordPinsMustMatchTheirTools(t *testing.T) {
	k := newCommitAcceptKit(t)
	ctx := context.Background()
	keys, err := CommitVerificationKeys(ctx, k.repo.path, k.head)
	if err != nil {
		t.Fatal(err)
	}
	want := VerificationKeys{Tree: k.treeOf(k.head), Base: k.base, CiDigest: keys.CiDigest, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH}
	seal := func(tools, pins map[string]string) []byte {
		t.Helper()
		record := VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: k.base, HeadCommit: k.head, TreeHash: want.Tree,
			CiDigest: keys.CiDigest, Tools: tools, Pins: pins, GoFlags: "", GoEnv: "", PinMismatch: []string{},
			Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass"}
		raw, err := SealVerificationRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if _, err := JudgeVerificationRecord(seal(map[string]string{"go": "go1.27.1"}, map[string]string{"go": "go1.27.1"}), want); err != nil {
		t.Fatalf("a record whose tools match its pins is reusable: %v", err)
	}
	contradicting := seal(map[string]string{"go": "go0.0-other"}, map[string]string{"go": "go1.27.1"})
	if _, err := JudgeVerificationRecord(contradicting, want); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "go") {
		t.Fatalf("a tool that contradicts its pin is refused under its name, got %v", err)
	}
	if _, err := JudgeVerificationRecord(seal(map[string]string{"go": "go0.0-other"}, map[string]string{}), want); err != nil {
		t.Fatalf("a tool without a pin is not judged: %v", err)
	}
}
