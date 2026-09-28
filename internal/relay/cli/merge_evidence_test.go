package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

type scriptedForge struct {
	unresolved bool
	late       bool
}

func (s scriptedForge) run(argv []string, _ time.Duration) (int, string, string, error) {
	last := argv[len(argv)-1]
	if argv[1] == "api" && argv[2] == "graphql" {
		query := ""
		for _, a := range argv {
			if len(a) > 6 && a[:6] == "query=" {
				query = a
			}
		}
		nodes := []any{}
		if s.unresolved || s.late {
			nodes = []any{map[string]any{"id": "T1", "isResolved": !s.unresolved, "isOutdated": false, "path": "a", "line": 1, "comments": map[string]any{"nodes": []any{map[string]any{"url": "u/T1", "author": map[string]any{"login": "r"}, "body": "x", "createdAt": "t"}}}}}
			if s.late {
				nodes = append(nodes, map[string]any{"id": "T2", "isResolved": true, "isOutdated": false, "path": "a", "line": 2, "comments": map[string]any{"nodes": []any{map[string]any{"url": "u/T2", "author": map[string]any{"login": "r"}, "body": "x", "createdAt": "t"}}}})
			}
		}
		field := "reviewThreads"
		if bytes.Contains([]byte(query), []byte("\n    reviews(")) {
			field = "reviews"
			nodes = []any{}
		} else if bytes.Contains([]byte(query), []byte("\n    comments(")) {
			field = "comments"
			nodes = []any{}
		}
		payload := map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{field: map[string]any{"totalCount": len(nodes), "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": nodes}}}}}
		b, _ := json.Marshal(payload)
		return 0, string(b), "", nil
	}
	switch {
	case bytes.Contains([]byte(last), []byte("/pulls/")):
		return 0, `{"number":7,"html_url":"u","state":"open","merged":false,"draft":false,"head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"dev"},"mergeable":true,"mergeable_state":"clean"}`, "", nil
	case bytes.Contains([]byte(last), []byte("/git/ref/")):
		return 0, `{"ref":"refs/heads/dev"}`, "", nil
	case bytes.Contains([]byte(last), []byte("/rules/branches/")):
		return 0, `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"dev-gate"}]}}]`, "", nil
	case bytes.Contains([]byte(last), []byte("/actions/runs/1/jobs")):
		return 0, `{"total_count":1,"jobs":[{"id":11,"name":"dev-gate","run_attempt":1,"status":"completed","conclusion":"success","html_url":"job"}]}`, "", nil
	case bytes.Contains([]byte(last), []byte("/actions/runs")):
		return 0, `{"total_count":1,"workflow_runs":[{"id":1,"name":"CI","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","workflow_id":100,"event":"pull_request","html_url":"run"}]}`, "", nil
	case bytes.Contains([]byte(last), []byte("/check-runs")):
		return 0, `{"total_count":1,"check_runs":[{"id":11,"name":"dev-gate","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"completed","conclusion":"success","app":{"id":42,"slug":"actions"}}]}`, "", nil
	case bytes.Contains([]byte(last), []byte("/status")):
		return 0, `{"total_count":0,"statuses":[]}`, "", nil
	}
	return 1, "", "unexpected " + last, nil
}
func runMerge(t *testing.T, s scriptedForge, args ...string) (int, map[string]any) {
	t.Helper()
	old := forgeRunner
	forgeRunner = func(context.Context) evidence.Runner { return s.run }
	var out, stderr bytes.Buffer
	code := Execute(context.Background(), append([]string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}, args...), &out, &stderr)
	forgeRunner = old
	var payload map[string]any
	if json.Unmarshal(out.Bytes(), &payload) != nil {
		t.Fatalf("%d %s %s", code, out.String(), stderr.String())
	}
	return code, payload
}
func Test24_CLI_39_MergeEvidenceExitContracts(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		code, p := runMerge(t, scriptedForge{})
		if code != 0 || p["verdict"] != "ready" {
			t.Fatal(code, p)
		}
	})
	t.Run("unresolved", func(t *testing.T) {
		code, p := runMerge(t, scriptedForge{unresolved: true})
		if code != 2 || p["verdict"] != "not_ready" {
			t.Fatal(code, p)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		old := forgeRunner
		forgeRunner = func(context.Context) evidence.Runner {
			return func(argv []string, d time.Duration) (int, string, string, error) {
				if bytes.Contains([]byte(argv[len(argv)-1]), []byte("/rules/branches/")) {
					return 1, "", "gh: forbidden (HTTP 403)", nil
				}
				return scriptedForge{}.run(argv, d)
			}
		}
		t.Cleanup(func() { forgeRunner = old })
		var out, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}, &out, &stderr)
		var p map[string]any
		_ = json.Unmarshal(out.Bytes(), &p)
		if code != 2 || p["verdict"] != "unknown" {
			t.Fatal(code, p)
		}
	})
	t.Run("usage", func(t *testing.T) {
		var out, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"merge-evidence", "--repository=--x", "--pull-request", "1"}, &out, &stderr)
		if code != 4 {
			t.Fatal(code, out.String())
		}
	})
	t.Run("restate late", func(t *testing.T) {
		_, first := runMerge(t, scriptedForge{})
		file := filepath.Join(t.TempDir(), "record.json")
		raw, _ := json.Marshal(first)
		if os.WriteFile(file, raw, 0600) != nil {
			t.Fatal()
		}
		code, p := runMerge(t, scriptedForge{late: true}, "--restate", file)
		rest := p["restatement"].(map[string]any)
		if code != 2 || rest["current"] != false {
			t.Fatal(code, p)
		}
	})
}
func TestCLI39WholePayloadDeterministic(t *testing.T) {
	_, a := runMerge(t, scriptedForge{})
	_, b := runMerge(t, scriptedForge{})
	for _, p := range []map[string]any{a, b} {
		delete(p, "observation")
		h := p["handoff"].(map[string]any)
		delete(h, "baseVerifiedAt")
		r := p["reread"].(map[string]any)
		delete(r, "verifiedAt")
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal(a, b)
	}
}
