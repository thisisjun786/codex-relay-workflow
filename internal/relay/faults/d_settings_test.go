package faults

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
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
		cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay", "--state", pyDir, "--json"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
		out, e := cmd.Output()
		pyCode := 0
		if e != nil {
			if ex, ok := e.(*exec.ExitError); ok {
				pyCode = ex.ExitCode()
			} else {
				t.Fatal(e)
			}
		}
		var pyReply map[string]any
		if e = json.Unmarshal(out, &pyReply); e != nil {
			t.Fatalf("python %q: %v", out, e)
		}
		if code != pyCode || !reflect.DeepEqual(goReply, pyReply) {
			t.Fatalf("%v: Go %d %v; Python %d %v", args, code, goReply, pyCode, pyReply)
		}
	}
	journal := map[string][]store.JournalEntry{}
	for _, dir := range []string{goDir, pyDir} {
		s, e := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		for _, check := range []struct {
			kind, subject string
			count         int64
		}{{"fault_policy_set", "crw:report_omitted:degraded", 2}, {"fault_limit_set", "crw:notification", 1}, {"fault_limit_set", "crw:extension_z", 1}} {
			rows, e := s.Journal(context.Background(), check.kind, check.subject)
			if e != nil || int64(len(rows)) != check.count {
				t.Fatalf("%s journal %s: %v %v", dir, check.kind, rows, e)
			}
			key := check.kind + ":" + check.subject
			if dir == goDir {
				journal[key] = rows
			} else {
				for i, r := range rows {
					var g, p any
					if e = json.Unmarshal([]byte(journal[key][i].Detail), &g); e != nil {
						t.Fatal(e)
					}
					if e = json.Unmarshal([]byte(r.Detail), &p); e != nil {
						t.Fatal(e)
					}
					if !reflect.DeepEqual(g, p) {
						t.Fatalf("%s journal detail: Go %v Python %v", key, g, p)
					}
				}
			}
		}
		s.Close()
	}
}
