package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// Criterion c1/c2: the local kit's tip reader refuses an owner/name repository without reaching gh, and answers for the local path the kit exists for.
func TestLegacyLocalKitRefusesAnOwnerNameTarget(t *testing.T) {
	k := newIntegrationKit(t)

	calls := legacyLocalFakeGH(t)
	if _, err := k.sched.Tips.Tip(context.Background(), "owner/repo", "dev"); err == nil {
		t.Fatal("the local kit read an owner/name repository: want a refusal")
	}
	recorded, err := os.ReadFile(calls)
	if err == nil {
		t.Fatalf("the local kit took an owner/name target to gh, want a refusal before the production reader:\n%s", strings.TrimSpace(string(recorded)))
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	tip, err := k.sched.Tips.Tip(context.Background(), k.repo.path, "dev")
	if err != nil || tip.SHA == "" || tip.Source != "local_git" {
		t.Fatalf("the local path read = %+v %v, want the production local reader's answer", tip, err)
	}
}
