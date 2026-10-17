package worker

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"strings"
	"sync"
	"testing"
)

type permitFixture struct {
	mu                sync.Mutex
	held, cpu, maxCPU int
}

func (f *permitFixture) Acquire(_ context.Context, _ executor.Reference, _ string, cpu int) (Permit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cpu+cpu > 2 || f.held >= 1 {
		return Permit{}, errors.New("full")
	}
	f.held++
	f.cpu += cpu
	f.maxCPU = max(f.maxCPU, f.cpu)
	return Permit{ID: "permit", CPU: cpu}, nil
}
func (f *permitFixture) End(_ context.Context, _ executor.Reference, _ Permit, proof string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if proof == "" {
		return errors.New("no proof")
	}
	f.held--
	f.cpu = 0
	return nil
}

type blockingGroup struct {
	entered chan Group
	done    chan struct{}
	unknown bool
}

func (f *blockingGroup) Run(_ context.Context, g Group) (CommandResult, error) {
	f.entered <- g
	<-f.done
	if f.unknown {
		return CommandResult{}, errors.New("lost worker")
	}
	return CommandResult{ExitCode: 0, Terminated: true, Proof: "entire cgroup empty", CPUMethod: "cgroup-v2"}, nil
}
func TestReviewerAndNestedCommandsShareHeavyBudget(t *testing.T) {
	a, _ := task(t)
	f := &permitFixture{}
	runner := &blockingGroup{entered: make(chan Group, 1), done: make(chan struct{})}
	s := Supervisor{Permits: f, Runner: runner}
	done := make(chan error, 1)
	go func() {
		_, err := s.Run(context.Background(), executor.ReferenceOf(a), "executor-test", []string{"make", "test"}, 2)
		done <- err
	}()
	g := <-runner.entered
	if g.Permit.CPU != 2 || g.Permit.ID == "" {
		t.Fatal("missing inherited group budget")
	}
	if _, err := s.Run(context.Background(), executor.ReferenceOf(a), "reviewer-test", []string{"go", "test", "./..."}, 1); err == nil {
		t.Fatal("reviewer escaped shared budget")
	}
	close(runner.done)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.held != 0 || f.maxCPU > 2 {
		t.Fatal("permit accounting")
	}
	// A lost runner response cannot prove descendants have exited.
	runner.unknown = true
	if _, err := s.Run(context.Background(), executor.ReferenceOf(a), "unknown-command", []string{"tool"}, 2); err == nil {
		t.Fatal("expected lost response")
	}
	if f.held != 1 {
		t.Fatal("premature permit return")
	}
}

func TestCgroupRunnerRefusesUnconfinedCommandIdentity(t *testing.T) {
	runner := CgroupRunner{Root: "/synthetic/cgroup", CommandUID: 0}
	if _, err := runner.Run(context.Background(), Group{Permit: Permit{ID: strings.Repeat("a", 64), CPU: 1}, Args: []string{"true"}}); err == nil {
		t.Fatal("unconfined command identity accepted")
	}
}
