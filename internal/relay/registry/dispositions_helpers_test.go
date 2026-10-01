package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// storeOnlyDispositions are the cli-shape fixtures of test_dispositions that need only a store: no
// given.host, and dispositions-show at every step. Each is compared whole with its golden.
var storeOnlyDispositions = sync.OnceValues(func() ([]string, error) {
	directory := filepath.Join("..", "..", "..", "contract", "fixtures", "cli-shape")
	paths, err := filepath.Glob(filepath.Join(directory, "test_dispositions__*.json"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		type step struct {
			Argv []json.RawMessage `json:"argv"`
		}
		var fixture struct {
			Given map[string]json.RawMessage `json:"given"`
			Run   struct {
				step
				Steps []step `json:"steps"`
			} `json:"run"`
		}
		if err := json.Unmarshal(raw, &fixture); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		steps := fixture.Run.Steps
		if steps == nil {
			steps = []step{fixture.Run.step}
		}
		storeOnly := fixture.Given["host"] == nil
		for _, s := range steps {
			storeOnly = storeOnly && len(s.Argv) > 0 && string(s.Argv[0]) == `"dispositions-show"`
		}
		if storeOnly {
			names = append(names, filepath.Base(path))
		}
	}
	return names, nil
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

// sameDispositionsAsGolden replays every store-only fixture (or those containing a substring)
// and compares each step's exit code and whole stdout with the golden, identity values
// normalised.
func sameDispositionsAsGolden(t *testing.T, contains ...string) int {
	t.Helper()
	names, err := storeOnlyDispositions()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	count := 0
	for _, name := range names {
		matched := len(contains) == 0
		for _, c := range contains {
			matched = matched || strings.Contains(name, c)
		}
		if !matched {
			continue
		}
		count++
		steps := map[string]dispositionStep{}
		for id, g := range runDispositionsFixture(t, name) {
			exit, err := strconv.Atoi(g[0])
			if err != nil {
				t.Fatal(err)
			}
			steps[id] = dispositionStep{Exit: exit, Stdout: normaliseIdentity(g[1])}
		}
		golden.CheckJSON(t, name, steps)
	}
	if count == 0 {
		t.Fatalf("no store-only fixture matches %v", contains)
	}
	return count
}

// dispositionStep is one fixture step's exit and stdout.
type dispositionStep struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
}
