package migrate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const invOldTemp = ".migrate-ABCDEFGHIJKLMNOPQRSTUVWXYZ-1.tmp"

func invLink(t *testing.T, path, target string) {
	t.Helper()
	mkdirs(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// invRootStamp renders a directory's own type, mode and mtime. The shared fingerprint helper skips the directory it is
// given (publish_test.go), so a test that must prove the directory itself is untouched compares this stamp beside it.
func invRootStamp(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Lstat(path)
	must(t, err)
	return fi.Mode().String() + " " + fi.ModTime().String()
}

func TestClassifyLockRefusals(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		dir   bool
	}{
		{"session lock file", "sessions/rec-1.json.lock", false},
		{"goalplan lock directory without owner", "goalplans/rec-1/.goalplan.lock", true},
		{"dispatch lock directory", "dispatches/rec-1/d-1.json.lock", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws, src, dst := invRowProject(t)
			entries := map[string]string{
				"sessions/rec-1.json":           "{}",
				"goalplans/rec-1/goalplan.json": "{}",
				"dispatches/rec-1/d-1.json":     invDispatch("d-1", "complete", "complete"),
			}
			if c.dir {
				entries[c.entry+"/"] = ""
			} else {
				entries[c.entry] = "12345"
			}
			invTree(t, src, entries)
			invRowRefusal(t, ws, src, dst, ReasonLocked)
		})
	}
	t.Run("codex role update lock", func(t *testing.T) {
		base := isolate(t)
		c := filepath.Join(base, "ec")
		invTree(t, c, map[string]string{"agents/.architect-update.lock": "1", "agents/architect.toml": "role"})
		before := fingerprint(t, c)
		r, err := Open(Options{Scope: ScopeCodex, CodexHome: c})
		must(t, err)
		defer r.Close()
		p, err := classify(r)
		var ref *RefusedError
		if !errors.As(err, &ref) || ref.Reason != ReasonLocked {
			t.Fatalf("classify = %v (%v), want a locked refusal", p, err)
		}
		if fingerprint(t, c) != before {
			t.Error("the lock refusal wrote into the codex home")
		}
	})
}

func TestClassifySubtreeRootRefusals(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, src, ws string)
		want  Reason
	}{
		{"copy root is a link", func(t *testing.T, src, ws string) {
			invTree(t, src, map[string]string{"sessions/rec-1.json": "{}"})
			invLink(t, filepath.Join(src, "plan"), filepath.Join(ws, "elsewhere"))
		}, ReasonLink},
		{"skipped root is a link", func(t *testing.T, src, ws string) {
			invTree(t, src, map[string]string{"sessions/rec-1.json": "{}"})
			invLink(t, filepath.Join(src, "release"), filepath.Join(ws, "elsewhere"))
		}, ReasonLink},
		{"copy root is a file", func(t *testing.T, src, ws string) {
			invTree(t, src, map[string]string{"sessions/rec-1.json": "{}", "plan/rec-1": "not a directory"})
		}, ReasonNotDirectory},
		{"skipped root is a file", func(t *testing.T, src, ws string) {
			invTree(t, src, map[string]string{"sessions/rec-1.json": "{}", "release": "not a directory"})
		}, ReasonNotDirectory},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws, src, dst := invRowProject(t)
			c.build(t, src, ws)
			invRowRefusal(t, ws, src, dst, c.want)
		})
	}
}

func TestClassifyUserPlanTmp(t *testing.T) {
	ws, src, dst := invRowProject(t)
	invTree(t, src, map[string]string{
		"plan/rec-1/draft.tmp": "ordinary plan data",
		"sessions/rec-1.json":  "{}",
		"sessions/rec-1.json.1234.aabbccdd-eeff-0011-2233-445566778899.tmp": "half a write",
		"sessions/" + invOldTemp: "an older run's temporary",
	})
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	if it := invWant(t, p, ScopeProject, "plan/rec-1/draft.tmp", DispCopy); it.Destination != "plan/rec-1/draft.tmp" {
		t.Errorf("a user plan .tmp is ordinary data; got %+v", it)
	}
	if it := invWant(t, p, ScopeProject, "sessions/rec-1.json.1234.aabbccdd-eeff-0011-2233-445566778899.tmp", DispSkip); it.Reason != inventoryReasonIntermediate {
		t.Errorf("producer intermediate reason = %q", it.Reason)
	}
	if it := invWant(t, p, ScopeProject, "sessions/"+invOldTemp, DispSkip); it.Reason != inventoryReasonOldTemp {
		t.Errorf("older-run temporary reason = %q", it.Reason)
	}
	// A temporary of an older run that sits where this run's destination is is reported too, and stays untouched.
	invTree(t, dst, map[string]string{"sessions/" + invOldTemp: "left over"})
	// dst/sessions holds nothing but that temporary, so a fingerprint of the directory is the temporary file's own
	// content, mode and mtime. Captured before the classification and compared after it, it proves the run neither
	// rewrote nor retimed the temporary (the Lstat below only proves it was not removed).
	beforeTemp := fingerprint(t, filepath.Join(dst, "sessions"))
	beforeDestRoot := invRootStamp(t, filepath.Join(dst, "sessions"))
	invTree(t, src, map[string]string{"sessions/rec-2.json": "{}"})
	p2, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	it := invWant(t, p2, ScopeProject, "", DispSkip)
	if it.Destination != "sessions/"+invOldTemp || it.Reason != inventoryReasonOldTemp {
		t.Errorf("destination-side older-run temporary = %+v", it)
	}
	if _, err := os.Lstat(filepath.Join(dst, "sessions", invOldTemp)); err != nil {
		t.Errorf("the older-run temporary was touched: %v", err)
	}
	if after := fingerprint(t, filepath.Join(dst, "sessions")); after != beforeTemp {
		t.Errorf("classification changed the destination temporary:\nbefore: %q\nafter:  %q", beforeTemp, after)
	}
	if after := invRootStamp(t, filepath.Join(dst, "sessions")); after != beforeDestRoot {
		t.Errorf("classification changed the destination directory: before %q, after %q", beforeDestRoot, after)
	}
}

func TestClassifyUnknownChildren(t *testing.T) {
	ws, src, _ := invRowProject(t)
	invTree(t, src, map[string]string{
		"sessions/rec-1.json":         "{}",
		"sessions/extra.txt":          "unknown",
		"evidence-attempts/x.txt":     "unknown",
		"evidence-unrecordable/x.txt": "unknown",
		"divergence/notes.txt":        "unknown",
		"unknown-dir/child.json":      "unknown",
		"notes.txt":                   "unknown",
	})
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	for _, s := range []string{"sessions/extra.txt", "evidence-attempts/x.txt", "evidence-unrecordable/x.txt",
		"divergence/notes.txt", "unknown-dir", "notes.txt"} {
		invWant(t, p, ScopeProject, s, DispSkip)
	}
	for _, it := range p.Items {
		if it.Source == "unknown-dir/child.json" {
			t.Errorf("an unknown directory was followed: %+v", it)
		}
	}
}

func TestClassifyRecords(t *testing.T) {
	cases := []struct {
		name    string
		entries map[string]string
		want    Reason // "" means the plan succeeds and the record is copied
		copied  string
	}{
		{"malformed bg record", map[string]string{"bg/bad.json": "{oops"}, ReasonUnreadable, ""},
		{"bg record of another shape", map[string]string{"bg/bad.json": `{"x":1}`}, ReasonUnreadable, ""},
		{"malformed dispatch record", map[string]string{"dispatches/rec-1/d-1.json": "{oops"}, ReasonUnreadable, ""},
		{"running bg job", map[string]string{"bg/j1.json": invBg("j1", "running")}, ReasonActive, ""},
		{"unknown bg status", map[string]string{"bg/j1.json": invBg("j1", "paused")}, ReasonActive, ""},
		{"terminal bg job", map[string]string{"bg/j1.json": invBg("j1", "failed")}, "", "bg/j1.json"},
		{"active dispatch with terminal attempts", map[string]string{"dispatches/rec-1/d-1.json": invDispatch("d-1", "active", "complete")}, ReasonActive, ""},
		{"dispatch with an in-flight attempt", map[string]string{"dispatches/rec-1/d-1.json": invDispatch("d-1", "complete", "running")}, ReasonActive, ""},
		{"terminal dispatch", map[string]string{"dispatches/rec-1/d-1.json": invDispatch("d-1", "stopped", "failed")}, "", "dispatches/rec-1/d-1.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws, src, dst := invRowProject(t)
			entries := map[string]string{"sessions/rec-1.json": "{}"}
			for k, v := range c.entries {
				entries[k] = v
			}
			invTree(t, src, entries)
			if c.want != "" {
				invRowRefusal(t, ws, src, dst, c.want)
				return
			}
			p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
			must(t, err)
			invWant(t, p, ScopeProject, c.copied, DispCopy)
		})
	}
}

func TestClassifyDestinationConflicts(t *testing.T) {
	record := `{"phase":"B"}`
	t.Run("equal destination is not a refusal", func(t *testing.T) {
		ws, src, dst := invRowProject(t)
		invTree(t, src, map[string]string{"sessions/rec-1.json": record})
		invTree(t, dst, map[string]string{"sessions/rec-1.json": record})
		p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
		must(t, err)
		invWant(t, p, ScopeProject, "sessions/rec-1.json", DispCopy)
	})
	t.Run("differing destination refuses", func(t *testing.T) {
		ws, src, dst := invRowProject(t)
		invTree(t, src, map[string]string{"sessions/rec-1.json": record})
		invTree(t, dst, map[string]string{"sessions/rec-1.json": "other bytes"})
		invRowRefusal(t, ws, src, dst, ReasonDiffers)
	})
	t.Run("destination link refuses", func(t *testing.T) {
		ws, src, dst := invRowProject(t)
		invTree(t, src, map[string]string{"sessions/rec-1.json": record})
		invTree(t, dst, map[string]string{"sessions/keep": "x"})
		invTree(t, dst, map[string]string{"sessions/.keep": "x"})
		invLink(t, filepath.Join(dst, "sessions", "rec-1.json"), filepath.Join(dst, "sessions", ".keep"))
		invRowRefusal(t, ws, src, dst, ReasonLink)
	})
	t.Run("codex destination conflict", func(t *testing.T) {
		base := isolate(t)
		c := filepath.Join(base, "ec")
		invTree(t, c, map[string]string{
			".codexclaw-install.json": "{}",
			".crw-install.json":       "another install record",
			"config.toml":             "cfg",
		})
		before := fingerprint(t, c)
		beforeRoot := invRootStamp(t, c)
		r, err := Open(Options{Scope: ScopeCodex, CodexHome: c})
		must(t, err)
		defer r.Close()
		p, err := classify(r)
		var ref *RefusedError
		if !errors.As(err, &ref) || ref.Reason != ReasonDiffers {
			t.Fatalf("classify = %v (%v), want a differing refusal", p, err)
		}
		if fingerprint(t, c) != before {
			t.Error("the destination conflict refusal wrote into the codex home")
		}
		if after := invRootStamp(t, c); after != beforeRoot {
			t.Errorf("the destination conflict refusal changed the codex home root: before %q, after %q", beforeRoot, after)
		}
	})
}

func TestClassifyUnsupportedName(t *testing.T) {
	ws, src, dst := invRowProject(t)
	invTree(t, src, map[string]string{"sessions/bad\nname.json": "{}"})
	invRowRefusal(t, ws, src, dst, ReasonUnsupported)
}

func TestClassifyDeterminism(t *testing.T) {
	ws, src, _ := invRowProject(t)
	invTree(t, src, invProjectEntries())
	first, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	second, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	if !reflect.DeepEqual(first.Items, second.Items) {
		t.Error("two classifications of one tree differ")
	}
	order := map[Scope]int{ScopeProject: 0, ScopeUser: 1, ScopeCodex: 2}
	last := -1
	for _, it := range first.Items {
		if order[it.Scope] < last {
			t.Fatalf("items are not grouped in scope order at %+v", it)
		}
		last = order[it.Scope]
	}
}

func TestClassifyAllScope(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), map[string]string{"sessions/rec-1.json": "{}"})
	invTree(t, filepath.Join(base, "eu"), map[string]string{"subagents.json": "{}"})
	mkdirs(t, filepath.Join(base, "ev"))
	invTree(t, filepath.Join(base, "ec"), map[string]string{"config.toml": "cfg"})
	p, err := invClassify(t, Options{Scope: ScopeAll, Cwd: ws, FromHome: filepath.Join(base, "eu"),
		ToHome: filepath.Join(base, "ev"), CodexHome: filepath.Join(base, "ec")})
	must(t, err)
	invWant(t, p, ScopeProject, "sessions/rec-1.json", DispCopy)
	invWant(t, p, ScopeUser, "subagents.json", DispCopy)
	invWant(t, p, ScopeCodex, "config.toml", DispSkip)
}

// TestClassifyAllScopeOrder proves the selected scope is classified in the fixed order project -> user -> codex, with
// each root's container item first, by asserting the whole ordered item sequence of a ScopeAll walk. TestClassifyAllScope
// only checks inclusion, and TestClassifyDeterminism only checks that no scope appears out of group order, so neither
// pins the order of the roots' own items.
func TestClassifyAllScopeOrder(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), map[string]string{"sessions/rec-1.json": "{}"})
	invTree(t, filepath.Join(base, "eu"), map[string]string{"subagents.json": "{}"})
	mkdirs(t, filepath.Join(base, "ev"))
	invTree(t, filepath.Join(base, "ec"), map[string]string{"config.toml": "cfg"})
	p, err := invClassify(t, Options{Scope: ScopeAll, Cwd: ws, FromHome: filepath.Join(base, "eu"),
		ToHome: filepath.Join(base, "ev"), CodexHome: filepath.Join(base, "ec")})
	must(t, err)
	// The project root and the user root each emit their container item first; the project "sessions" directory is a
	// traversed container, so it is reported before its child; the Codex root maps only a leaf, so it has no container.
	want := []string{
		"project:.", "project:sessions", "project:sessions/rec-1.json",
		"user:.", "user:subagents.json",
		"codex:config.toml",
	}
	if got := invSources(p); !reflect.DeepEqual(got, want) {
		t.Errorf("all-scope item order = %v, want %v", got, want)
	}
}
