package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1372 (4), evaluation d1 of CRW-1380 a49bcbbc: two near-simultaneous `crw flow start`s of session runs for one owner thread
// cannot both bind. A start binds the owner before its run process takes the run lock, so the second start comes in the window where
// the first run's process holds nothing yet; it must still find the first run live and not bind. Then each run's process binds under
// its own run lock: the first finds its own line, the second finds the first run live and is not bound either.
func TestTwoNearSimultaneousSessionStartsOfOneOwnerBindOne(t *testing.T) {
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
	var second struct {
		code        int
		out, errOut string
	}
	nested := false
	spawn := td.Spawn
	td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		if !nested {
			nested = true
			// the first start has bound its owner and its process has not taken its run lock: the second start comes now
			var out, errOut bytes.Buffer
			second.code = RunWith(ctx, td.Deps, []string{"start", script, "--thread", sessOwner, "--state", w.state, "--json"}, strings.NewReader(""), &out, &errOut)
			second.out, second.errOut = out.String(), errOut.String()
		}
		return spawn(ctx, req)
	}
	code, out, errOut := td.do("start", script, "--thread", sessOwner, "--state", w.state, "--json")
	if code != 0 || second.code != 0 || len(td.spawned) != 2 {
		t.Fatalf("the starts: %d %s %s / %d %s %s", code, out, errOut, second.code, second.out, second.errOut)
	}
	var first, other StartAnswer
	if err := json.Unmarshal([]byte(answerOf(out)), &first); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(answerOf(second.out)), &other); err != nil {
		t.Fatalf("%v\n%s", err, second.out)
	}
	if first.Run == "" || other.Run == "" || first.Run == other.Run {
		t.Fatalf("the runs: %q %q", first.Run, other.Run)
	}
	if first.Bound == other.Bound {
		t.Fatalf("both or neither of two near-simultaneous starts bound the owner: first %+v / second %+v (%s)", first.Bound, other.Bound, second.errOut)
	}
	if !first.Bound || other.Bound || !strings.Contains(other.BindWarning, "one thread binds one live run") {
		t.Fatalf("the first start keeps the owner, the second warns: first bound=%v / second bound=%v warning %q", first.Bound, other.Bound, other.BindWarning)
	}
	if got := bound.Lookup(sessOwner).Line; got.RunID != first.Run || got.Kind != bound.KindOrchestrator {
		t.Fatalf("the owner's line after both starts: %+v, want the first run %s", got, first.Run)
	}
	// both run processes take their run locks and bind as the process does: one binding stands
	for _, run := range []string{first.Run, other.Run} {
		lock, err := TakeLock(w.state, run, "_run", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Release()
	}
	table := engine.FakePolicy()
	resB, err := bindOwner(w.state, other.Run, sessOwner, table)
	if err != nil || resB.Bound || resB.Holder != first.Run {
		t.Fatalf("the second run's process bind: %+v %v, want the first run %s holding", resB, err, first.Run)
	}
	resA, err := bindOwner(w.state, first.Run, sessOwner, table)
	if err != nil || resA.Holder != "" || !(resA.Bound || resA.Already) {
		t.Fatalf("the first run's process bind: %+v %v", resA, err)
	}
	if got := bound.Lookup(sessOwner).Line; got.RunID != first.Run {
		t.Fatalf("the owner's line after both processes bound: %+v, want the first run %s", got, first.Run)
	}
}
