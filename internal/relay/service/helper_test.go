package service

import (
	"context"
	"io"
	"os"
	"os/exec"
)

// helperEnv makes this test binary one of its helper processes (runHelper) in place of its tests.
const helperEnv = "CRW_SERVICE_TEST_HELPER"

// The helpers run from init, before TestMain, on the main thread: a leader thread that exits
// must be the thread the kernel reports for the process.
func init() {
	if role := os.Getenv(helperEnv); role != "" {
		os.Exit(runHelper(role, os.Getenv(helperEnv+"_ARG")))
	}
}

// runHelper is one helper process, the fixtures that were Python scripts until todo 44. The idle
// helper is here, with no build constraint, so a test that needs a process of its own on any
// platform starts no program found on PATH; the roles only Linux can run are runPlatformHelper's
// (reap_test.go).
//
//   - "idle": a process that stays alive until its stdin is closed and then exits, as cat did.
//     It is the controller's worker, the supervisor reapStop stops, and the other process the
//     worker-policy cases name as the serving worker. A caller that does not give it a stdin
//     pipe gives it /dev/null, which ends it at once.
func runHelper(role, arg string) int {
	if role == "idle" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	return runPlatformHelper(role, arg)
}

// helper starts one of this binary's helper processes.
func helper(ctx context.Context, name, arg string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+name, helperEnv+"_ARG="+arg)
	cmd.Stderr = os.Stderr
	return cmd
}
