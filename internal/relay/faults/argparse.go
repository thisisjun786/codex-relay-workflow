package faults

import (
	"context"
	"strconv"
	"strings"
)

type numberArgsKey struct{}

// integerArg is an int option's value as a handler reads it, and whether there is one. A command
// line's numbers arrive converted in the context (int64, as the parser read them); the ledger's own
// callers hand a limit or a cursor over as text, which is read as Go reads a decimal integer.
func integerArg(ctx context.Context, name, raw string) (int64, bool) {
	if numbers, ok := ctx.Value(numberArgsKey{}).(map[string]any); ok {
		if n, ok := numbers[strings.TrimPrefix(name, "--")].(int64); ok {
			return n, true
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil
}
