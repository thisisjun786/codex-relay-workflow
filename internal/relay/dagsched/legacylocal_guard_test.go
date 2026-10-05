package dagsched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// errLegacyLocalForgeTarget is the legacy local kit's refusal of a repository that is not an absolute path: the production mergeturn.TargetReader would take an owner/name target to the forge,
// and this kit reads only the temporary repository its plan lands on.
var errLegacyLocalForgeTarget = errors.New("the legacy local kit reads only an absolute local path; an owner/name repository would take the production reader to the forge, so it is refused")

// legacyLocalTipReader is the legacy local kit's tip reader: the production local reader behind that guard. An absolute path is the world the kit exists for and goes to mergeturn.TargetReader
// unchanged; anything else is refused here, before the production reader can reach gh, and the error fails the test that surfaced it.
type legacyLocalTipReader struct{}

func (legacyLocalTipReader) Tip(ctx context.Context, repository, base string) (mergeturn.Tip, error) {
	if !filepath.IsAbs(repository) {
		return mergeturn.Tip{}, fmt.Errorf("%w: repository %q", errLegacyLocalForgeTarget, repository)
	}
	return mergeturn.TargetReader{}.Tip(ctx, repository, base)
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
func TestLegacyLocalKitRefusesAnOwnerNameTarget(t *testing.T) {
	k := newLegacyLocalIntegrationKit(t)

	calls := legacyLocalFakeGH(t)
	_, err := k.sched.Tips.Tip(context.Background(), "owner/repo", "dev")
	if err == nil {
		t.Fatal("the legacy local kit read an owner/name repository: want the guard's refusal")
	}
	if !errors.Is(err, errLegacyLocalForgeTarget) {
		t.Fatalf("the tip reader's error = %v, want the legacy local guard's refusal", err)
	}
	recorded, err := os.ReadFile(calls)
	if err == nil {
		t.Fatalf("the legacy local kit took an owner/name target to gh, want the guard to refuse it first:\n%s", strings.TrimSpace(string(recorded)))
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	tip, err := k.sched.Tips.Tip(context.Background(), k.repo.path, "dev")
	if err != nil || tip.SHA == "" || tip.Source != "local_git" {
		t.Fatalf("the local path read = %+v %v, want the production local reader's answer", tip, err)
	}
}
