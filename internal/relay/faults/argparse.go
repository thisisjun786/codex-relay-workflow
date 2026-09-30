package faults

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

type faultArgs struct {
	text    map[string]string
	numbers map[string]any
}
type numberArgsKey struct{}

// Numeric CLI actions arrive converted; non-CLI internal calls still use their
// existing string argument interface.
func integerArg(ctx context.Context, name, raw string) *big.Int {
	if numbers, ok := ctx.Value(numberArgsKey{}).(map[string]any); ok {
		if n, ok := numbers[strings.TrimPrefix(name, "--")].(*big.Int); ok {
			return n
		}
	}
	n, _ := argparse.ParseInt(raw)
	return n
}

func faultParse(prog, command string, argv []string, stdout, stderr io.Writer) (*faultArgs, int, bool) {
	result := argparse.Parse(command, argv)
	if result.Help {
		fmt.Fprint(stdout, argparse.Help(prog, command))
		return nil, 0, true
	}
	if result.Message != "" {
		fmt.Fprint(stderr, result.Error(prog, command))
		return nil, 2, true
	}
	values := map[string]string{}
	for name, items := range result.Values {
		values["--"+name] = items[len(items)-1]
	}
	return &faultArgs{values, result.Numbers}, 0, false
}
