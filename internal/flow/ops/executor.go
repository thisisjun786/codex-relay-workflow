package ops

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

const NameExecutorCollect = "executorCollect"
const OpExecutorCollect = "crw." + NameExecutorCollect

type executorCollectOp struct{ l *Lane }

func (o *executorCollectOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var id string
	if err := args(req.Args, &id); err != nil || id == "" {
		return engine.OpResult{}, fmt.Errorf("crw.executorCollect(execution_id): exactly one execution id is required: %v", err)
	}
	c := o.l.d.Executors
	if c == nil {
		return engine.OpResult{}, fmt.Errorf("crw.executorCollect: no executor collector is wired")
	}
	ack, err := c.Collect(ctx, id)
	if err != nil {
		return engine.OpResult{}, err
	}
	return engine.OpResult{Result: result(ack), Data: map[string]any{
		"execution_id":  id,
		"task_digest":   ack.TaskDigest,
		"result_digest": ack.ResultDigest,
		"state":         ack.State,
	}}, nil
}

func (o *executorCollectOp) Proven(ctx context.Context, events []engine.Event, row engine.Event) (bool, string, error) {
	c := o.l.d.Executors
	if c == nil {
		return false, "no executor collector is wired", nil
	}
	id := str(row.Data, "execution_id")
	if id == "" {
		return false, "the row names no execution", nil
	}
	_, ack, err := c.Receipt(id)
	if err != nil {
		return false, "the execution has no current received receipt: " + err.Error(), nil
	}
	if ack.State != "received" || ack.TaskDigest != str(row.Data, "task_digest") || ack.ResultDigest != str(row.Data, "result_digest") {
		return false, "the stored receipt no longer matches the row", nil
	}
	return true, "", nil
}
