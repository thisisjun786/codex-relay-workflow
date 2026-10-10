package evidence

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1110 post-evaluation round (P0 d1): the verdicts a recovery moves beside the main list are the only copies once the shortened
// list is published, so the overflow files, and the directories that hold them, must be on stable storage before that publication,
// and a failure to get them there leaves the main list as it is.

// durableTrace records the syncs and the main-list publication of one RecoverOverflow in order.
type durableTrace struct{ events []string }

func (d *durableTrace) install(t *testing.T, failAt string) {
	t.Helper()
	oldFile, oldDir := syncFile, syncDirectory
	t.Cleanup(func() { syncFile, syncDirectory = oldFile, oldDir })
	syncFile = func(f *os.File) error {
		d.events = append(d.events, "file:"+filepath.Base(f.Name()))
		if failAt == "file" {
			return errors.New("injected file sync failure")
		}
		return f.Sync()
	}
	syncDirectory = func(dir string) error {
		d.events = append(d.events, "dir:"+dir)
		if failAt == "dir" || failAt == "dir:"+dir {
			return errors.New("injected directory sync failure")
		}
		return oldDir(dir)
	}
}

func seed65(t *testing.T) (cwd string, before []byte) {
	t.Helper()
	cwd = t.TempDir()
	before = rewriteGuardSeed(t, cwd, rewriteGuardRecords(65, nil))
	return cwd, before
}

func TestRecoverOverflowSyncsTheOverflowBeforeTheMainListShrinks(t *testing.T) {
	cwd, _ := seed65(t)
	var trace durableTrace
	trace.install(t, "")
	write := func(dir string, s state.State) error {
		trace.events = append(trace.events, "main")
		return state.WriteState(dir, s)
	}
	must(t, RecoverOverflow(cwd, "s1", write))
	main := slices.Index(trace.events, "main")
	if main < 0 {
		t.Fatalf("the main list was not rewritten: %v", trace.events)
	}
	before := trace.events[:main]
	wantDirs := []string{
		overflowDir(cwd, "s1"),
		filepath.Join(cwd, ".crw", OverflowSubdir),
		filepath.Join(cwd, ".crw"),
		cwd,
	}
	for _, dir := range wantDirs {
		if !slices.Contains(before, "dir:"+dir) {
			t.Errorf("%s was not synced before the main list was rewritten: %v", dir, trace.events)
		}
	}
	if !slices.ContainsFunc(before, func(e string) bool { return strings.HasPrefix(e, "file:") && strings.HasSuffix(e, ".tmp") }) {
		t.Errorf("the overflow record's data was not synced before the main list was rewritten: %v", trace.events)
	}
}

func TestRecoverOverflowKeepsTheMainListWhenTheOverflowCannotBeSynced(t *testing.T) {
	for _, failAt := range []string{"file", "dir"} {
		t.Run(failAt, func(t *testing.T) {
			cwd, before := seed65(t)
			var trace durableTrace
			trace.install(t, failAt)
			wrote := false
			err := RecoverOverflow(cwd, "s1", func(dir string, s state.State) error { wrote = true; return state.WriteState(dir, s) })
			if err == nil || wrote {
				t.Fatalf("the main list was shortened although the overflow was not durable: err=%v wrote=%v", err, wrote)
			}
			if after := rewriteGuardFile(t, cwd); !bytes.Equal(after, before) {
				t.Fatal("the main list changed")
			}
		})
	}
}

// CRW-1110 verification round 3: a sync that fails at any one directory on the way to the overflow record (the session directory,
// evidence-overflow, .crw or the workspace) is returned, and the main list keeps its 65 entries.
func TestRecoverOverflowKeepsTheMainListWhenAnyAncestorCannotBeSynced(t *testing.T) {
	for _, step := range []string{"session dir", "overflow dir", "crw dir", "workspace"} {
		t.Run(step, func(t *testing.T) {
			cwd, before := seed65(t)
			dir := map[string]string{
				"session dir":  overflowDir(cwd, "s1"),
				"overflow dir": filepath.Join(cwd, ".crw", OverflowSubdir),
				"crw dir":      filepath.Join(cwd, ".crw"),
				"workspace":    cwd,
			}[step]
			var trace durableTrace
			trace.install(t, "dir:"+dir)
			wrote := false
			err := RecoverOverflow(cwd, "s1", func(dir string, s state.State) error { wrote = true; return state.WriteState(dir, s) })
			if !slices.Contains(trace.events, "dir:"+dir) {
				t.Fatalf("%s was never synced: %v", dir, trace.events)
			}
			if err == nil || wrote {
				t.Fatalf("the main list was shortened although %s could not be synced: err=%v wrote=%v", dir, err, wrote)
			}
			if after := rewriteGuardFile(t, cwd); !bytes.Equal(after, before) {
				t.Fatal("the main list changed")
			}
		})
	}
}

// A 65th verdict recorded by the gate itself goes through the same durable write.
func TestRecordTombstoneBesideTheCapIsSynced(t *testing.T) {
	cwd := t.TempDir()
	rewriteGuardSeed(t, cwd, rewriteGuardRecords(64, nil))
	var trace durableTrace
	trace.install(t, "")
	if !RecordTombstone(cwd, "s1", agent("new", "t1"), MaxAttempts, WriteUnrecordableMarker) {
		t.Fatal("not recorded")
	}
	if !slices.Contains(trace.events, "dir:"+overflowDir(cwd, "s1")) || !slices.Contains(trace.events, "dir:"+filepath.Join(cwd, ".crw", OverflowSubdir)) {
		t.Fatalf("the overflow directories were not synced: %v", trace.events)
	}
}

// CRW-1112 post-evaluation round (P0 d1): a link anywhere in the chain .crw/evidence-overflow/<session dir> is refused by the readers
// and by the removal, as writeOverflow refuses it, so a record of another directory is neither read nor unlinked.
func TestOverflowChainLinksAreRefused(t *testing.T) {
	for _, linked := range []string{"overflow dir", "session dir", "crw dir"} {
		t.Run(linked, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			// A real overflow store with one record, as the external backup of the same session.
			must(t, writeOverflowAt(outside, "s1", "a1", "t1"))
			external := filepath.Join(outside, ".crw", OverflowSubdir, sessionRecordDir("s1"), tupleDigest("a1", "t1")+".json")
			if _, err := os.Stat(external); err != nil {
				t.Fatal(err)
			}
			switch linked {
			case "overflow dir":
				must(t, os.MkdirAll(filepath.Join(cwd, ".crw"), 0o755))
				must(t, os.Symlink(filepath.Join(outside, ".crw", OverflowSubdir), filepath.Join(cwd, ".crw", OverflowSubdir)))
			case "session dir":
				must(t, os.MkdirAll(filepath.Join(cwd, ".crw", OverflowSubdir), 0o755))
				must(t, os.Symlink(filepath.Join(outside, ".crw", OverflowSubdir, sessionRecordDir("s1")), filepath.Join(cwd, ".crw", OverflowSubdir, sessionRecordDir("s1"))))
			case "crw dir":
				must(t, os.Symlink(filepath.Join(outside, ".crw"), filepath.Join(cwd, ".crw")))
			}
			if got, unreadable := OverflowVerdicts(cwd, "s1"); len(got) != 0 || !unreadable {
				t.Errorf("a linked chain was read: %+v unreadable=%v", got, unreadable)
			}
			if HasTombstone(cwd, "s1", agent("a1", "t1")) {
				t.Error("a verdict behind a link counts as recorded")
			}
			if ResolveOverflowVerdict(cwd, "s1", "a1", "t1") || ResolveTombstone(cwd, "s1", agent("a1", "t1")) {
				t.Error("a verdict behind a link was resolved")
			}
			if _, err := os.Stat(external); err != nil {
				t.Fatalf("the external record was removed: %v", err)
			}
		})
	}
}

// writeOverflowAt records one verdict beside the main list of a store rooted at cwd.
func writeOverflowAt(cwd, session, agentID, turn string) error {
	return writeOverflow(cwd, session, state.UnverifiedSubagent{AgentID: agentID, TurnID: turn, AgentType: "executor", Attempts: 3,
		RecordedAt: "2026-10-10T00:00:00.000Z", Resolvable: true})
}
