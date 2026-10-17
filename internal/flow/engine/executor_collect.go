package engine

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

type ExecutorCollector struct {
	Registry *executor.Registry
	Adapter  executor.Adapter
	Spool    string
	Peer     executor.Peer
	Limits   executor.Limits
}

func (c *ExecutorCollector) Collect(ctx context.Context, executionID string) (executor.Acknowledgement, error) {
	if c == nil || c.Registry == nil {
		return executor.Acknowledgement{}, errors.New("executor collector is not wired")
	}
	a, _, err := c.Registry.Current(executionID)
	if err != nil {
		return executor.Acknowledgement{}, err
	}
	d, err := (executor.Port{Adapter: c.Adapter}).Collect(ctx, a)
	if err != nil {
		return executor.Acknowledgement{}, err
	}
	return c.Registry.Collect(executor.ReferenceOf(a), c.Spool, d.ResultPath, c.Peer, c.Limits)
}

func (c *ExecutorCollector) Receipt(executionID string) (executor.Result, executor.Acknowledgement, error) {
	if c == nil || c.Registry == nil {
		return executor.Result{}, executor.Acknowledgement{}, errors.New("executor collector is not wired")
	}
	a, _, err := c.Registry.Current(executionID)
	if err != nil {
		return executor.Result{}, executor.Acknowledgement{}, err
	}
	return c.Registry.Receipt(executor.ReferenceOf(a))
}
