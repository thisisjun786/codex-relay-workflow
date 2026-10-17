//go:build linux

package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CgroupRunner requires an operator-delegated cgroup v2 subtree. No fallback
// silently runs arbitrary native commands without a kernel CPU bound.
type CgroupRunner struct {
	Root                   string
	Directory              string
	Env                    []string
	CommandUID, CommandGID uint32
}

// The tool user cannot move its processes out of the worker-owned cgroup or
// regain privilege with a setuid binary. Provisioning that identity is an
// explicit host-profile operation; the worker never creates accounts.
func (r CgroupRunner) Available() error {
	if os.Geteuid() != 0 || r.CommandUID == 0 {
		return errors.New("bounded commands require an approved distinct unprivileged tool identity and privileged worker")
	}
	if _, err := os.Stat(fmt.Sprintf("/run/user/%d/bus", r.CommandUID)); err == nil {
		return errors.New("tool identity has an independent service manager")
	}
	root, err := filepath.EvalSymlinks(r.Root)
	if err != nil || root != r.Root || !strings.HasPrefix(root, "/sys/fs/cgroup/") {
		return errors.New("approved cgroup v2 subtree required")
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("cgroup subtree must be worker-owned")
	}
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	if !strings.Contains(" "+strings.TrimSpace(string(controllers))+" ", " cpu ") {
		return errors.New("CPU controller is not delegated")
	}
	if _, err = os.Stat("/usr/bin/setpriv"); err != nil {
		return errors.New("setpriv is required for no-new-privileges command supervision")
	}
	return nil
}

func (r CgroupRunner) Run(ctx context.Context, g Group) (CommandResult, error) {
	out := CommandResult{ExitCode: -1, CPUMethod: "cgroup-v2"}
	if g.Permit.CPU < 1 || len(g.Args) == 0 || len(g.Permit.ID) != 64 {
		return out, errors.New("invalid command permit")
	}
	if _, err := hex.DecodeString(g.Permit.ID); err != nil {
		return out, err
	}
	if err := r.Available(); err != nil {
		return out, err
	}
	root, err := filepath.EvalSymlinks(r.Root)
	if err != nil || root != r.Root || !strings.HasPrefix(root, "/sys/fs/cgroup/") {
		return out, errors.New("approved cgroup v2 subtree required")
	}
	group := filepath.Join(root, g.Permit.ID)
	// An existing group is an unresolved invocation, never permission to start again.
	if err = os.Mkdir(group, 0700); err != nil {
		return out, err
	}
	failBeforeStart := func(err error) (CommandResult, error) {
		out.Terminated = true
		out.Proof = "command not started; exclusive cgroup creation"
		return out, err
	}
	if err = os.WriteFile(filepath.Join(group, "cpu.max"), []byte(strconv.Itoa(g.Permit.CPU*100000)+" 100000"), 0600); err != nil {
		return failBeforeStart(err)
	}
	raw, err := os.ReadFile(filepath.Join(group, "cpu.max"))
	if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(g.Permit.CPU*100000)+" 100000" {
		return failBeforeStart(errors.New("CPU quota readback differs"))
	}
	fd, err := os.Open(group)
	if err != nil {
		return failBeforeStart(err)
	}
	defer fd.Close()
	cmd := exec.CommandContext(ctx, "/usr/bin/setpriv", append([]string{"--no-new-privs", "--"}, g.Args...)...)
	cmd.Dir = r.Directory
	for _, v := range r.Env {
		key, _, _ := strings.Cut(v, "=")
		switch key {
		case "CRW_CI_CPU_SCOPED", "CRW_CI_CPU_PROCS", "CRW_CI_CPUS", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR":
			continue
		}
		cmd.Env = append(cmd.Env, v)
	}
	cmd.Env = append(cmd.Env, "CRW_CI_CPU_SCOPED="+strconv.Itoa(g.Permit.CPU), "CRW_CI_CPU_PROCS="+filepath.Join(group, "cgroup.procs"), "CRW_CI_CPUS="+strconv.Itoa(g.Permit.CPU))
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(fd.Fd()), Credential: &syscall.Credential{Uid: r.CommandUID, Gid: r.CommandGID, Groups: []uint32{}}}
	// A cancellation kills the entire group, including background descendants.
	cmd.Cancel = func() error { return os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("1"), 0600) }
	if err = cmd.Start(); err != nil {
		return failBeforeStart(err)
	}
	waitErr := cmd.Wait()
	if cmd.ProcessState != nil {
		out.ExitCode = cmd.ProcessState.ExitCode()
	}
	// A foreground exit is insufficient. Kill remaining descendants and read the
	// kernel's populated bit before returning the shared reservation.
	if err = os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("1"), 0600); err != nil {
		return out, errors.Join(waitErr, err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		raw, err = os.ReadFile(filepath.Join(group, "cgroup.events"))
		if err != nil {
			return out, errors.Join(waitErr, err)
		}
		if strings.Contains(string(raw), "populated 0") {
			out.Terminated = true
			out.Proof = fmt.Sprintf("cgroup %s populated=0 after wait and kill", g.Permit.ID)
			return out, waitErr
		}
		time.Sleep(10 * time.Millisecond)
	}
	return out, errors.Join(waitErr, errors.New("command group termination remains unknown"))
}

func (r CgroupRunner) InspectGroup(_ context.Context, g Group, cancel bool) (CommandResult, error) {
	out := CommandResult{ExitCode: -1, CPUMethod: "cgroup-v2"}
	if err := r.Available(); err != nil {
		return out, err
	}
	if len(g.Permit.ID) != 64 {
		return out, errors.New("invalid permit identity")
	}
	if _, err := hex.DecodeString(g.Permit.ID); err != nil {
		return out, err
	}
	group := filepath.Join(r.Root, g.Permit.ID)
	if cancel {
		if err := os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("1"), 0600); err != nil {
			return out, err
		}
	}
	raw, err := os.ReadFile(filepath.Join(group, "cgroup.events"))
	if err != nil {
		return out, err
	}
	if strings.Contains(string(raw), "populated 0") {
		out.Terminated = true
		out.Proof = "dead command launcher; recorded cgroup " + g.Permit.ID + " populated=0"
	}
	return out, nil
}
