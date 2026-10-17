package agent

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func (*Backend) ExecutorKind() executor.Kind { return executor.Codex }

var _ engine.ExecutorProvider = (*Backend)(nil)
