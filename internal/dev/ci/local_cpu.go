//go:build dev

package ci

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// CRW-1591: one verification runs inside one CPU budget. On Linux the whole verifier is
// re-executed in a systemd user scope whose CPUQuota bounds every descendant of the run; hosts
// without a user manager get the approximation from the Go settings instead (a per-package GOMAXPROCS
// and the resolved package parallelism — an approximation, not a kernel cap over arbitrary native
// programs). A scope that cannot start falls back; a verification that already started inside one
// reports its own status and is never run twice.

const (
	// localCPUDefault is the budget when neither --cpus nor CRW_CI_CPUS names one (measured, CRW-1591).
	localCPUDefault = 4
	// localCPUEnvName is the environment variable a caller may set the budget with.
	localCPUEnvName = "CRW_CI_CPUS"
	// localCPUEnteredEnv marks the verifier process the scope launcher started, with the budget it
	// was asked for. Scoped steps also receive the verified value so a scope-aware heavy gate
	// can acquire its slot and exec in place instead of launching an unbounded sibling scope.
	localCPUEnteredEnv = "CRW_CI_CPU_SCOPED"
	// localCPUCgroup is the method a record carries when a kernel scope enforced the budget.
	localCPUCgroup = "systemd-cgroup-v2"
	// localCPUFallback is the method for the Go-settings approximation.
	localCPUFallback = "gomaxprocs"
	// localCPUProcsEnv names the variable a scoped step's environment carries: the cgroup.procs
	// file of the verification's own scope, so a step that passed through a gate can return to it.
	localCPUProcsEnv = "CRW_CI_CPU_PROCS"
	// localCPUGateUnavailable is the method a failed record carries when a gate could not bring
	// its step back into the verification's scope.
	localCPUGateUnavailable = "systemd-cgroup-v2-gate-unavailable"
	// localCPUGateFailure is the warning a step prints when it cannot rejoin the scope; the step
	// fails before its body runs, never running outside the budget.
	localCPUGateFailure = "cpu_budget_unavailable: heavy gate cannot enter the verification cgroup"
	// localCPUGateGuard moves the step's own shell back into the verification's scope. It runs
	// after the gate and the sealed env/chdir launcher, before any workflow body: a shell that
	// cannot write its own PID to the target refuses with a warning rather than running uncapped.
	localCPUGateGuard = `if ! { printf '%s\n' "$$" > "${CRW_CI_CPU_PROCS:-}"; } 2>/dev/null; then
  printf '%s\n' 'WARNING: ` + localCPUGateFailure + `' >&2
  exit 125
fi
`
)

// localCPUOption resolves the budget from the flag, CRW_CI_CPUS and the default. An explicit
// --cpus wins over the environment; the resolved value must be positive.
func localCPUOption(flagValue int, explicit bool, env string) (int, error) {
	if explicit {
		if flagValue <= 0 {
			return 0, fmt.Errorf("cpus must be positive: --cpus %d", flagValue)
		}
		return flagValue, nil
	}
	if env != "" {
		cpus, err := strconv.Atoi(env)
		if err != nil || cpus <= 0 {
			return 0, fmt.Errorf("%s=%q is not a positive CPU count", localCPUEnvName, env)
		}
		return cpus, nil
	}
	return localCPUDefault, nil
}

// localCPUEnv is the budget a step's environment enforces: the resolved GOFLAGS and, under the
// Go-settings fallback, the per-package GOMAXPROCS. A step or the caller's Env cannot widen them —
// this runs after both are appended. Legacy zero-budget options leave the environment untouched.
func localCPUEnv(env []string, opts localOptions) []string {
	if opts.CPUs <= 0 {
		return env
	}
	if opts.cpu == nil {
		normalized, err := localCPUOptions(opts)
		if err != nil {
			return env
		}
		opts = normalized
	}
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == "GOMAXPROCS" || name == "GODEBUG" || name == localCPUProcsEnv || name == localCPUEnteredEnv {
			continue
		}
		out = append(out, kv)
	}
	out = crwconfig.SetEnv(out, "GOFLAGS", localEngineFlags(opts.Parallel))
	// Under a cgroup Go reads the scope itself, so no GOMAXPROCS is set; the fallback names the
	// resolved per-package value instead.
	if opts.cpu != nil && opts.cpu.Method == localCPUFallback && opts.cpu.GOMAXPROCS > 0 {
		out = append(out, "GOMAXPROCS="+strconv.Itoa(opts.cpu.GOMAXPROCS))
	}
	if opts.cpu != nil && opts.cpu.Method == localCPUCgroup {
		out = append(out, localCPUEnteredEnv+"="+strconv.Itoa(opts.CPUs))
		// The step's way back into this scope. An unknown group gives an empty target — the
		// guard then fails closed, never aiming at the root cgroup.
		target := ""
		if opts.cpuGroup != "" {
			target = filepath.Join("/sys/fs/cgroup", filepath.FromSlash(opts.cpuGroup), "cgroup.procs")
		}
		out = append(out, localCPUProcsEnv+"="+target)
	}
	return out
}

// localCPUStepCommand prefixes a heavy step's script with the scope guard when the run is inside
// a kernel scope and the step goes through the heavy gate: whatever scope the gate moves it to,
// the body returns to the verification's shared cgroup — or refuses before running.
func localCPUStepCommand(opts localOptions, step localStep, command string) string {
	if opts.CPUs > 0 && opts.cpuMethod == localCPUCgroup &&
		step.heavy && strings.TrimSpace(opts.HeavyGate) != "" {
		return localCPUGateGuard + command
	}
	return command
}

// localCPUQuota is this process's own cgroup v2 cpu.max, read only for observation. A finite
// positive quota over a positive period is the answer; "max", a missing controller, or
// unreadable data is no observed quota, never an assumption about the caller's tree.
func localCPUQuota() (quota, period int64, group string, err error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, 0, "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			group = strings.TrimPrefix(line, "0::")
		}
	}
	if group == "" {
		return 0, 0, "", errors.New("no unified cgroup entry in /proc/self/cgroup")
	}
	for _, part := range strings.Split(group, "/") {
		if part == ".." {
			return 0, 0, "", fmt.Errorf("cgroup path %q leaves the cgroup root", group)
		}
	}
	raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", filepath.FromSlash(group), "cpu.max"))
	if err != nil {
		return 0, 0, "", err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 || fields[0] == "max" {
		return 0, 0, "", fmt.Errorf("cpu.max %q is not a finite quota", strings.TrimSpace(string(raw)))
	}
	if quota, err = strconv.ParseInt(fields[0], 10, 64); err != nil || quota <= 0 {
		return 0, 0, "", fmt.Errorf("cpu.max %q carries no positive quota", fields[0])
	}
	if period, err = strconv.ParseInt(fields[1], 10, 64); err != nil || period <= 0 {
		return 0, 0, "", fmt.Errorf("cpu.max %q carries no positive period", fields[1])
	}
	return quota, period, group, nil
}

// localRunWithCPU runs the verification inside its CPU budget: the whole invocation re-executes
// under a systemd user scope when the host allows one, else the Go-settings fallback applies.
// A scope that did not start falls back; a started child's status is the answer, even when the
// verification itself failed — it is never run a second time.
func localRunWithCPU(opts localOptions, argv []string, reusePath string, stdout, stderr io.Writer) int {
	if opts.CPUs == 0 {
		return localRunAndReport(opts, reusePath, stdout, stderr)
	}
	if opts.CPUs < 0 {
		return failf(stderr, "local: cpus must be positive")
	}
	if os.Getenv(localCPUEnteredEnv) == strconv.Itoa(opts.CPUs) {
		// The scope was already attempted; the kernel says which method actually bounds this
		// process, not the marker. A finite ceiling no larger than the request is the cgroup
		// backend (compared without quota*N overflow); an unobservable quota still runs exactly
		// once, under the fallback limits.
		quota, period, group, err := localCPUQuota()
		opts.cpuMethod = localCPUFallback
		opts.cpuGroup = ""
		if err == nil && quota/period <= int64(opts.CPUs) &&
			(quota%period == 0 || quota/period < int64(opts.CPUs)) {
			opts.cpuMethod = localCPUCgroup
			opts.cpuGroup = group
		}
		return localRunAndReport(opts, reusePath, stdout, stderr)
	}
	started, code, reason := localTryCPUScope(opts.CPUs, argv, stdout, stderr)
	if started {
		return code
	}
	fmt.Fprintf(stdout, "local: CPU scope unavailable (%s); using per-job Go limits\n", reason)
	opts.cpuMethod = localCPUFallback
	opts.cpuGroup = ""
	return localRunAndReport(opts, reusePath, stdout, stderr)
}

// localTryCPUScope launches argv in a systemd user scope with CPUQuota=<N>00% and reports whether
// the child was entered. The entry script touches an owned marker before execing the verifier, so
// a launcher that never ran (no user manager, a failing systemd-run) is told apart from a child
// that ran and failed: only the former may fall back.
func localTryCPUScope(cpus int, argv []string, stdout, stderr io.Writer) (started bool, code int, reason string) {
	if runtime.GOOS != "linux" {
		return false, 0, "cgroup CPU scopes are Linux-only"
	}
	run, err := exec.LookPath("systemd-run")
	if err != nil {
		return false, 0, "systemd-run is not on PATH"
	}
	tempDir, err := tmploc.Dir()
	if err != nil {
		return false, 0, err.Error()
	}
	if err := homeguard.Refuse(tempDir); err != nil {
		return false, 0, fmt.Sprintf("TMPDIR: %v", err)
	}
	if _, err := tmploc.Check(); err != nil {
		return false, 0, err.Error()
	}
	dir, err := tmploc.MkdirTemp("crw-ci-cpu-")
	if err != nil {
		return false, 0, err.Error()
	}
	defer os.RemoveAll(dir)
	marker := filepath.Join(dir, "entered")
	entry := filepath.Join(dir, "entry.sh")
	if err := os.WriteFile(entry, []byte("#!/bin/sh\nset -e\n: > \"$1\"\nshift\nexec \"$@\"\n"), 0o700); err != nil {
		return false, 0, err.Error()
	}
	words := []string{
		"--user", "--scope", "--quiet", "--expand-environment=no",
		"--unit=" + filepath.Base(dir),
		"--property=CPUQuota=" + strconv.Itoa(cpus) + "00%",
		"--", "/bin/sh", entry, marker,
	}
	words = append(words, argv...)
	cmd := exec.Command(run, words...)
	cmd.Env = crwconfig.SetEnv(os.Environ(), localCPUEnteredEnv, strconv.Itoa(cpus))
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	if _, statErr := os.Stat(marker); statErr == nil {
		// The child entered: its status is the answer, never a reason to run the
		// verification again.
		code := 0
		var exit *exec.ExitError
		if err != nil {
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else {
				code = 1
			}
		}
		return true, code, ""
	}
	if err == nil {
		err = errors.New("systemd-run exited without starting the command")
	}
	return false, 0, err.Error()
}
