package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// checkpoint is one recorded Python call from testdata/gen_assignment.py: the whole store as
// SQL at that moment, the clock, the call and Python's whole answer.
type checkpoint struct {
	Op       string            `json:"op"`
	Args     map[string]string `json:"args"`
	SQL      string            `json:"sql"`
	Now      float64           `json:"now"`
	ISO      string            `json:"iso"`
	StateDir string            `json:"stateDir"`
	Root     string            `json:"root"`
	Result   map[string]any    `json:"result"`
}

var pythonAssignment = sync.OnceValues(func() (map[string][]checkpoint, error) {
	raw, err := os.ReadFile("testdata/python_assignment.json")
	if err != nil {
		return nil, err
	}
	var out map[string][]checkpoint
	return out, json.Unmarshal(raw, &out)
})

func pythonProgram(t *testing.T) string {
	t.Helper()
	point := assignmentScenario(t, "__program__")[0]
	argv, _ := point.Result["ok"].([]any)
	if len(argv) != 1 || !filepath.IsAbs(argv[0].(string)) || filepath.Base(argv[0].(string)) != "codex-session-relay" {
		t.Fatalf("python relay program %v", argv)
	}
	return argv[0].(string)
}

func assignmentScenario(t *testing.T, name string) []checkpoint {
	t.Helper()
	all, err := pythonAssignment()
	if err != nil {
		t.Fatal(err)
	}
	points, ok := all[name]
	if !ok {
		t.Fatalf("no python assignment scenario %q", name)
	}
	return points
}

// replay loads a checkpoint's rows into a fresh Go store and makes the same call. It returns
// Go's answer and Python's, with Python's store directory rewritten to the Go store's, and the
// physical identity (device, inode) of the Go store substituted after asserting it is present.
func replay(t *testing.T, point checkpoint) (got, want map[string]any) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.DB.ExecContext(ctx(), "DELETE FROM schema_meta"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx(), point.SQL); err != nil {
		t.Fatal(err)
	}
	r := &Registry{Store: s, Now: func() string { return point.ISO }, Policy: ResolveRolePolicy(map[string]string{})}
	view := &AssignmentView{R: r, Clock: func() float64 { return point.Now }, Policy: DefaultRetryPolicy,
		Program: func() []string { return []string{"codex-session-relay"} }}
	var answer any
	switch point.Op {
	case "state":
		answer, err = view.State(ctx(), point.Args["relationship"])
	case "for_issue":
		answer, err = view.ForIssue(ctx(), point.Args["issue"])
	case "mark":
		answer, err = view.Mark(ctx(), point.Args["relationship"], "merged", point.Args["evidence"], point.Args["actor"], point.Args["expected_event"])
	case "register":
		in := Registration{
			Parent:            Endpoint{parent, host, ns("/parent"), nullString()},
			Child:             Endpoint{point.Args["child"], host, ns(point.Root), nullString()},
			IssueKey:          point.Args["issue"],
			ArtifactRoots:     []string{point.Root},
			AllowedRecipients: []string{parent},
			DispatchRequestID: point.Args["dispatch"],
			DispatchTurnID:    ns(point.Args["turn"]),
			Supersedes:        point.Args["supersedes"],
		}
		var x Relationship
		x, err = r.Register(ctx(), in)
		if err == nil {
			answer = x.ContractRecord()
		}
	default:
		t.Fatalf("unknown op %q", point.Op)
	}
	got = outcome(t, answer, err)
	raw, _ := json.Marshal(point.Result)
	text := strings.ReplaceAll(string(raw), point.StateDir, dir)
	// Each relay names its own executable in a recovery command (supervisorchannel.relay_program);
	// Python's is its console script, Go's is fixed here. Everything after it is compared.
	if program := pythonProgram(t); program != "" {
		text = strings.ReplaceAll(text, `"`+program+` --state`, `"codex-session-relay --state`)
	}
	if err := json.Unmarshal([]byte(text), &want); err != nil {
		t.Fatal(err)
	}
	if relay := obj(obj(want["ok"])["relay"]); relay != nil {
		py := obj(relay["store"])
		gostore := obj(obj(obj(got["ok"])["relay"])["store"])
		if py["inode"] == nil || gostore["inode"] == nil || py["device"] == nil || gostore["device"] == nil {
			t.Fatalf("store identity missing: python %v go %v", py, gostore)
		}
		py["inode"], py["device"] = gostore["inode"], gostore["device"]
	}
	return got, want
}

// samePoint compares one checkpoint whole with Python and returns Go's answer.
func samePoint(t *testing.T, name string, index int) map[string]any {
	t.Helper()
	point := assignmentScenario(t, name)[index]
	got, want := replay(t, point)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("%s[%d] %s differs from Python\nGO %s\nPY %s\n", name, index, point.Op, g, w)
	}
	return got
}

// sameScenario compares every checkpoint of a scenario and returns Go's answers.
func sameScenario(t *testing.T, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i := range assignmentScenario(t, name) {
		out = append(out, samePoint(t, name, i))
	}
	return out
}

func okOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	ok, present := answer["ok"].(map[string]any)
	if !present {
		t.Fatalf("expected an answer, got %v", answer)
	}
	return ok
}

func refusedOf(answer map[string]any) string {
	r, _ := answer["refused"].(map[string]any)
	s, _ := r["reason"].(string)
	return s
}
