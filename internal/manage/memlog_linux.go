//go:build linux

package manage

import "context"

func init() {
	Register(Command{Name: "memlog", Summary: "append a host memory sample, by group and top ten", Run: memlogRun})
}

// memlogRun is crw manage memlog on Linux: the sampler reads the host's /proc.
func memlogRun(ctx context.Context, e *Env, args []string) int {
	return memlogRunWith(ctx, e, coreDefaults(e), args, memlogNewProcSampler("/proc"))
}
