package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

type hookProbeResult struct {
	exit           int
	stdout, stderr string
}

// runHookProbeGo answers what `crw skill hook-probe` answered for args, and holds that answer,
// each UNREACHED line reduced to its function, to the golden kept under args (first taken as
// what hook_probe.py answered).
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
	result := hookProbeResult{exit, stdout.String(), stderr.String()}
	checkSkillAnswer(t, "", "", append([]string{"skill", "hook-probe"}, args...), normalizedAnswer(skillProcessResult(result)))
	return result
}

func TestHookProbeReplayFailures(t *testing.T) {
	goldenRoot(t)
	binary := recordedCRW(t)
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
		answer := runHookProbeGo(t, binary, args...)
		if answer.exit != 1 || !strings.Contains(answer.stdout, "DOCUMENTED WITHOUT A FIXTURE: T29") || !strings.Contains(answer.stdout, "The contract advertises these traces and nothing exercises them.") {
			t.Fatalf("replay did not report trace drift: %+v", answer)
		}
	})

	t.Run("missing explicit contract", func(t *testing.T) {
		missing := filepath.Join(root, ".omo", "evidence", "missing-hook-contract.md")
		args := []string{"replay", "--fixtures", decisions, "--contract", missing, "--host-fixtures", host}
		answer := runHookProbeGo(t, binary, args...)
		if answer.exit != 3 || !strings.Contains(answer.stderr, "no such file or directory") {
			t.Fatalf("replay did not reject missing contract: %+v", answer)
		}
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
		answer := runHookProbeGo(t, binary, args...)
		if answer.exit != 1 || !strings.Contains(answer.stdout, "names a capability record that is not beside it") {
			t.Fatalf("replay did not reject unpaired host record: %+v", answer)
		}
	})

}

func TestHookProbeMalformedSelectionAndCounters(t *testing.T) {
	goldenRoot(t)
	binary := recordedCRW(t)
	decisions := diskSkillPath(defaultFixture("decisions"))
	raw, err := os.ReadFile(filepath.Join(decisions, "claim-whose-preimage-is-not-a-string.json"))
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

	counterRaw, err := os.ReadFile(filepath.Join(decisions, "t24-invalid-persisted-counts.json"))
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
	answer := runHookProbeGo(t, binary, "decide", path)
	if answer.exit != 0 || !strings.Contains(answer.stdout, `"state": "`+expectedState+`"`) {
		t.Fatalf("decide did not produce %s: %+v", expectedState, answer)
	}
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
	runHookProbeGo(t, binary, "decide", path)
}
