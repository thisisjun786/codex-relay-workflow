package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func goCLI39(t *testing.T, scenario, restate string) (int, map[string]any, string, string) {
	t.Helper()
	s := scriptedForge{}
	if scenario == "unresolved" {
		s.unresolved = true
	}
	if scenario == "late" {
		s.late = true
	}
	runner := func(argv []string, d time.Duration) (int, string, string, error) {
		if scenario == "unknown" && bytes.Contains([]byte(argv[len(argv)-1]), []byte("/rules/branches/")) {
			return 1, "", "gh: forbidden (HTTP 403)", nil
		}
		return s.run(argv, d)
	}
	old := forgeRunner
	forgeRunner = func(context.Context) evidence.Runner { return runner }
	defer func() { forgeRunner = old }()
	argv := []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}
	if scenario == "usage" {
		argv = []string{"merge-evidence", "--repository=--x", "--pull-request", "1"}
	}
	if restate == "<empty>" {
		argv = append(argv, "--restate=")
	} else if restate != "" {
		argv = append(argv, "--restate", restate)
	}
	var out, stderr bytes.Buffer
	code := Execute(context.Background(), argv, &out, &stderr)
	var payload map[string]any
	if json.Unmarshal(out.Bytes(), &payload) != nil {
		t.Fatalf("go %s %d %s %s", scenario, code, out.String(), stderr.String())
	}
	return code, payload, out.String(), stderr.String()
}

var graphqlTokenCLI39 = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*|\$|[!():{}\[\],]`)

func normalizeCLI39(p map[string]any) {
	if provenance, ok := p["provenance"].(map[string]any); ok {
		if calls, ok := provenance["calls"].([]any); ok {
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				argv, _ := call["argv"].([]any)
				for i, arg := range argv {
					text, _ := arg.(string)
					if strings.HasPrefix(text, "query=") {
						// The source document's formatting is not provenance, but its operation,
						// variables and requested fields are. GraphQL tokens retain exactly those.
						argv[i] = "query=" + strings.Join(graphqlTokenCLI39.FindAllString(strings.TrimPrefix(text, "query="), -1), " ")
					}
				}
			}
		}
	}
	if o, ok := p["observation"].(map[string]any); ok {
		o["startedAt"] = "<time>"
		o["finishedAt"] = "<time>"
	}
	if h, ok := p["handoff"].(map[string]any); ok {
		h["baseVerifiedAt"] = "<time>"
	}
	if r, ok := p["reread"].(map[string]any); ok {
		r["verifiedAt"] = "<time>"
	}
}
func compareCLI39(t *testing.T, scenario string, restate bool) {
	t.Helper()
	var goFile string
	if restate {
		_, goReady, _, _ := goCLI39(t, "ready", "")
		goFile = filepath.Join(t.TempDir(), "go.json")
		goRecord := goReady["handoff"].(map[string]any)
		goRecord["headSha"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		raw, _ := json.Marshal(goRecord)
		_ = os.WriteFile(goFile, raw, 0600)
	}
	code, goPayload, _, _ := goCLI39(t, scenario, goFile)
	normalizeCLI39(goPayload)
	expectGolden(t, scenario, map[string]any{"code": code, "payload": goPayload}, goFile)
}
func Test24_CLI_39_LivePythonWholePayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restate bool
	}{{"ready", false}, {"unresolved", false}, {"unknown", false}, {"usage", false}, {"late", true}} {
		t.Run(tc.name, func(t *testing.T) { compareCLI39(t, tc.name, tc.restate) })
	}
}

func Test24_MergeEvidenceEmptyRestateMatchesLivePython(t *testing.T) {
	for _, restate := range []string{"", "<empty>"} {
		code, got, goStdout, goStderr := goCLI39(t, "ready", restate)
		normalizeCLI39(got)
		expectGolden(t, "ready "+restate, map[string]any{"code": code, "payload": got, "stdout": normalizeCLI39Bytes(t, goStdout), "stderr": goStderr})
	}
}

var cli39Timestamp = regexp.MustCompile(`2026-[0-9]{2}-[0-9]{2}T[0-9:.]+[+-][0-9:]+`)

func normalizeCLI39Bytes(t *testing.T, document string) string {
	t.Helper()
	if !json.Valid([]byte(document)) {
		t.Fatalf("merge-evidence stdout is not JSON:\n%s", document)
	}
	return cli39Timestamp.ReplaceAllString(document, "<time>")
}
