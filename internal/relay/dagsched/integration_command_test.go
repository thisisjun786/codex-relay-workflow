package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationNoopCASAllowsCheckedOutBranchAndRejectsMovedTip(t *testing.T) {
	r := newGitRepo(t)
	head := r.git("rev-parse", "dev")
	if err := UpdateIntegrationRef(context.Background(), r.path, "dev", head, head); err != nil {
		t.Fatalf("no-op CAS: %v", err)
	}
	r.git("commit", "--allow-empty", "-m", "concurrent move")
	if err := UpdateIntegrationRef(context.Background(), r.path, "dev", head, head); err == nil {
		t.Fatal("stale no-op CAS succeeded")
	}
	if got := r.git("rev-parse", "dev"); got == head {
		t.Fatal("CAS overwrote concurrent tip")
	}
}

func TestRepositoryVerificationCanRetainTheSameRecordOnRetry(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
	commandConfig(t, k.repo.path, "printf same-output", "")
	w := integrationWorktree{s: k.sched, in: k.batchIn(), deps: IntegrationDeps(repositoryIntegrationVerifier("printf same-output")), dir: k.repo.path, tmp: t.TempDir(), baseTip: k.base, batch: "command-retry"}
	for i := 0; i < 2; i++ {
		if _, _, pass, err := w.verify(context.Background()); err != nil || !pass {
			t.Fatalf("verification %d: pass %v, error %v", i, pass, err)
		}
	}
}

func TestRepositoryIntegrationFailureRetainsActualMoveWithoutMark(t *testing.T) {
	for _, failCommand := range []bool{false, true} {
		t.Run(map[bool]string{false: "verification fails", true: "command fails"}[failCommand], func(t *testing.T) {
			k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
			k.acceptByCommit("a")
			path := filepath.Join(t.TempDir(), "integrate.sh")
			body := "set -eu\ngit merge --no-ff --no-edit -m command-merge \"$1\"\n"
			if failCommand {
				body += "exit 9\n"
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			commandConfig(t, k.repo.path, "test ! -e a.txt", "sh '"+path+"'")
			in := k.batchIn()
			in.IntegrationRef = "dev"
			_, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationDeps(stubVerifier(writeStubVerifier(t))))
			if err == nil || countStage(k.stageRows(), "marked") != 0 {
				t.Fatalf("failure marked: %v", err)
			}
			tip := k.repo.git("rev-parse", "dev")
			if tip == k.base {
				t.Fatal("command's actual move was undone")
			}
			move, failedRecord := false, false
			for _, row := range k.stageRows() {
				var detail map[string]any
				if json.Unmarshal([]byte(row.Detail), &detail) != nil {
					continue
				}
				if detail["command_head"] == tip && detail["old_head"] == k.base {
					move = true
				}
				if raw, ok := detail["verification_record"].(string); ok {
					rec, err := DecodeVerificationRecord([]byte(raw))
					if err == nil && rec.Result == "fail" && rec.CommandVerification != nil {
						failedRecord = true
					}
				}
			}
			if !move || (!failCommand && !failedRecord) {
				t.Fatalf("lost move %v or failure record %v", move, failedRecord)
			}
		})
	}
}

func TestRepositoryIntegrationChecksThePostCommandTip(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(map[bool]string{false: "same tip", true: "concurrent tip"}[moved], func(t *testing.T) {
			k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
			k.acceptByCommit("a")
			path := filepath.Join(t.TempDir(), "integrate.sh")
			if err := os.WriteFile(path, []byte("set -eu\ngit show-ref --verify refs/heads/\"$1\"\ngit merge --no-ff --no-edit -m command-merge \"$1\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			commandConfig(t, k.repo.path, "", "sh '"+path+"'")
			in := k.batchIn()
			in.IntegrationRef = "dev"
			calls := 0
			deps := IntegrationDeps(stubVerifier(writeStubVerifier(t)))
			deps.Update = func(ctx context.Context, checkout, ref, next, old string) error {
				calls++
				if next != old || next == k.base {
					t.Fatalf("not a post-command CAS: %s -> %s", old, next)
				}
				if moved {
					k.repo.git("commit", "--allow-empty", "-m", "concurrent")
				}
				return UpdateIntegrationRef(ctx, checkout, ref, next, old)
			}
			res, err := k.sched.IntegrateBatch(context.Background(), in, deps)
			if calls != 1 {
				t.Fatalf("CAS calls %d; err %v", calls, err)
			}
			if moved {
				if err == nil || countStage(k.stageRows(), "marked") != 0 {
					t.Fatalf("concurrent move accepted: %+v %v", res, err)
				}
			} else if err != nil || len(res.Merged) != 1 || countStage(k.stageRows(), "marked") != 1 {
				t.Fatalf("integration: %+v %v", res, err)
			}
			found := false
			for _, row := range k.stageRows() {
				if strings.Contains(row.Detail, `"integrate_command"`) && strings.Contains(row.Detail, `"command_head"`) && strings.Contains(row.Detail, `"exit":0`) {
					found = true
				}
			}
			if !found {
				t.Fatal("command result and actual target missing")
			}
		})
	}
}

func TestRepositoryIntegrationNeverCallsMainOrReleaseCommands(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
	k.acceptByCommit("a")
	marker := filepath.Join(t.TempDir(), "called")
	commandConfig(t, k.repo.path, "", "touch '"+marker+"'")
	for _, target := range []string{"main", "release/next"} {
		in := k.batchIn()
		in.IntegrationRef = target
		if _, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationDeps(stubVerifier(writeStubVerifier(t)))); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("release command ran")
	}
}
