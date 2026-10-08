package dagsched

import (
	"context"
	"testing"
)

// CRW-965 (parent decision D6): the push takes only a head the relay verified and moved the branch onto. A tip with one
// local commit on top of the verified move, and a tip of another branch, are refused and the remote does not move.
func TestPushRefusesATipThatTheRelayDidNotMove(t *testing.T) {
	cases := map[string]func(k *batchKit, other string){
		"an extra local commit on the verified move": func(k *batchKit, other string) {
			k.repo.git("checkout", "-q", "dev-int")
			k.repo.write("extra.txt", "extra\n")
			k.repo.git("add", "extra.txt")
			k.repo.git("commit", "-q", "-m", "extra")
			k.repo.git("checkout", "-q", "dev")
		},
		"a tip of an unrelated branch": func(k *batchKit, other string) {
			k.repo.git("update-ref", "refs/heads/dev-int", other)
		},
	}
	for name, move := range cases {
		t.Run(name, func(t *testing.T) {
			k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
			remote := bareRemote(t)
			k.pushRepo(t, remote)
			k.acceptByCommit("a")
			if _, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil {
				t.Fatalf("batch: %v", err)
			}
			k.repo.git("checkout", "-q", "-B", "unrelated", k.base)
			k.repo.write("unrelated.txt", "unrelated\n")
			k.repo.git("add", "unrelated.txt")
			k.repo.git("commit", "-q", "-m", "unrelated")
			other := k.repo.git("rev-parse", "HEAD")
			k.repo.git("checkout", "-q", "dev")
			move(k, other)
			_, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto)
			if refusalReasonOf(err) != "merge_base_mismatch" {
				t.Fatalf("a tip the relay did not move must be refused before the push: %v", err)
			}
			if got := remoteBranch(t, remote, "dev"); got != "" {
				t.Fatalf("the remote moved to %s although the tip was not a verified move", got)
			}
		})
	}
}
