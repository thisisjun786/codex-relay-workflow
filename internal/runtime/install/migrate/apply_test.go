package migrate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

var errApplyInterrupted = errors.New("interrupted")

// apPlan classifies a project scope whose source holds entries, after an optional setup hook, and returns the worktree,
// the roots and the plan. Every environment root is a temporary directory.
func apPlan(t *testing.T, entries map[string]string, setup func(src string)) (string, *Roots, *Plan) {
	t.Helper()
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	src := filepath.Join(ws, ProjectSourceName)
	invTree(t, src, entries)
	if setup != nil {
		setup(src)
	}
	r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	return ws, r, p
}

func apDst(ws, rel string) string {
	return filepath.Join(ws, crwdir.DirName, filepath.FromSlash(rel))
}

// apReadOnlySource makes a source directory read-only, and restores its mode so the temporary tree can be removed.
func apReadOnlySource(t *testing.T, rel string) func(string) {
	return func(src string) {
		dir := filepath.Join(src, filepath.FromSlash(rel))
		must(t, os.Chmod(dir, 0o555))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}
}

// apPrivateSource makes a source directory private (0700), and restores its mode so the temporary tree can be removed.
func apPrivateSource(t *testing.T, rel string) func(string) {
	return func(src string) {
		dir := filepath.Join(src, filepath.FromSlash(rel))
		must(t, os.Chmod(dir, 0o700))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}
}

// apFailRename fails the n-th no-replace rename: the first publishes the canonical .gitignore, the next ones the files.
func apFailRename(n int) func(string) error {
	seen := 0
	return func(step string) error {
		if step == "rename" {
			seen++
			if seen == n {
				return errApplyInterrupted
			}
		}
		return nil
	}
}

func apItem(t *testing.T, res *ApplyResult, source string) ApplyItem {
	t.Helper()
	for _, ai := range res.Items {
		if ai.Source == source {
			return ai
		}
	}
	t.Fatalf("no item for %q", source)
	return ApplyItem{}
}

func TestApplyCopiesAndARerunSkips(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}", "plan/s/x.md": "plan bytes"}, nil)
	res, err := apply(r, p)
	must(t, err)
	if !res.SourceVerified || res.WritesCompleted != 2 {
		t.Fatalf("first run: verified=%v writes=%d, want true and 2", res.SourceVerified, res.WritesCompleted)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("session bytes = %q", got)
	}
	if got := get(t, apDst(ws, "plan/s/x.md")); got != "plan bytes" {
		t.Errorf("plan bytes = %q", got)
	}
	again, err := apply(r, p)
	must(t, err)
	if again.WritesCompleted != 0 || !again.SourceVerified {
		t.Errorf("rerun: writes=%d verified=%v, want 0 and true", again.WritesCompleted, again.SourceVerified)
	}
	for _, ai := range again.Items {
		if applyWrites(ai.Item) && !applyDir(ai.Item) && ai.Result != ResultAlreadyEqual {
			t.Errorf("%s: %q, want already-equal", ai.Source, ai.Result)
		}
	}
}

func TestApplyRefusesAChangedSource(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/rec-1.json": "{\"phase\":\"P\"}"}, nil)
	put(t, filepath.Join(ws, ProjectSourceName, "sessions", "rec-1.json"), "{\"phase\":\"BB\"}", 0o644)
	res, err := apply(r, p)
	wantRefusal(t, err, applyReasonChanged)
	if res.SourceVerified {
		t.Error("a stopped run reported a verified source")
	}
	if _, err := os.Lstat(apDst(ws, "sessions/rec-1.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the destination must not exist: %v", err)
	}
}

func TestApplyInterruptionThenRerun(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}", "sessions/b.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	pub.at = apFailRename(3) // 1 the canonical .gitignore, 2 the first file, 3 the second
	res, err := applyWith(r, p, pub)
	if !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("interrupted run: %v", err)
	}
	if res.WritesCompleted != 1 || res.SourceVerified {
		t.Errorf("interrupted run: writes=%d verified=%v, want 1 and false", res.WritesCompleted, res.SourceVerified)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the completed file must be whole: %q", got)
	}
	if _, err := os.Lstat(apDst(ws, "sessions/b.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the second file must be absent: %v", err)
	}
	again, err := apply(r, p)
	must(t, err)
	if again.WritesCompleted != 1 || !again.SourceVerified {
		t.Errorf("rerun: writes=%d verified=%v, want 1 and true", again.WritesCompleted, again.SourceVerified)
	}
	if got := get(t, apDst(ws, "sessions/b.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the rerun must finish the copy: %q", got)
	}
}

func TestApplyWritesReferencesLast(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{
		"ledger.jsonl":          "{\"event\":\"created\"}",
		"sessions/a.json":       "{\"phase\":\"P\"}",
		"plan/s/x.md":           "plan bytes",
		"interview/freeze.json": "{\"planFiles\":[{\"path\":\"x.md\"}]}",
	}, nil)
	pub := newPub(t)
	pub.at = apFailRename(3) // .gitignore, then the plan artifact, then the freeze that names it
	if _, err := applyWith(r, p, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("interrupted run: %v", err)
	}
	if _, err := os.Lstat(apDst(ws, "plan/s/x.md")); err != nil {
		t.Errorf("the plan artifact must be published first: %v", err)
	}
	for _, rel := range []string{"interview/freeze.json", "ledger.jsonl", "sessions/a.json"} {
		if _, err := os.Lstat(apDst(ws, rel)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s must not be published before the plan artifact: %v", rel, err)
		}
	}
}

func TestApplyRankTable(t *testing.T) {
	cases := []struct {
		scope Scope
		src   string
		want  int
	}{
		{ScopeProject, "plan", 0}, {ScopeProject, "plan/s/x.md", 0}, {ScopeProject, "evidence", 0},
		{ScopeProject, "evidence/rec-1/test-receipt.json", 0}, {ScopeProject, "interview/freeze.json", 1},
		{ScopeProject, "sessions", 2}, {ScopeProject, "sessions/a.json", 2}, {ScopeProject, "ledger.jsonl", 2},
		{ScopeProject, "interviews/rec-1.jsonl", 2}, {ScopeProject, "goalplans", 2},
		{ScopeProject, "goalplans/s/goalplan.json", 2}, {ScopeProject, "goalplans/s/ledger.jsonl", 2},
		{ScopeProject, "goalplans/s/schema-v2.marker", 2}, {ScopeProject, "sources/rec-1.json", 2},
		{ScopeProject, "dispatches/rec-1/d.json", 2}, {ScopeProject, "bg/j.json", 2},
		{ScopeProject, "bg/disabled", 2}, {ScopeProject, "bg/enabled-at", 2}, {ScopeProject, "bg/ledger.jsonl", 2},
		{ScopeProject, "attest.json", 2}, {ScopeProject, "subagents.json", 2},
		{ScopeProject, "evidence-attempts/rec-1-a.json", 1}, {ScopeProject, "evidence-unrecordable/rec-1-a-1.json", 1},
		{ScopeProject, "metrics.jsonl", 1}, {ScopeProject, "objective-kind/rec-1.json", 1},
		{ScopeProject, "divergence/rec-1.mode.json", 1}, {ScopeProject, "divergence/candidates.jsonl", 1},
		{ScopeProject, "render-observations.jsonl", 1}, {ScopeProject, "bg/j.out", 1}, {ScopeProject, "bg/j.exit", 1},
		{ScopeUser, "subagents.json", 2}, {ScopeUser, "config.json", 2},
		{ScopeCodex, installSource, 0}, {ScopeCodex, selfHealSource, 0}, {ScopeCodex, "config.toml.codexclaw-2026.bak", 0},
	}
	for _, c := range cases {
		if got := applyRank(Item{Scope: c.scope, Source: c.src}); got != c.want {
			t.Errorf("applyRank(%s %q) = %d, want %d", c.scope, c.src, got, c.want)
		}
	}
}

// The rename step runs before the rename and the directory sync after it, so the destination exists by then.
func TestApplyRechecksThePublishedFile(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, path string){
		"mode":  func(t *testing.T, path string) { _ = os.Chmod(path, 0o600) },
		"bytes": func(t *testing.T, path string) { put(t, path, "changed bytes", 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
			pub := newPub(t)
			seen := 0
			pub.at = func(step string) error {
				if step == "dirsync" {
					if seen++; seen == 2 {
						damage(t, apDst(ws, "sessions/a.json"))
					}
				}
				return nil
			}
			if _, err := applyWith(r, p, pub); err == nil {
				t.Fatal("a published file that changed must fail the run")
			}
		})
	}
}

func TestApplyKeepsTheDestinationModeOfAnEqualFile(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	put(t, apDst(ws, "sessions/a.json"), "{\"phase\":\"P\"}", 0o600)
	res, err := apply(r, p)
	must(t, err)
	if res.WritesCompleted != 0 {
		t.Errorf("an equal destination is not a write: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); ai.Result != ResultAlreadyEqual || ai.Note == "" {
		t.Errorf("item = %q note %q, want already-equal and a note", ai.Result, ai.Note)
	}
	if fi, err := os.Stat(apDst(ws, "sessions/a.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("equality must not change the destination mode: %v %v", fi, err)
	}
}

func TestApplyFinishesDirectoryModesAfterAnInterruption(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"plan/s/x.md": "plan bytes"}, apReadOnlySource(t, "plan/s"))
	// The run finishes the destination directory at the source's read-only mode, so restore it before the tree is removed.
	t.Cleanup(func() { _ = os.Chmod(apDst(ws, "plan/s"), 0o755) })
	pub := newPub(t)
	pub.at = apFailRename(2) // the canonical .gitignore, then the file: the directories keep the temporary mode
	if _, err := applyWith(r, p, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("interrupted run: %v", err)
	}
	if fi, err := os.Stat(apDst(ws, "plan/s")); err != nil || fi.Mode().Perm() != applyTempMode || fi.Mode()&fs.ModeSticky == 0 {
		t.Fatalf("the interrupted run must leave the private marker mode: %v %v", fi, err)
	}
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(apDst(ws, "plan/s")); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("the rerun must finish the mode: %v %v", fi, err)
	}
}

func TestApplyKeepsTheModeOfAForeignDirectory(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"plan/s/x.md": "plan bytes", "sessions/a.json": "{\"phase\":\"P\"}"}, apReadOnlySource(t, "plan/s"))
	// Two directories this migration did not create: one at 0755, one private at 0700 holding only plan-named entries.
	// Neither carries the migration's private marker mode, so neither may be chmodded.
	mkdirs(t, apDst(ws, "plan/s"))
	mkdirs(t, apDst(ws, "sessions"))
	must(t, os.Chmod(apDst(ws, "sessions"), 0o700))
	res, err := apply(r, p)
	must(t, err)
	for _, rel := range []string{"plan/s", "sessions"} {
		if fi, err := os.Stat(apDst(ws, rel)); err != nil || fi.Mode().Perm() != 0o700 && rel == "sessions" || rel == "plan/s" && (err != nil || fi.Mode().Perm() != 0o755) {
			t.Errorf("%s must keep its mode: %v %v", rel, fi, err)
		}
		if ai := apItem(t, res, rel); !strings.Contains(ai.Note, "kept its mode") {
			t.Errorf("%s must be reported, note = %q", rel, ai.Note)
		}
	}
}

func TestApplyFinishesTheRootModeAfterAnInterruption(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	pub.at = apFailRename(2)
	if _, err := applyWith(r, p, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("interrupted run: %v", err)
	}
	root := filepath.Join(ws, crwdir.DirName)
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != applyTempMode {
		t.Fatalf("the interrupted run must leave the root at the temporary mode: %v %v", fi, err)
	}
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the rerun must finish the root mode: %v %v", fi, err)
	}
}

func TestApplyKeepsUnknownFieldsAndNonUTF8Bytes(t *testing.T) {
	body := "{\"phase\":\"P\",\"unknown\":[1,2],\"note\":\"" + string([]byte{0xff, 0xfe}) + "\"}"
	ws, r, p := apPlan(t, map[string]string{"sessions/rec-1.json": body}, nil)
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	if got := get(t, apDst(ws, "sessions/rec-1.json")); got != body {
		t.Errorf("the copied bytes changed: %q", got)
	}
}

func TestApplyKeepsTheInstallManifestConsentAndBackup(t *testing.T) {
	base := isolate(t)
	home := filepath.Join(base, "codex")
	mkdirs(t, home)
	body := "{\"version\":2,\"configPath\":\"/c/config.toml\",\"backupPath\":\"/c/config.toml.codexclaw-2026.bak\",\"tableKeys\":{\"memories.dedicated_tools\":{\"table\":\"memories\",\"key\":\"dedicated_tools\",\"priorValue\":\"false\",\"appliedValue\":\"true\",\"setByCodexclaw\":true}}}"
	put(t, filepath.Join(home, installSource), body, 0o644)
	r, err := Open(Options{Scope: ScopeCodex, CodexHome: home})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	if got := get(t, filepath.Join(home, installDest)); got != body {
		t.Errorf("the install manifest changed: %q", got)
	}
}

func TestApplyNeverStartsOnARefusedPlan(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), map[string]string{
		"bg/j1.json":                invBg("j1", "running"),
		"dispatches/rec-1/d-1.json": invDispatch("d-1", "active", "claimed"),
	})
	r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	if _, err := classify(r); err == nil {
		t.Fatal("a running job or an active dispatch must refuse the scope")
	}
	if _, err := os.Stat(filepath.Join(ws, crwdir.DirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused plan must write nothing: %v", err)
	}
}
func TestApplyWithANilPlanWritesNothing(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	res, err := apply(r, nil)
	if err != nil || res.WritesCompleted != 0 || len(res.Items) != 0 {
		t.Fatalf("a nil plan must be a no-op: %v %v", res, err)
	}
}

func TestApplyRefusesAChangedSourceRoot(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	src := filepath.Join(ws, ProjectSourceName)
	must(t, os.Chmod(src, 0o700))
	t.Cleanup(func() { _ = os.Chmod(src, 0o755) })
	if _, err := apply(r, p); err == nil {
		t.Fatal("a source root whose mode changed must stop the run")
	}
}

func TestApplyCountsAWholeFileWhoseDirectorySyncFailed(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	seen := 0
	pub.at = func(step string) error {
		if step == "dirsync" {
			if seen++; seen == 2 {
				return errApplyInterrupted
			}
		}
		return nil
	}
	res, err := applyWith(r, p, pub)
	if err == nil {
		t.Fatal("a failed directory sync must stop the run")
	}
	if res.WritesCompleted != 1 {
		t.Errorf("the whole final file must be counted: %d", res.WritesCompleted)
	}
	if get(t, apDst(ws, "sessions/a.json")) != "{\"phase\":\"P\"}" {
		t.Error("the final file must be whole")
	}
}

// A directory this run left at the private marker mode must be finished even when the source mode is itself private, so
// the sticky marker never survives a rerun.
func TestApplyClearsTheMarkerOfAPrivateDirectoryAfterAnInterruption(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, apPrivateSource(t, "sessions"))
	t.Cleanup(func() { _ = os.Chmod(apDst(ws, "sessions"), 0o755) })
	pub := newPub(t)
	pub.at = apFailRename(2)
	if _, err := applyWith(r, p, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("interrupted run: %v", err)
	}
	if fi, err := os.Stat(apDst(ws, "sessions")); err != nil || fi.Mode()&fs.ModeSticky == 0 {
		t.Fatalf("the interrupted run must leave the marker mode: %v %v", fi, err)
	}
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(apDst(ws, "sessions"))
	if err != nil || fi.Mode().Perm() != 0o700 || fi.Mode()&fs.ModeSticky != 0 {
		t.Errorf("the rerun must clear the marker: %v %v", fi, err)
	}
}

// The same case for a scope root whose source is private, which is common.
func TestApplyFinishesTheRootModeOfAPrivateSource(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, apPrivateSource(t, ""))
	root := filepath.Join(ws, crwdir.DirName)
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	pub := newPub(t)
	pub.at = apFailRename(2)
	if _, err := applyWith(r, p, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("interrupted run: %v", err)
	}
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(root)
	if err != nil || fi.Mode().Perm() != 0o700 || fi.Mode()&fs.ModeSticky != 0 {
		t.Errorf("the rerun must clear the root marker: %v %v", fi, err)
	}
}
