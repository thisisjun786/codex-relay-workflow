package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1372 (4), Argon V5 F3 of 4d293b7b: the owner_bound fact of a start's preview reads a run live as the binding does (runLive: its
// process holds the run lock or its start holds the start lock). A second start, or a preview, inside the first start's window, where
// the first run's line is written and its process holds nothing yet, warns that the thread binds that live run: it does not read pass
// while the start that follows is not bound.

func ownerBoundFact(t *testing.T, out string) Fact {
	t.Helper()
	var ans StartAnswer
	if err := json.Unmarshal([]byte(answerOf(out)), &ans); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range ans.Plan.Facts {
		if f.Name == FactOwnerBound {
			return f
		}
	}
	t.Fatalf("no %s fact in %+v", FactOwnerBound, ans.Plan.Facts)
	return Fact{}
}

func TestOwnerBoundFactInTheFirstStartsWindowWarnsAsTheBindingRefuses(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	td := w.deps()
	script := helloScript(t)
	cfg := filepath.Join(t.TempDir(), "codex")
	_ = os.MkdirAll(cfg, 0o700)
	_ = os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("[agents]\nmax_threads = 4\n"), 0o600)
	td.Getenv = td.w.withTemp(func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return cfg, true
		}
		return "", false
	})
	var preview, second struct {
		code        int
		out, errOut string
	}
	nested := false
	spawn := td.Spawn
	td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		if !nested {
			nested = true
			// the first start holds its start lock and has bound its owner; its process holds no run lock yet
			var out, errOut bytes.Buffer
			preview.code = RunWith(ctx, td.Deps, []string{"start", script, "--thread", sessOwner, "--state", w.state, "--preview", "--json"}, strings.NewReader(""), &out, &errOut)
			preview.out, preview.errOut = out.String(), errOut.String()
			out.Reset()
			errOut.Reset()
			second.code = RunWith(ctx, td.Deps, []string{"start", script, "--thread", sessOwner, "--state", w.state, "--json"}, strings.NewReader(""), &out, &errOut)
			second.out, second.errOut = out.String(), errOut.String()
		}
		return spawn(ctx, req)
	}
	code, out, errOut := td.do("start", script, "--thread", sessOwner, "--state", w.state, "--json")
	if code != 0 || preview.code != 0 || second.code != 0 {
		t.Fatalf("the starts: %d %s %s / preview %d %s / %d %s %s", code, out, errOut, preview.code, preview.errOut, second.code, second.out, second.errOut)
	}
	var first, other StartAnswer
	if err := json.Unmarshal([]byte(answerOf(out)), &first); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(answerOf(second.out)), &other); err != nil {
		t.Fatalf("%v\n%s", err, second.out)
	}
	if !first.Bound || other.Bound {
		t.Fatalf("the first start keeps the owner and the second is not bound: first %v second %v", first.Bound, other.Bound)
	}
	for name, got := range map[string]Fact{"the preview": ownerBoundFact(t, preview.out), "the second start": ownerBoundFact(t, second.out)} {
		if got.Level != LevelWarn || !strings.Contains(got.Reading, first.Run) {
			t.Fatalf("%s reads owner_bound %+v while run %s is live and the second start is not bound (%q)", name, got, first.Run, other.BindWarning)
		}
	}
}
