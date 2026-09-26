package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// pythonDispositions is testdata/python_dispositions.json (gen_dispositions.py): the Python
// CLI's exit and stdout for every step of every store-only test_dispositions fixture.
var pythonDispositions = sync.OnceValues(func() (map[string]map[string]struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
}, error) {
	raw, err := os.ReadFile("testdata/python_dispositions.json")
	if err != nil {
		return nil, err
	}
	var out map[string]map[string]struct {
		Exit   int    `json:"exit"`
		Stdout string `json:"stdout"`
	}
	return out, json.Unmarshal(raw, &out)
})

// The run-dependent identity of a store: a fresh random id and the file's device and inode.
var runIdentity = regexp.MustCompile(`("storeId": )"[0-9a-f]{32}"|("device": )\d+|("inode": )\d+`)

func normaliseIdentity(text string) string {
	return runIdentity.ReplaceAllStringFunc(text, func(m string) string {
		return m[:strings.Index(m, ":")+2] + "<ID>"
	})
}

type fixtureStep struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
}

// runDispositionsFixture replays one cli-shape fixture through the Go relay CLI (ExecuteAs, in
// process), seeding given.sql_seed through the Go store and given.files as the runner does.
func runDispositionsFixture(t *testing.T, name string) map[string][2]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contract", "fixtures", "cli-shape", name))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Given struct {
			SQLSeed string            `json:"sql_seed"`
			Files   map[string]string `json:"files"`
		} `json:"given"`
		Run struct {
			fixtureStep
			Steps []fixtureStep `json:"steps"`
		} `json:"run"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	state := filepath.Join(home, "state")
	for file, text := range fixture.Given.Files {
		target := filepath.Join(home, file)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.Given.SQLSeed != "" {
		s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx(), fixture.Given.SQLSeed); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	steps := fixture.Run.Steps
	if steps == nil {
		steps = []fixtureStep{fixture.Run.fixtureStep}
	}
	out := map[string][2]string{}
	for i, step := range steps {
		id := step.ID
		if id == "" {
			id = itoa(i)
		}
		var stdout, stderr bytes.Buffer
		code := ExecuteAs(ctx(), "codex-session-relay", append([]string{"--state", state}, step.Argv...), &stdout, &stderr, nil)
		out[id] = [2]string{itoa(code), strings.ReplaceAll(stdout.String(), home, "<HOME>")}
	}
	return out
}

// sameDispositionsAsPython replays every recorded fixture (or those containing a substring) and
// compares exit code and whole stdout with Python's, identity values normalised.
func sameDispositionsAsPython(t *testing.T, contains ...string) int {
	t.Helper()
	want, err := pythonDispositions()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	count := 0
	for name, steps := range want {
		matched := len(contains) == 0
		for _, c := range contains {
			matched = matched || strings.Contains(name, c)
		}
		if !matched {
			continue
		}
		count++
		got := runDispositionsFixture(t, name)
		for id, w := range steps {
			g := got[id]
			if g[0] != itoa(w.Exit) || normaliseIdentity(g[1]) != normaliseIdentity(w.Stdout) {
				t.Errorf("%s step %s: go exit %s\n%s\npython exit %d\n%s", name, id, g[0], g[1], w.Exit, w.Stdout)
			}
		}
	}
	if count == 0 {
		t.Fatalf("no recorded fixture matches %v", contains)
	}
	return count
}
