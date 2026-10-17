package dagsched

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

func crw1650Kit(t *testing.T) (*releaseKit, ExecutorPlacement) {
	t.Helper()
	k := newReleaseKit(t)
	h := enrolled(t, k, "host-a", 1)
	k.putPlan("p", 0, "r", addRelNode("I", dag.NodeImplementation))
	if _, err := k.sched.DeclareRegions(context.Background(), "p", "I", "parent", []Region{region("x.go", "file", "")}); err != nil {
		t.Fatal(err)
	}
	in := placement(h)
	return k, in
}

func crw1650ReplayChecks() []executor.Check {
	return []executor.Check{
		{ID: "a", Args: []string{"echo", "a"}, Resources: executor.Resources{CPU: 1, Heavy: true}},
		{ID: "z", Args: []string{"echo", "z"}, Resources: executor.Resources{CPU: 1, Heavy: true}},
	}
}

func crw1650ReplayArtifacts() []executor.Artifact {
	return []executor.Artifact{
		{Path: "a.txt", SHA256: executor.Digest([]byte("a")), Size: 1},
		{Path: "b.txt", SHA256: executor.Digest([]byte("b")), Size: 1},
	}
}

func crw1650Stored(t *testing.T, k *releaseKit, id string) []byte {
	t.Helper()
	stored, err := k.sched.ExecutorAssignment(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCRW1650ExecutorReplayNormalizesWork(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*executor.Task){
		"checks only": func(tk *executor.Task) { tk.Checks = []executor.Check{tk.Checks[1], tk.Checks[0]} },
		"artifacts only": func(tk *executor.Task) {
			tk.Input.Artifacts = []executor.Artifact{tk.Input.Artifacts[1], tk.Input.Artifacts[0]}
		},
		"both": func(tk *executor.Task) {
			tk.Checks = []executor.Check{tk.Checks[1], tk.Checks[0]}
			tk.Input.Artifacts = []executor.Artifact{tk.Input.Artifacts[1], tk.Input.Artifacts[0]}
		},
	} {
		t.Run(name, func(t *testing.T) {
			k, in := crw1650Kit(t)
			in.Task.Checks = crw1650ReplayChecks()
			in.Task.Input.Artifacts = crw1650ReplayArtifacts()
			first, err := k.sched.ReleaseExecutor(ctx, "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, in)
			if err != nil {
				t.Fatal(err)
			}
			storedBefore := crw1650Stored(t, k, first.Admission.Task.Identity.ExecutionID)
			replay := in
			mutate(&replay.Task)
			before, err := json.Marshal(replay.Task)
			if err != nil {
				t.Fatal(err)
			}
			second, err := k.sched.ReleaseExecutor(ctx, "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, replay)
			if err != nil {
				t.Fatalf("a reordered replay of the same work was refused: %v", err)
			}
			after, err := json.Marshal(replay.Task)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("the replay normalized the caller's task in place")
			}
			if second.Admission.TaskDigest != first.Admission.TaskDigest || second.Admission.Task.Identity.ExecutionID != first.Admission.Task.Identity.ExecutionID {
				t.Errorf("the replay did not return the original assignment: %+v vs %+v", second, first)
			}
			storedAfter := crw1650Stored(t, k, first.Admission.Task.Identity.ExecutionID)
			if !bytes.Equal(storedBefore, storedAfter) {
				t.Error("the replay rewrote the stored assignment")
			}
			if got := k.heldSlots(); got != 1 {
				t.Errorf("%d slots held; want one", got)
			}
		})
	}
}

func TestCRW1650ExecutorReplayRejectsArgvCollision(t *testing.T) {
	ctx := context.Background()
	k, in := crw1650Kit(t)
	in.Task.Checks = []executor.Check{{ID: "test", Args: []string{"echo", "a b"}, Resources: executor.Resources{CPU: 1, Heavy: true}}}
	first, err := k.sched.ReleaseExecutor(ctx, "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, in)
	if err != nil {
		t.Fatal(err)
	}
	storedBefore := crw1650Stored(t, k, first.Admission.Task.Identity.ExecutionID)
	replay := in
	replay.Task.Checks = []executor.Check{{ID: "test", Args: []string{"echo", "a", "b"}, Resources: executor.Resources{CPU: 1, Heavy: true}}}
	if _, err := k.sched.ReleaseExecutor(ctx, "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, replay); err == nil || !strings.Contains(err.Error(), "replayed placement changed immutable work") {
		t.Fatalf("an argv collision replayed: %v", err)
	}
	storedAfter := crw1650Stored(t, k, first.Admission.Task.Identity.ExecutionID)
	if !bytes.Equal(storedBefore, storedAfter) {
		t.Error("the refused replay rewrote the stored assignment")
	}
	if got := k.heldSlots(); got != 1 {
		t.Errorf("%d slots held; want one", got)
	}
}

func TestCRW1650ExecutorReplayRejectsChangedWork(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*executor.Task){
		"model":  func(tk *executor.Task) { tk.Requested.Model = "changed" },
		"effort": func(tk *executor.Task) { tk.Requested.Effort = "changed" },
		"prompt": func(tk *executor.Task) {
			tk.Input.Prompt, tk.Input.BriefDigest = "changed", executor.Digest([]byte("changed"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			k, in := crw1650Kit(t)
			first, err := k.sched.ReleaseExecutor(ctx, "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, in)
			if err != nil {
				t.Fatal(err)
			}
			storedBefore := crw1650Stored(t, k, first.Admission.Task.Identity.ExecutionID)
			replay := in
			mutate(&replay.Task)
			if _, err := k.sched.ReleaseExecutor(ctx, "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, replay); err == nil || !strings.Contains(err.Error(), "replayed placement changed immutable work") {
				t.Fatalf("changed %s replayed: %v", name, err)
			}
			storedAfter := crw1650Stored(t, k, first.Admission.Task.Identity.ExecutionID)
			if !bytes.Equal(storedBefore, storedAfter) {
				t.Error("the refused replay rewrote the stored assignment")
			}
			if got := k.heldSlots(); got != 1 {
				t.Errorf("%d slots held; want one", got)
			}
		})
	}
}
