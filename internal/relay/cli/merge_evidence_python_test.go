package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

type pythonCLIResult struct {
	Code    int            `json:"code"`
	Payload map[string]any `json:"payload"`
	Stdout  string         `json:"stdout"`
	Stderr  string         `json:"stderr"`
}

// pythonCLI39 is what testdata/python_cli39.py answered (recorded: see askPython).
func pythonCLI39(t *testing.T, scenario, restate string) pythonCLIResult {
	t.Helper()
	var result pythonCLIResult
	askPython(t, scenario+" "+restate, &result, func() (any, error) { return livePythonCLI39(t, scenario, restate) })
	return result
}

// livePythonCLI39 runs testdata/python_cli39.py. Only a capture closure calls it.
func livePythonCLI39(t *testing.T, scenario, restate string) (pythonCLIResult, error) {
	repo, _ := filepath.Abs("../../..")
	script, _ := filepath.Abs("testdata/python_cli39.py")
	args := []string{"run", "--no-sync", "python", script, scenario}
	if restate == "<empty>" {
		args = append(args, "")
	} else if restate != "" {
		args = append(args, restate)
	}
	cmd := exec.Command("uv", args...)
	cmd.Dir = repo
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "CODEX_HOME="+home, "TMPDIR="+os.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return pythonCLIResult{}, fmt.Errorf("python %s: %v %s", scenario, err, out)
	}
	var result pythonCLIResult
	if err = json.Unmarshal(out, &result); err != nil {
		return result, fmt.Errorf("%v: %s", err, out)
	}
	return result, nil
}
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
	var pyFile, goFile string
	if restate {
		_, goReady, _, _ := goCLI39(t, "ready", "")
		goFile = filepath.Join(t.TempDir(), "go.json")
		goRecord := goReady["handoff"].(map[string]any)
		goRecord["headSha"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		raw, _ := json.Marshal(goRecord)
		_ = os.WriteFile(goFile, raw, 0600)
		pyFile = filepath.Join(t.TempDir(), "py.json")
	}
	// Python restates the handoff of its own ready answer (recorded with it: see askPython).
	var py pythonCLIResult
	askPython(t, scenario, &py, func() (any, error) {
		if restate {
			ready, err := livePythonCLI39(t, "ready", "")
			if err != nil {
				return nil, err
			}
			pyRecord := ready.Payload["handoff"].(map[string]any)
			pyRecord["headSha"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			raw, _ := json.Marshal(pyRecord)
			if err = os.WriteFile(pyFile, raw, 0600); err != nil {
				return nil, err
			}
		}
		return livePythonCLI39(t, scenario, pyFile)
	}, pyFile)
	code, goPayload, _, _ := goCLI39(t, scenario, goFile)
	normalizeCLI39(py.Payload)
	normalizeCLI39(goPayload)
	if code != py.Code || !reflect.DeepEqual(goPayload, py.Payload) {
		a, _ := json.Marshal(goPayload)
		b, _ := json.Marshal(py.Payload)
		t.Fatalf("%s complete parity differs code go=%d python=%d\ngo=%s\npython=%s", scenario, code, py.Code, a, b)
	}
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
		py := pythonCLI39(t, "ready", restate)
		code, got, goStdout, goStderr := goCLI39(t, "ready", restate)
		normalizeCLI39(py.Payload)
		normalizeCLI39(got)
		if code != py.Code || !reflect.DeepEqual(got, py.Payload) {
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(py.Payload)
			t.Fatalf("%q --restate differs code go=%d python=%d\ngo=%s\npython=%s", restate, code, py.Code, a, b)
		}
		pyBytes := normalizeCLI39Bytes(t, py.Stdout)
		goNormalized := normalizeCLI39Bytes(t, goStdout)
		if py.Stderr != goStderr || pyBytes != goNormalized {
			t.Fatalf("%q bytes differ\npython stderr=%q\ngo stderr=%q\npython=%s\ngo=%s", restate, py.Stderr, goStderr, pyBytes, goNormalized)
		}
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
