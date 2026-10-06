package dagsched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// errLegacyLocalForgeTarget is the legacy local kit's refusal of a repository that is not an absolute path: the production mergeturn.TargetReader would take an owner/name target to the forge,
// and this kit reads only the temporary repository its plan lands on.
var errLegacyLocalForgeTarget = errors.New("the legacy local kit reads only an absolute local path; an owner/name repository would take the production reader to the forge, so it is refused")

// legacyLocalTipReader is the legacy local kit's tip reader: the production local reader behind that guard. An absolute path is the world the kit exists for and goes to mergeturn.TargetReader
// unchanged; anything else is refused here, before the production reader can reach gh. Every refusal is remembered and fails the test when it ends, so a caller that swallows the error (a sweep
// skips a tip it cannot read) still fails the test; a test that provokes the refusal on purpose takes it with takeRefused.
type legacyLocalTipReader struct {
	t       testing.TB
	mu      sync.Mutex
	refused []string
}

func newLegacyLocalTipReader(t testing.TB) *legacyLocalTipReader {
	t.Helper()
	r := &legacyLocalTipReader{t: t}
	t.Cleanup(func() {
		if left := r.takeRefused(); len(left) > 0 {
			t.Errorf("the legacy local kit was given repositories that are not local paths: %v", left)
		}
	})
	return r
}

func (r *legacyLocalTipReader) Tip(ctx context.Context, repository, base string) (mergeturn.Tip, error) {
	if !filepath.IsAbs(repository) {
		r.mu.Lock()
		r.refused = append(r.refused, repository)
		r.mu.Unlock()
		return mergeturn.Tip{}, fmt.Errorf("%w: repository %q", errLegacyLocalForgeTarget, repository)
	}
	return mergeturn.TargetReader{}.Tip(ctx, repository, base)
}

// takeRefused returns the non-local repositories refused since the last call, and forgets them: a test that provokes the refusal on purpose takes it here, and the end-of-test check stays quiet.
func (r *legacyLocalTipReader) takeRefused() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.refused
	r.refused = nil
	return out
}

// legacyLocalFakeGH writes a fake gh that records its invocation and answers nothing, puts its directory first on PATH, and returns the log it appends to. The legacy local kit reads only
// the temporary repository its plan lands on, an absolute path; an owner/name repository is the shape whose read the production mergeturn.TargetReader takes to the forge, and this kit
// must refuse it before that call, so a log that exists is the kit reaching gh. No test of this package runs a real gh.
func legacyLocalFakeGH(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "gh-calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$LEGACY_LOCAL_FAKE_GH_LOG\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGACY_LOCAL_FAKE_GH_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// Criterion c1/c2: the legacy local kit's tip reader refuses an owner/name repository without reaching gh, and answers for the local path the kit exists for.
// sequential: t.Setenv("LEGACY_LOCAL_FAKE_GH_LOG") is process-wide.
func TestLegacyLocalKitRefusesAnOwnerNameTarget(t *testing.T) {
	k := newLegacyLocalIntegrationKit(t)
	reader, ok := k.sched.Tips.(*legacyLocalTipReader)
	if !ok {
		t.Fatalf("the legacy local kit's tip reader = %T, want the legacy local guard", k.sched.Tips)
	}

	calls := legacyLocalFakeGH(t)
	_, err := reader.Tip(context.Background(), "owner/repo", "dev")
	if err == nil {
		t.Fatal("the legacy local kit read an owner/name repository: want the guard's refusal")
	}
	if !errors.Is(err, errLegacyLocalForgeTarget) {
		t.Fatalf("the tip reader's error = %v, want the legacy local guard's refusal", err)
	}
	if refused := reader.takeRefused(); len(refused) != 1 || refused[0] != "owner/repo" {
		t.Fatalf("the guard recorded %v, want the one refused owner/name target", refused)
	}
	recorded, err := os.ReadFile(calls)
	if err == nil {
		t.Fatalf("the legacy local kit took an owner/name target to gh, want the guard to refuse it first:\n%s", strings.TrimSpace(string(recorded)))
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	tip, err := reader.Tip(context.Background(), k.repo.path, "dev")
	if err != nil || tip.SHA == "" || tip.Source != "local_git" {
		t.Fatalf("the local path read = %+v %v, want the production local reader's answer", tip, err)
	}
}

// legacyLocalProvokedTB is a testing.TB the guard's end-of-test check can run against without failing the real test: it records the clean-up functions and the errors the check reports. The guard's
// check calls only Helper, Cleanup and Errorf, so the nil embedded TB is never reached.
type legacyLocalProvokedTB struct {
	testing.TB
	mu      sync.Mutex
	cleanup []func()
	failed  []string
}

func (b *legacyLocalProvokedTB) Helper() {}

func (b *legacyLocalProvokedTB) Cleanup(f func()) {
	b.mu.Lock()
	b.cleanup = append(b.cleanup, f)
	b.mu.Unlock()
}

func (b *legacyLocalProvokedTB) Errorf(format string, args ...any) {
	b.mu.Lock()
	b.failed = append(b.failed, fmt.Sprintf(format, args...))
	b.mu.Unlock()
}

// Criterion c1: the guard fails a test that returns its refusal to nobody: the check runs when the test ends, not when the read happens, so a caller that swallows the error (a sweep skips a tip
// it cannot read) cannot leave the test green.
// sequential: t.Setenv("LEGACY_LOCAL_FAKE_GH_LOG") is process-wide.
func TestLegacyLocalGuardFailsATestThatSwallowsTheRefusal(t *testing.T) {
	tb := &legacyLocalProvokedTB{}
	reader := newLegacyLocalTipReader(tb)
	calls := legacyLocalFakeGH(t)

	if _, err := reader.Tip(context.Background(), "owner/repo", "dev"); !errors.Is(err, errLegacyLocalForgeTarget) {
		t.Fatalf("the refused read = %v, want the guard's refusal", err)
	}
	if len(tb.failed) != 0 {
		t.Fatalf("the guard failed the test before it ended: %v", tb.failed)
	}
	for _, check := range tb.cleanup {
		check()
	}
	if len(tb.failed) != 1 || !strings.Contains(tb.failed[0], "owner/repo") {
		t.Fatalf("the guard's end-of-test check reported %v, want the swallowed owner/name refusal", tb.failed)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Fatal("the guard's refusal reached gh")
	}
}
