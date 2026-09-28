package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Options struct {
	AllowIsolated, Takeover                     bool
	Actor                                       string
	SegmentSeconds, Deadline, DeadlineMonotonic *float64
	MaxSegments                                 *int
	Timeout                                     time.Duration
}

func (o Options) timeout() time.Duration {
	if o.Timeout != 0 {
		return o.Timeout
	}
	return 20 * time.Second
}
func (s *Service) command(args ...string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	prefix := []string{}
	if filepath.Base(executable) != "codex-session-relay" {
		prefix = append(prefix, "relay")
	}
	prefix = append(prefix, "--state", s.Selection.Path)
	if s.Socket != "" {
		prefix = append(prefix, "--socket", s.Socket)
	}
	return exec.Command(executable, append(prefix, args...)...), nil
}
func environmentSet(env []string, key, value string) []string {
	prefix := key + "="
	for i, s := range env {
		if strings.HasPrefix(s, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
func (s *Service) environment() []string {
	env := os.Environ()
	if s.Scope.Authority == "isolated" {
		env = environmentSet(env, ScopeEnv, s.Scope.Root)
	}
	return environmentSet(env, "CODEX_SESSION_RELAY_STATE", s.Selection.Path)
}
func (s *Service) launch(cmd *exec.Cmd) error {
	if err := os.MkdirAll(s.Selection.Path, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(s.path("daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = log, log
	err = cmd.Start()
	return errors.Join(err, log.Close())
}
func (s *Service) logTail() string {
	raw, err := os.ReadFile(s.path("daemon.log"))
	if err != nil {
		return ""
	}
	lines := strings.SplitAfter(string(raw), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "")
}
func (s *Service) Start(ctx context.Context, o Options) (Object, error) {
	if gate := s.AuthorityCheck(o.AllowIsolated); !truth(get(gate, "ok")) {
		return gate, nil
	}
	intent := s.Intent()
	if !truth(get(intent, "enabled")) {
		return obj("ok", false, "reason", "service_disabled", "intent", intent, "detail", "start never enables a service; enable it explicitly first"), nil
	}
	policy := s.LaunchEnvironment
	if policy == nil {
		policy = s.ResolveLaunchPolicy()
	}
	if refusal := LaunchRefusal(policy); refusal != nil {
		return refusal, nil
	}
	if s.LockIsHeld() {
		return obj("ok", false, "reason", "already_running", "status", s.Status(ctx)), nil
	}
	conflicts := []any{}
	for _, one := range s.Conflicts() {
		r := one.(Object)
		if truth(get(r, "live")) {
			conflicts = append(conflicts, r)
		}
	}
	if len(conflicts) > 0 {
		return obj("ok", false, "reason", "scope_owned_by_other_store", "conflicts", conflicts), nil
	}
	launch, err := randomID()
	if err != nil {
		return nil, err
	}
	s.LaunchID = launch
	s.Takeover = o.Takeover
	s.LaunchEnvironment = policy
	args := []string{"service", "run"}
	if o.AllowIsolated {
		args = append(args, "--allow-isolated-scope")
	}
	if o.SegmentSeconds != nil {
		args = append(args, "--segment-seconds", strconv.FormatFloat(*o.SegmentSeconds, 'g', -1, 64))
	}
	if o.MaxSegments != nil {
		args = append(args, "--max-segments", strconv.Itoa(*o.MaxSegments))
	}
	if o.Deadline != nil {
		args = append(args, "--deadline-monotonic", strconv.FormatFloat(Monotonic()+*o.Deadline, 'g', -1, 64))
	}
	args = append(args, "--launch-id", launch)
	if o.Takeover {
		args = append(args, "--takeover-scope")
	}
	cmd, err := s.command(args...)
	if err != nil {
		return nil, err
	}
	cmd.Env = s.environment()
	if p := text(get(policy, "path")); p != "" {
		cmd.Env = environmentSet(cmd.Env, text(get(policy, "variable")), p)
	}
	cmd.Env = environmentSet(cmd.Env, SettledEnv, launch)
	// Startup answers at supervisor readiness, before the worker publishes its
	// policy. An inherited pipe preserves that boundary even with a fast Go
	// worker: the launcher releases publication only after taking its snapshot.
	publication, release, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer publication.Close()
	defer release.Close()
	cmd.ExtraFiles = []*os.File{publication}
	cmd.Env = environmentSet(cmd.Env, publicationFDEnv, "3")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = s.launch(cmd); err != nil {
		return nil, err
	}
	done := make(chan int, 1)
	go func() { _ = cmd.Wait(); done <- cmd.ProcessState.ExitCode() }()
	abandon := func() string {
		h := OpenProcess(cmd.Process.Pid)
		defer h.Close()
		if h.Gone || h.Wait(0) {
			return "exited"
		}
		if terminate(h, max(time.Second, o.timeout()/2)) == "exited" {
			<-done
			return "terminated"
		}
		return "still_running"
	}
	deadline := time.NewTimer(o.timeout())
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	finish := func(r Object) Object {
		return set(r, "launchPolicy", set(policy, "readAt", "before this launch", "compareWith", "the worker's own published policy: doctor workerPolicy, or status launchPolicy.runningDigest"))
	}
	for {
		r := s.Record()
		if truth(get(r, "pid")) && truth(get(r, "readyAt")) && get(r, "launchId") == launch && s.LockIsHeld() {
			if s.StoreID == "" {
				s.StoreID = text(get(r, "storeId"))
			}
			return finish(obj("ok", true, "reason", nil, "pid", get(r, "pid"), "scopeAuthority", s.Scope.Authority, "scopeRoot", s.Scope.Root, "status", s.Status(ctx))), nil
		}
		select {
		case code := <-done:
			return finish(obj("ok", false, "reason", "child_exited", "exitCode", code, "log", s.logTail())), nil
		case <-ctx.Done():
			abandon()
			return nil, ctx.Err()
		case <-deadline.C:
			log := s.logTail()
			child := abandon()
			return finish(obj("ok", false, "reason", "did_not_report", "log", log, "child", child)), nil
		case <-poll.C:
		}
	}
}
func (s *Service) Restart(ctx context.Context, o Options) (Object, error) {
	intent := s.Intent()
	if !truth(get(intent, "enabled")) {
		return obj("ok", false, "reason", "service_disabled", "intent", intent, "detail", "restart never enables a service the owner turned off"), nil
	}
	policy := s.ResolveLaunchPolicy()
	if refusal := LaunchRefusal(policy); refusal != nil {
		return set(refusal, "intent", intent, "stop", obj("ok", false, "reason", "refused", "supervisor", "untouched", "worker", "untouched")), nil
	}
	stopped, err := s.Stop(o.Actor, o.timeout())
	if err != nil {
		return nil, err
	}
	if !truth(get(stopped, "ok")) && get(stopped, "reason") != "not_running" {
		return obj("ok", false, "reason", get(stopped, "reason"), "stop", stopped), nil
	}
	if !truth(get(s.Intent(), "enabled")) {
		return obj("ok", false, "reason", "service_disabled", "stop", stopped, "intent", s.Intent(), "detail", "intent changed to disabled while the service was stopping"), nil
	}
	s.LaunchEnvironment = policy
	started, err := s.Start(ctx, o)
	return obj("ok", get(started, "ok"), "reason", get(started, "reason"), "stop", stopped, "start", started), err
}
func RestartDelay(failures int) float64 {
	if failures <= 1 {
		return 2
	}
	if failures >= 9 {
		return 300
	}
	return math.Min(300, math.Ldexp(2, failures-1))
}
func (s *Service) SpawnWorker(lock, scope *os.File, token string, seconds float64, instant *float64, allow bool) (*exec.Cmd, error) {
	args := []string{"daemon"}
	if instant == nil {
		args = append(args, "--deadline", strconv.FormatFloat(seconds, 'g', -1, 64))
	} else {
		args = append(args, "--deadline-monotonic", strconv.FormatFloat(*instant, 'g', -1, 64))
	}
	scopeFD := -1
	files := []*os.File{lock}
	if scope != nil {
		scopeFD = 4
		files = append(files, scope)
	}
	args = append(args, "--supervised-token", token, "--supervised-lock-fd", "3", "--supervised-scope-fd", strconv.Itoa(scopeFD))
	if allow {
		args = append(args, "--allow-isolated-scope")
	}
	cmd, err := s.command(args...)
	if err != nil {
		return nil, err
	}
	cmd.Env = s.environment()
	if value := os.Getenv(publicationFDEnv); value != "" {
		fd, e := strconv.Atoi(value)
		if e != nil {
			return nil, e
		}
		// Duplicate for this child; closing this copy never consumes the gate.
		duplicate, e := duplicateFile(fd)
		if e != nil {
			return nil, e
		}
		defer duplicate.Close()
		cmd.Env = environmentSet(cmd.Env, publicationFDEnv, strconv.Itoa(3+len(files)))
		files = append(files, duplicate)
	}
	cmd.ExtraFiles = files
	workerAttributes(cmd)
	if err = s.launch(cmd); err != nil {
		return nil, fmt.Errorf("spawn worker: %w", err)
	}
	return cmd, nil
}
