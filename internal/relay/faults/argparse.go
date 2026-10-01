package faults

import (
	"context"
	"math/big"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

type numberArgsKey struct{}

// Numeric CLI actions arrive converted; non-CLI internal calls still use their
// existing string argument interface.
func integerArg(ctx context.Context, name, raw string) *big.Int {
	if numbers, ok := ctx.Value(numberArgsKey{}).(map[string]any); ok {
		if n, ok := numbers[strings.TrimPrefix(name, "--")].(*big.Int); ok {
			return n
		}
	}
	n, _ := pyvalue.ParseInt(raw)
	return n
}
