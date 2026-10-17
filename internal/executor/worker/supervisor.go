package worker

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

type Permit struct {
	ID  string `json:"id"`
	CPU int    `json:"cpu"`
}
type Permits interface {
	Acquire(context.Context, executor.Reference, string, int) (Permit, error)
	End(context.Context, executor.Reference, Permit, string) error
}
type Group struct {
	Reference executor.Reference `json:"reference"`
	Permit    Permit             `json:"permit"`
	Args      []string           `json:"args"`
}
type CommandResult struct {
	ExitCode   int    `json:"exit_code"`
	Terminated bool   `json:"terminated"`
	Proof      string `json:"proof"`
	CPUMethod  string `json:"cpu_method"`
}
type GroupRunner interface {
	Run(context.Context, Group) (CommandResult, error)
}
type Supervisor struct {
	Permits Permits
	Runner  GroupRunner
}

// Run counts every root tool command, including reviewer tools and unknown
// classes. Nested descendants inherit the same cgroup/permit; they cannot create
// an unbounded sibling through a second heavy gate. A lost reply retains the grant.
func (s Supervisor) Run(ctx context.Context, r executor.Reference, id string, args []string, cpu int) (CommandResult, error) {
	if s.Permits == nil || s.Runner == nil || len(args) == 0 {
		return CommandResult{}, errors.New("supervised command requires a permit authority, runner and argv")
	}
	p, err := s.Permits.Acquire(ctx, r, id, cpu)
	if err != nil {
		return CommandResult{}, err
	}
	out, err := s.Runner.Run(ctx, Group{Reference: r, Permit: p, Args: args})
	if out.Terminated && out.Proof != "" {
		err = errors.Join(err, s.Permits.End(context.WithoutCancel(ctx), r, p, out.Proof))
	}
	return out, err
}
