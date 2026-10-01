package faults

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// FLT-34: policy and budget listing pages and mutations, and the journal entries they write.
func TestDPolicyAndLimitWholeReplies(t *testing.T) {
	home, e := os.MkdirTemp("/dev/shm", "fault-d-settings-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	goDir := filepath.Join(home, "go")
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
	}
	checks := []struct {
		kind, subject string
		count         int
	}{{"fault_policy_set", "crw:report_omitted:degraded", 2}, {"fault_limit_set", "crw:notification", 1}, {"fault_limit_set", "crw:extension_z", 1}}
	// Each checked journal's details, by kind and subject, compared as the values they encode.
	journals := map[string][]any{}
	readStore(t, context.Background(), filepath.Join(goDir, "relay.sqlite3"), func(ctx context.Context, s *store.Store) error {
		for _, check := range checks {
			rows, e := s.Journal(ctx, check.kind, check.subject)
			if e != nil {
				return e
			}
			key := check.kind + ":" + check.subject
			if len(rows) != check.count {
				t.Fatalf("journal %s: %v", key, rows)
			}
			journals[key] = []any{}
			for _, r := range rows {
				var value any
				if e = json.Unmarshal([]byte(r.Detail), &value); e != nil {
					return e
				}
				journals[key] = append(journals[key], value)
			}
		}
		return nil
	})
	checkGolden(t, "journals", nil, runPathsOf(t, home), journals)
}
