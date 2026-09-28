package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

type hookProbeResult struct {
	exit           int
	stdout, stderr string
}

func buildHookProbeCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "crw")
	command := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/crw")
	command.Dir = repositoryRoot()
	command.Env = oracleEnv("TMPDIR=/var/tmp")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build crw: %v\n%s", err, output)
	}
	return binary
}

func runHookProbePython(t *testing.T, args ...string) hookProbeResult {
	t.Helper()
	root := repositoryRoot()
	command := exec.Command(
		filepath.Join(root, ".venv", "bin", "python"),
		append([]string{filepath.Join(root, "plugins", "crw", "skills", "crw-run", "scripts", "hook_probe.py")}, args...)...,
	)
	command.Env = oracleEnv("PYTHONDONTWRITEBYTECODE=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	exit := 0
	if err := command.Run(); err != nil {
		var failed *exec.ExitError
		if !errors.As(err, &failed) {
			t.Fatal(err)
		}
		exit = failed.ExitCode()
	}
	return hookProbeResult{exit, stdout.String(), stderr.String()}
}

func runHookProbeGo(t *testing.T, binary string, args ...string) hookProbeResult {
	t.Helper()
	command := exec.Command(binary, append([]string{"skill", "hook-probe"}, args...)...)
	command.Env = oracleEnv("TMPDIR=/var/tmp")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	exit := 0
	if err := command.Run(); err != nil {
		var failed *exec.ExitError
		if !errors.As(err, &failed) {
			t.Fatal(err)
		}
		exit = failed.ExitCode()
	}
	return hookProbeResult{exit, stdout.String(), stderr.String()}
}

func requireHookProbeParity(t *testing.T, python, goResult hookProbeResult) {
	t.Helper()
	if goResult != python {
		t.Fatalf("live Python mismatch\nexit Python=%d Go=%d\nstdout %s\nstderr %s", python.exit, goResult.exit, firstHookProbeDifference(python.stdout, goResult.stdout), firstHookProbeDifference(python.stderr, goResult.stderr))
	}
}

func firstHookProbeDifference(want, got string) string {
	at := 0
	for at < len(want) && at < len(got) && want[at] == got[at] {
		at++
	}
	from := max(0, at-20)
	return "at=" + fmt.Sprint(at) + " Python=" + fmt.Sprintf("%q", want[from:min(len(want), at+800)]) + " Go=" + fmt.Sprintf("%q", got[from:min(len(got), at+800)])
}

func diskSkillPath(parts ...string) string {
	return filepath.Join(append([]string{repositoryRoot(), "plugins"}, parts...)...)
}

func TestHookProbeReplayFailuresMatchLivePython(t *testing.T) {
	binary := buildHookProbeCLI(t)
	root := repositoryRoot()
	decisions := diskSkillPath(defaultFixture("decisions"))
	contractPath := diskSkillPath(defaultContract("hook-contract.md"))
	host := diskSkillPath(defaultFixture("host"))

	t.Run("drifted documented trace", func(t *testing.T) {
		fixtures := filepath.Join(t.TempDir(), "decisions")
		if err := os.CopyFS(fixtures, os.DirFS(decisions)); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(fixtures)
		if err != nil {
			t.Fatal(err)
		}
		removed := 0
		for _, entry := range entries {
			if !strings.HasPrefix(strings.ToLower(entry.Name()), "t29") {
				continue
			}
			if err := os.Remove(filepath.Join(fixtures, entry.Name())); err != nil {
				t.Fatal(err)
			}
			removed++
		}
		if removed == 0 {
			t.Fatal("T29 fixture lookup found nothing")
		}
		args := []string{"replay", "--fixtures", fixtures, "--contract", contractPath, "--host-fixtures", host}
		python := runHookProbePython(t, args...)
		if python.exit != 1 || !strings.Contains(python.stdout, "DOCUMENTED WITHOUT A FIXTURE: T29") || !strings.Contains(python.stdout, "The contract advertises these traces and nothing exercises them.") {
			t.Fatalf("Python oracle did not report trace drift: %+v", python)
		}
		requireHookProbeParity(t, python, runHookProbeGo(t, binary, args...))
	})

	t.Run("missing explicit contract", func(t *testing.T) {
		missing := filepath.Join(root, ".omo", "evidence", "missing-hook-contract.md")
		args := []string{"replay", "--fixtures", decisions, "--contract", missing, "--host-fixtures", host}
		python := runHookProbePython(t, args...)
		if python.exit != 3 || !strings.Contains(python.stderr, "No such file or directory") {
			t.Fatalf("Python oracle did not reject missing contract: %+v", python)
		}
		requireHookProbeParity(t, python, runHookProbeGo(t, binary, args...))
	})

	t.Run("unpaired host observation", func(t *testing.T) {
		hostFixtures := t.TempDir()
		raw, err := os.ReadFile(filepath.Join(host, "host-observation-codex-0.154.0.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hostFixtures, "host-observation-codex-0.154.0.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"replay", "--fixtures", decisions, "--contract", contractPath, "--host-fixtures", hostFixtures}
		python := runHookProbePython(t, args...)
		if python.exit != 1 || !strings.Contains(python.stdout, "names a capability record that is not beside it") {
			t.Fatalf("Python oracle did not reject unpaired host record: %+v", python)
		}
		requireHookProbeParity(t, python, runHookProbeGo(t, binary, args...))
	})

}

func TestHookProbeMalformedSelectionAndCountersMatchLivePython(t *testing.T) {
	binary := buildHookProbeCLI(t)
	fixturePath := filepath.Join(defaultFixture("decisions"), "claim-whose-preimage-is-not-a-string.json")
	raw, err := fs.ReadFile(bundledSkillFiles, fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	fixtureValue, err := hook.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	fixture := asObject(fixtureValue)
	newer := asObject(objGet(fixture, "observation"))
	newerMarker := asObject(objGet(newer, "marker"))
	newerID := strings.Repeat("b", 64)
	newerMarker = objSet(newerMarker, "assignmentId", newerID)

	oldDispatch := "older-dispatch"
	digest := sha256.Sum256([]byte(oldDispatch))
	olderID := hex.EncodeToString(digest[:])
	older := contract.OrderedObject{
		{Key: "assignmentId", Value: olderID},
		{Key: "intent", Value: contract.OrderedObject{{Key: "declaredAt", Value: "2025-12-31T23:00:00+00:00"}, {Key: "dispatchRequestIdHash", Value: olderID}}},
		{Key: "claims", Value: []any{contract.OrderedObject{{Key: "factId", Value: "claims/session-1111/claim.json"}, {Key: "sessionId", Value: "session-1111"}, {Key: "dispatchRequestId", Value: oldDispatch}}}},
	}
	workspaceObservation := objSet(newer, "workspace", contract.OrderedObject{{Key: "assignments", Value: []any{older, newerMarker}}})
	workspaceObservation = objSet(workspaceObservation, "marker", nil)

	t.Run("newer non-string dispatch id", func(t *testing.T) {
		assertDecideParity(t, binary, workspaceObservation, "marker_malformed")
	})

	baseCountersPath := filepath.Join(defaultFixture("decisions"), "t24-invalid-persisted-counts.json")
	counterRaw, err := fs.ReadFile(bundledSkillFiles, baseCountersPath)
	if err != nil {
		t.Fatal(err)
	}
	counterValue, err := hook.Decode(counterRaw)
	if err != nil {
		t.Fatal(err)
	}
	counterFixture := asObject(counterValue)
	steps, _ := objGet(counterFixture, "steps").([]any)
	baseObservation := asObject(objGet(asObject(steps[0]), "observation"))
	for name, counters := range map[string]any{"array": []any{}, "string": "bad"} {
		t.Run(name+" counters", func(t *testing.T) {
			assertDecideParity(t, binary, objSet(baseObservation, "counters", counters), "marker_malformed")
		})
	}

	for name, mutate := range map[string]func(hook.Object) hook.Object{
		"unmanaged": func(observation hook.Object) hook.Object {
			return objSet(observation, "marker", nil)
		},
		"unbound": func(observation hook.Object) hook.Object {
			marker := asObject(objGet(observation, "marker"))
			marker = objSet(marker, "bound", nil)
			return objSet(observation, "marker", marker)
		},
		"declared release": func(observation hook.Object) hook.Object {
			return objSet(observation, "disposition", contract.OrderedObject{{Key: "outcome", Value: "in_progress"}, {Key: "sessionId", Value: "session-1111"}, {Key: "turnId", Value: "turn-0001"}})
		},
	} {
		t.Run(name+" preserves precedence", func(t *testing.T) {
			assertDecideParityAnyState(t, binary, objSet(mutate(cloneHookObject(t, baseObservation)), "counters", []any{}))
		})
	}
}

func cloneHookObject(t *testing.T, source hook.Object) hook.Object {
	t.Helper()
	var raw bytes.Buffer
	if err := contract.Emit(&raw, source); err != nil {
		t.Fatal(err)
	}
	value, err := hook.Decode(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return asObject(value)
}

func TestHookProbeReturnSitesMatchCanonicalPythonSource(t *testing.T) {
	// Given the embedded Python oracle source and the live Python AST implementation.
	pythonSites, err := pythonReturnSites(bundledSkillFiles)
	if err != nil {
		t.Fatal(err)
	}
	root := repositoryRoot()
	code := `import importlib.util,json,pathlib
p=pathlib.Path(` + fmt.Sprintf("%q", filepath.Join(root, "plugins", "crw", "skills", "crw-run", "scripts", "hook_probe.py")) + `)
s=importlib.util.spec_from_file_location("hook_probe",p);m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
sites=m.return_sites()
print(json.dumps([{"function":name,"line":line,"source":sites[(name,line)].splitlines()[0].strip()} for name,line in sites],sort_keys=True))`
	command := exec.Command(filepath.Join(root, ".venv", "bin", "python"), "-c", code)
	command.Env = oracleEnv("PYTHONDONTWRITEBYTECODE=1")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var live []struct {
		Function string `json:"function"`
		Line     int    `json:"line"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(output, &live); err != nil {
		t.Fatal(err)
	}
	got := make([]struct {
		Function string `json:"function"`
		Line     int    `json:"line"`
		Source   string `json:"source"`
	}, len(pythonSites))
	for i, site := range pythonSites {
		got[i].Function, got[i].Line, got[i].Source = site.function, site.line, site.source
	}
	if !reflect.DeepEqual(got, live) {
		t.Fatalf("source parser drifted from live Python AST\nGo: %#v\nPython: %#v", got, live)
	}
}

func assertDecideParity(t *testing.T, binary string, observation contract.OrderedObject, expectedState string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "observation.json")
	var raw bytes.Buffer
	if err := contract.Emit(&raw, contract.OrderedObject{{Key: "observation", Value: observation}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	python := runHookProbePython(t, "decide", path)
	if python.exit != 0 || !strings.Contains(python.stdout, `"state": "`+expectedState+`"`) {
		t.Fatalf("Python oracle did not produce %s: %+v", expectedState, python)
	}
	requireHookProbeParity(t, python, runHookProbeGo(t, binary, "decide", path))
}

func assertDecideParityAnyState(t *testing.T, binary string, observation contract.OrderedObject) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "observation.json")
	var raw bytes.Buffer
	if err := contract.Emit(&raw, contract.OrderedObject{{Key: "observation", Value: observation}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	requireHookProbeParity(t, runHookProbePython(t, "decide", path), runHookProbeGo(t, binary, "decide", path))
}
