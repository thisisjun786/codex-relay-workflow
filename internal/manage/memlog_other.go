//go:build !linux

package manage

import (
	"context"
	"fmt"
)

func init() {
	Register(Command{Name: "memlog", Summary: "append a host memory sample, by group and top ten", Run: memlogUnsupported})
}

// memlogUnsupported is crw manage memlog off Linux: the tree it samples is not there, so
// it reports unsupported_os and exits 2.
func memlogUnsupported(_ context.Context, e *Env, _ []string) int {
	fmt.Fprintln(e.Stderr, "crw manage memlog: error: unsupported_os (this command samples Linux /proc)")
	return usageExit
}
