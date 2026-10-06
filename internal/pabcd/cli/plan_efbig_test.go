package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// TestPlanEfbigHelperProcess is the child of TestPlanInitReportsTheFileSizeLimitFailure, not a test of
// its own: a file size limit is a setting of the whole process, and lowering it in the go test process
// also fails the harness's own writes, so the limit is lowered in a child that exits as soon as the
// call under test has returned.
func TestPlanEfbigHelperProcess(t *testing.T) {
	cwd := os.Getenv("CRW_PLAN_EFBIG_CWD")
	if cwd == "" {
		t.Skip("child of the file size limit test")
	}
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		fmt.Println("setup:", err)
		os.Exit(2)
	}
	zero := old
	zero.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &zero); err != nil {
		fmt.Println("setup:", err)
		os.Exit(2)
	}
	result := RunPlanCli(PlanCliArgs{Verb: "init", Slug: "efbig", Phases: 1, Cwd: cwd})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	fmt.Printf("code=%d output=%q\n", result.Code, result.Output)
	os.Exit(0)
}

// TestPlanInitReportsTheFileSizeLimitFailure pins the oracle's text for a plan write that fails with
// EFBIG. The CRW-594 record claimed plan init's output was byte-identical, which hid this case: its
// recorded plan_test.go cases never lower RLIMIT_FSIZE. The code is unchanged here; only the text is
// pinned. A file size limit is a setting of the whole process, so the limit is lowered in a child.
func TestPlanInitReportsTheFileSizeLimitFailure(t *testing.T) {
	cwd := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPlanEfbigHelperProcess$")
	cmd.Env = append(os.Environ(), "CRW_PLAN_EFBIG_CWD="+cwd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper process: %v (output %q)", err, out)
	}
	got := strings.TrimSpace(string(out))
	if strings.HasPrefix(got, "setup:") {
		t.Skipf("cannot lower RLIMIT_FSIZE here: %s", got)
	}
	want := "code=1 output=\"plan init failed: EFBIG: file too large, write\""
	if got != want {
		t.Errorf("plan init past RLIMIT_FSIZE: got %s, want %s", got, want)
	}
}
