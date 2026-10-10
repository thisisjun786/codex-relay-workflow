package dagsched

import (
	"context"
	"runtime"
	"testing"
)

// CRW-965 (parent decisions D1 and D2): the record judge compares the tool values with the record's own pins and the
// platform, and leaves goFlags and goEnv to the writer. The kit's tree declares no tool (CRW-1026 judges the declared ones). These tests pin both sides of that boundary.
func TestVerificationJudgeLeavesGoFlagsAndGoEnvToTheWriter(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	head := k.heads["a"]
	keys, err := CommitVerificationKeys(context.Background(), k.repo.path, head)
	if err != nil {
		t.Fatal(err)
	}
	want := VerificationKeys{Tree: k.treeOf(head), Base: k.base, CiDigest: keys.CiDigest, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Pins: keys.Pins}
	seal := func(tools, pins map[string]string) []byte {
		raw, err := SealVerificationRecord(VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: k.base, HeadCommit: head,
			TreeHash: want.Tree, CiDigest: keys.CiDigest, Tools: tools, Pins: pins, GoFlags: "-p=99", GoEnv: "GOFOO=bar",
			PinMismatch: []string{}, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", Jobs: nil})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// goFlags, goEnv and an unpinned tool value differ from anything this host runs: the record is judged by the keys, and reusable
	if _, err := JudgeVerificationRecord(seal(map[string]string{"go": "go0.0.1"}, map[string]string{}), want); err != nil {
		t.Fatalf("goFlags, goEnv and an unpinned tool are not judged by value: %v", err)
	}
	// a tool value that contradicts its own pin is refused
	_, err = JudgeVerificationRecord(seal(map[string]string{"go": "go0.0.1"}, map[string]string{"go": "go1.27.1"}), want)
	if refusalReasonOf(err) != "disposition_conflict" {
		t.Fatal("a tool that differs from its own pin must refuse the record as disposition_conflict")
	}
}

// CRW-965 (parent decision D2): a stored verification stands for the keys the verifier would judge now only when every key
// matches, the platform included. A missing key or another platform verifies again.
func TestVerifiedKeysHoldOnlyForTheSamePlatform(t *testing.T) {
	now := map[string]string{"tree": "t", "ci_digest": "c", "dependency_go_sum": "g", "dependency_web_lock": "", "os": "linux", "arch": "amd64"}
	same := map[string]string{}
	for key, value := range now {
		same[key] = value
	}
	if !verifiedKeysHold(same, now) {
		t.Fatal("the same keys and platform reuse the stored verification")
	}
	for _, key := range []string{"os", "arch", "ci_digest", "tree"} {
		other := map[string]string{}
		for k, v := range same {
			other[k] = v
		}
		other[key] = "other"
		if verifiedKeysHold(other, now) {
			t.Fatalf("a stored verification with another %s must verify again", key)
		}
	}
	missing := map[string]string{}
	for k, v := range same {
		if k != "arch" {
			missing[k] = v
		}
	}
	if verifiedKeysHold(missing, now) {
		t.Fatal("a row without the arch key is an older row and verifies again")
	}
}
