package integrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execStub replaces the world's program runner for the commands whose first word ends with
// suffix; every other command runs for real.
func (w *world) execStub(suffix string, stub func(ctx context.Context, c Cmd) (int, error)) {
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		if len(c.Argv) > 0 && strings.HasSuffix(c.Argv[0], suffix) {
			return stub(ctx, c)
		}
		return delegate(ctx, c)
	}
}

func (w *world) requireAllPending(nums ...string) {
	w.t.Helper()
	for _, n := range nums {
		w.mustWhere(n, "pending")
	}
}

// A signal (watch turns SIGINT and SIGTERM into a cancelled context) that arrives while a gate
// runs is not a verdict: the head under the gate and every head behind it stay pending.
func TestPassCancelledDuringGateKeepsEveryHeadPending(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.pend("2", w.head("b", map[string]string{"b.txt": "b\n"}, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.execStub("gate.sh", func(ctx context.Context, c Cmd) (int, error) {
		cancel()
		return -1, ctx.Err()
	})
	base := w.remoteDev()
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.requireAllPending("1", "2")
	if w.remoteDev() != base || len(rep.Moved) != 0 {
		t.Fatalf("a cancelled pass pushed or moved something: %+v", rep)
	}
}

// A verdict that was reached before the signal stays: only the heads not yet judged are kept.
func TestPassCancelledAfterAVerdictKeepsThatVerdictOnly(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.pend("2", w.head("b", map[string]string{"b.txt": "b\n"}, true))
	w.ctlWrite("block-CRW-1", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		rc, err := delegate(ctx, c)
		if len(c.Argv) > 1 && strings.HasSuffix(c.Argv[0], "gate.sh") && c.Argv[1] == "CRW-2" {
			cancel()
			return -1, ctx.Err()
		}
		return rc, err
	}
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.mustWhere("1", "blocked")
	w.mustWhere("2", "pending")
}

// A gate that could not run (not a negative verdict) does not block the head either.
func TestPassGateThatCannotRunKeepsTheHeadPending(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.execStub("gate.sh", func(context.Context, Cmd) (int, error) {
		return -1, errors.New("fork: resource temporarily unavailable")
	})
	rep := w.pass()
	requireExit(t, rep, ExitUsage, "error")
	w.requireAllPending("1")
	if !strings.Contains(rep.Error, "resource temporarily unavailable") {
		t.Fatalf("error %q", rep.Error)
	}
}

// A signal during the verification is not a failed verification: no retry marker, no solo
// state, no block.
func TestPassCancelledDuringVerifyCountsNoFailure(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.pend("2", w.head("b", map[string]string{"b.txt": "b\n"}, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.execStub("verify.sh", func(ctx context.Context, c Cmd) (int, error) {
		cancel()
		return -1, ctx.Err()
	})
	base := w.remoteDev()
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.requireAllPending("1", "2")
	if w.remoteDev() != base || rep.Retry != "" || fileExists(filepath.Join(w.cfg.QueueDir, "retried")) || len(w.r.soloOwed()) != 0 {
		t.Fatalf("a cancelled verification was counted: %+v", rep)
	}
}

// A signal during the merges leaves the head that was being merged pending, not in conflict.
func TestPassCancelledDuringMergeKeepsTheHeadPending(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		for _, a := range c.Argv {
			if a == "--is-ancestor" {
				cancel()
				return -1, ctx.Err()
			}
		}
		return delegate(ctx, c)
	}
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.requireAllPending("1")
}

// The record and verify commands run in the private worktree whatever Git routing the process
// inherited: GIT_DIR and GIT_WORK_TREE of the shared checkout must not reach them.
func TestPassHooksDoNotInheritGitRouting(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	w.r.Env = append(append([]string(nil), w.env...), "GIT_DIR="+filepath.Join(w.checkout, ".git"), "GIT_WORK_TREE="+w.checkout, "GIT_INDEX_FILE="+filepath.Join(w.checkout, ".git", "index"))
	before := w.git(w.checkout, "status", "--porcelain")
	w.write(filepath.Join(w.root, "scripts", "record.sh"), `#!/bin/sh
git rev-parse --show-toplevel > "$CTL_DIR/record-top" || exit 1
root=$(git rev-parse --show-toplevel)
n=$(ls plugins/crw/skills | wc -l | tr -d ' ')
sed "s/\"version\": \"[^\"]*\"/\"version\": \"v-$n\"/" "$root/plugins/crw/.codex-plugin/plugin.json" > "$root/plugins/crw/.codex-plugin/plugin.json.new" && mv "$root/plugins/crw/.codex-plugin/plugin.json.new" "$root/plugins/crw/.codex-plugin/plugin.json"
`)
	w.write(filepath.Join(w.root, "scripts", "verify.sh"), `#!/bin/sh
git rev-parse --show-toplevel > "$CTL_DIR/verify-top" || exit 1
echo '{"ok":true}' > "$CRW_VERIFY_RECORD"
`)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	for _, f := range []string{"record-top", "verify-top"} {
		top := strings.TrimSpace(w.ctlRead(f))
		if !strings.HasPrefix(top, w.cfg.WorkRoot+string(filepath.Separator)+"run-") {
			t.Fatalf("%s ran against %q, not the private worktree", f, top)
		}
	}
	if after := w.git(w.checkout, "status", "--porcelain"); after != before {
		t.Fatalf("the shared checkout changed:\n%s", after)
	}
	if got := w.fileAt("dev", manifestPath); !strings.Contains(got, `"version": "v-2"`) {
		t.Fatalf("the record command did not re-derive the pushed manifest: %s", got)
	}
}

// Configured record_version_env is the operator's explicit word and is applied after the
// inherited environment is cleaned.
func TestRecordVersionEnvIsAppliedAfterTheCleanup(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	w.r.Env = append(append([]string(nil), w.env...), "GIT_DIR="+filepath.Join(w.checkout, ".git"))
	w.write(filepath.Join(w.root, "scripts", "record.sh"), "#!/bin/sh\necho \"git_dir=${GIT_DIR-unset} extra=$EXTRA\" > \"$CTL_DIR/record-env\"\n")
	w.r.Cfg.RecordVersionEnv = map[string]string{"EXTRA": "{tag}"}
	w.pass()
	got := w.ctlRead("record-env")
	if !strings.Contains(got, "git_dir=unset") || !strings.Contains(got, "extra=cc-q") {
		t.Fatalf("record environment %q", got)
	}
}

// Solo mode owes every head of the failed batch one run of its own, however often an earlier
// member fails alone: batching resumes only after each member has had its run.
func TestPassSoloAttemptsEveryMemberBeforeBatchingAgain(t *testing.T) {
	w := newWorld(t)
	heads := map[string]string{}
	for i, n := range []string{"1", "2", "3", "4"} {
		name := string(rune('a' + i))
		heads[n] = w.head(name, map[string]string{name + ".txt": name + "\n"}, true)
		w.pend(n, heads[n])
	}
	// The batch fails twice; head 1 fails alone twice (blocked); head 2 fails alone twice
	// (blocked); then everything passes.
	w.ctlWrite("verify-rc", "3\n3\n3\n3\n3\n3\n0\n0\n0\n")
	for i := 0; i < 2; i++ {
		requireExit(t, w.pass(), ExitVerifyFailed, "verification-failed(rc=3)")
	}
	for i := 0; i < 4; i++ {
		requireExit(t, w.pass(), ExitVerifyFailed, "verification-failed(rc=3)")
	}
	w.mustWhere("1", "blocked")
	w.mustWhere("2", "blocked")
	w.requireAllPending("3", "4")

	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Merged != "crw-3="+heads["3"] {
		t.Fatalf("head 3 was not run alone: merged %q", rep.Summary.Merged)
	}
	w.mustWhere("4", "pending")
	rep = w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Merged != "crw-4="+heads["4"] {
		t.Fatalf("head 4 was not run alone: merged %q", rep.Summary.Merged)
	}
	if len(w.r.soloOwed()) != 0 {
		t.Fatalf("solo state left behind: %v", w.r.soloOwed())
	}
}

// A solo-owed head the coordinator refreshed or removed is no longer owed; the solo state
// never outlives the heads it names.
func TestPassSoloStateDropsHeadsThatLeftThePending(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	b := w.head("b", map[string]string{"b.txt": "b\n"}, true)
	w.pend("1", a)
	w.pend("2", b)
	w.ctlWrite("verify-rc", "3\n3\n")
	w.pass()
	w.pass()
	if len(w.r.soloOwed()) != 2 {
		t.Fatalf("owed %v", w.r.soloOwed())
	}
	if err := os.Remove(filepath.Join(w.cfg.QueueDir, "pending", "1")); err != nil {
		t.Fatal(err)
	}
	c := w.head("c", map[string]string{"c.txt": "c\n"}, true)
	w.pend("2", c)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if len(w.r.soloOwed()) != 0 {
		t.Fatalf("owed %v after both owed heads left", w.r.soloOwed())
	}
}
