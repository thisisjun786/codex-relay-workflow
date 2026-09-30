package faults

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// FLT-34: policy and budget listing pages and mutations match live Python.
func TestDPolicyAndLimitWholeRepliesAgainstPython(t *testing.T) {
	home, e := os.MkdirTemp("/dev/shm", "fault-d-settings-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "python")
	steps := [][]string{
		{"fault-policy", "--product", "crw", "--fault-class", "report_omitted", "--severity", "degraded", "--threshold", "2", "--window", "120", "--reason", "observed twice"},
		{"fault-policy", "--product", "crw", "--fault-class", "report_omitted", "--severity", "degraded", "--threshold", "4", "--reason", "increase"},
		{"fault-policy", "--product", "crw", "--after", "observation_stalled", "--limit", "3"},
		{"fault-limit", "--product", "crw", "--kind", "notification", "--max-count", "2", "--window", "120"},
		{"fault-limit", "--product", "crw", "--kind", "extension_z", "--max-count", "3", "--window", "240"},
		{"fault-limit", "--product", "crw", "--after", "notification", "--limit", "2"},
	}
	for _, args := range steps {
		code, goReply := cliCall(t, goDir, args...)
		checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), map[string]any{"code": code, "reply": goReply})
		answer := pyCLIRun(t, home, "", append([]string{"--state", pyDir, "--json"}, args...), false, pyHomeEnv(home)...)
		out, pyCode := []byte(answer.Stdout), answer.Code
		var pyReply map[string]any
		if e = json.Unmarshal(out, &pyReply); e != nil {
			t.Fatalf("python %q: %v", out, e)
		}
		if code != pyCode || !reflect.DeepEqual(goReply, pyReply) {
			t.Fatalf("%v: Go %d %v; Python %d %v", args, code, goReply, pyCode, pyReply)
		}
	}
	checks := []struct {
		kind, subject string
		count         int64
	}{{"fault_policy_set", "crw:report_omitted:degraded", 2}, {"fault_limit_set", "crw:notification", 1}, {"fault_limit_set", "crw:extension_z", 1}}
	// journals reads each checked journal of the store in dir: every entry's detail, by kind and
	// subject.
	journals := func(dir string) (map[string][]string, error) {
		details := map[string][]string{}
		readStore(t, context.Background(), filepath.Join(dir, "relay.sqlite3"), func(ctx context.Context, s *store.Store) error {
			for _, check := range checks {
				rows, e := s.Journal(ctx, check.kind, check.subject)
				if e != nil {
					return e
				}
				key := check.kind + ":" + check.subject
				details[key] = []string{}
				for _, r := range rows {
					details[key] = append(details[key], r.Detail)
				}
			}
			return nil
		})
		return details, nil
	}
	goJournal, e := journals(goDir)
	if e != nil {
		t.Fatal(e)
	}
	// Each journal's details, compared as the values they encode.
	decoded := map[string][]any{}
	for key, details := range goJournal {
		decoded[key] = []any{}
		for _, detail := range details {
			var value any
			if e = json.Unmarshal([]byte(detail), &value); e != nil {
				t.Fatal(e)
			}
			decoded[key] = append(decoded[key], value)
		}
	}
	checkGolden(t, "journals", nil, runPathsOf(t, home), decoded)
	// Python's journals, as the store it wrote holds them after the steps.
	var pyJournal map[string][]string
	pyValue(t, "python journals", nil, pyRunPaths(t, home), &pyJournal, func() (any, error) { return journals(pyDir) })
	for _, check := range checks {
		key := check.kind + ":" + check.subject
		for dir, rows := range map[string][]string{goDir: goJournal[key], pyDir: pyJournal[key]} {
			if int64(len(rows)) != check.count {
				t.Fatalf("%s journal %s: %v", dir, check.kind, rows)
			}
		}
		for i, detail := range pyJournal[key] {
			var g, p any
			if e = json.Unmarshal([]byte(goJournal[key][i]), &g); e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal([]byte(detail), &p); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(g, p) {
				t.Fatalf("%s journal detail: Go %v Python %v", key, g, p)
			}
		}
	}
}
