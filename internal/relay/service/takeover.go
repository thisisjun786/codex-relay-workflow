package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"golang.org/x/sys/unix"
)

func ProcessIdentity(build string) ownership.Identity {
	boot, _ := BootID().(string)
	ticks, _ := StartTicks(os.Getpid()).(int64)
	return ownership.Identity{BootID: boot, PID: os.Getpid(), StartTicks: ticks, Build: build}
}

// TakeoverOptions are the controller's explicit launch inputs. PythonRelay is the
// absolute path of the retained fence release's console script, the only locator the
// controller uses for a Python candidate; nothing searches PATH or an install tree.
// ReadyTimeout bounds the wait for readiness, which spans the candidate's recovery.
type TakeoverOptions struct {
	PythonRelay  string
	ReadyTimeout time.Duration
}

// DefaultReadyTimeout is the controller's readiness bound when none is given. The
// 20-second channel bound covers start validation and the activation exchange only.
const DefaultReadyTimeout = 600 * time.Second

func NewTakeover(ctx context.Context, selection store.StateSelection, socket, build string, options TakeoverOptions) (*ownership.Controller, error) {
	physical, err := ownership.Physical(selection.DBPath())
	if err != nil {
		return nil, err
	}
	r, err := ownership.ReadRecord(physical.RealPath)
	if err != nil {
		return nil, err
	}
	if socket == "" && r.AppServerSocket != nil {
		socket = *r.AppServerSocket
	}
	canonical, err := store.CanonicalSocket(socket)
	if err != nil {
		return nil, err
	}
	if socket == "" {
		return nil, &ownership.Refused{Detail: "takeover requires an existing App Server scope"}
	}
	s, err := New(ctx, selection, canonical)
	if err != nil {
		return nil, err
	}
	// The existing authority, not a freshly created alternate scope, is used.
	// A Python fence created in an isolated root must record that root's key.
	return &ownership.Controller{Path: physical.RealPath, Socket: canonical, ScopeKey: s.Scope.Key(canonical), ScopeLock: s.Scope.path(canonical, ".lock"), Identity: ProcessIdentity(""), Runtime: &takeoverRuntime{s, build, options}, ValidateSchema: store.ValidateOwnershipSchema}, nil
}

type takeoverRuntime struct {
	service *Service
	build   string
	options TakeoverOptions
}

// Preflight checks what the candidate of the given runtime needs before any durable
// edge: the retained Python entry point, the service intent (both candidates are the
// service supervisor) and a launch declaration the candidate will not refuse.
func (r *takeoverRuntime) Preflight(ctx context.Context, record ownership.Record, to string) error {
	s := r.service
	if to == "python" {
		if err := r.pythonRelay(); err != nil {
			return err
		}
	} else if _, err := store.RefuseLiveState(s.Selection.DBPath()); err != nil {
		return &ownership.Refused{Detail: "the Go candidate could not open this store: " + err.Error()}
	}
	if !truth(get(s.Intent(), "enabled")) {
		return &ownership.Refused{Detail: "service_disabled: the " + to + " candidate is this service's supervisor and the service is not enabled; enable it before the transition"}
	}
	if refusal := LaunchRefusal(s.ResolveLaunchPolicy()); refusal != nil {
		return &ownership.Refused{Detail: text(get(refusal, "reason")) + ": " + text(get(refusal, "detail"))}
	}
	return ctx.Err()
}
func (r *takeoverRuntime) pythonRelay() error {
	path := r.options.PythonRelay
	if path == "" {
		return &ownership.Refused{Detail: "a Python candidate requires --python-relay <absolute path of the retained fence release's codex-session-relay>"}
	}
	if !filepath.IsAbs(path) {
		return &ownership.Refused{Detail: "--python-relay must be an absolute path: " + path}
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || unix.Access(path, unix.X_OK) != nil {
		return &ownership.Refused{Detail: "--python-relay is not an executable file: " + path}
	}
	return nil
}

// candidate is the frozen candidate argv of either runtime (decision 28): the service
// supervisor, `service run --takeover-candidate`, executed directly so its pid is the
// reply pid and its parent is this controller. It returns the build its readiness
// identity must carry.
func (r *takeoverRuntime) candidate(record ownership.Record) (*exec.Cmd, string, error) {
	args := []string{"--state", r.service.Selection.Path, "--socket", r.service.Socket, "service", "run", "--takeover-candidate"}
	if r.service.Scope.Authority == "isolated" {
		args = append(args, "--allow-isolated-scope")
	}
	if record.Owner == "python" {
		if err := r.pythonRelay(); err != nil {
			return nil, "", err
		}
		// Rollback step 4: the running Python build must equal python_compatibility_build.
		return exec.Command(r.options.PythonRelay, args...), record.PythonCompatibilityBuild, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	if filepath.Base(executable) != "codex-session-relay" {
		args = append([]string{"relay"}, args...)
	}
	return exec.Command(executable, args...), r.build, nil
}

// Drain uses pidfds and the boot/start identity, not installationId: the old
// owner deliberately has a different executable installation from this one.
func (r *takeoverRuntime) Drain(ctx context.Context, record ownership.Record) error {
	s := r.service
	run := s.Record()
	if run == nil {
		return nil
	} // the transfer locks still prove absence
	if get(run, "storeId") != record.StoreID || !s.sameStateDir(run) || !s.sameSocket(run) {
		return &ownership.Refused{Detail: "daemon identity disagrees with takeover store/scope"}
	}
	if !equal(get(run, "bootId"), BootID()) {
		return &ownership.Refused{Detail: "daemon boot identity unavailable or changed"}
	}
	// Request quiescence before opening any exclusive transfer lock.
	if err := s.RequestStop(); err != nil {
		return err
	}
	// Identify every holder first, then interrupt them together. A Go supervisor also
	// passes its interrupt on to its current worker and waits for it, and a supervised
	// worker absorbs the repeat (decision 42), so the pair stops on whichever lands first.
	var holders []*ProcessHandle
	defer func() {
		for _, h := range holders {
			_ = h.Close()
		}
	}()
	for _, pair := range [][2]string{{"pid", "startTicks"}, {"workerPid", "workerStartTicks"}} {
		pid := num(get(run, pair[0]))
		if pid == 0 {
			continue
		}
		h := OpenProcess(pid)
		if h.Gone {
			continue
		}
		if h.FD < 0 {
			return &ownership.Refused{Detail: h.Detail}
		}
		holders = append(holders, h)
		if get(run, pair[1]) == nil || !equal(get(run, pair[1]), StartTicks(pid)) {
			return &ownership.Refused{Detail: "daemon process identity changed"}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A graceful interrupt is delivered to Go; retained Python uses SIGTERM.
	sig := unix.SIGTERM
	if record.Owner == "go" {
		sig = unix.SIGINT
	}
	for _, h := range holders {
		if !h.Send(sig) {
			return fmt.Errorf("quiesce signal refused: %s", h.Detail)
		}
	}
	for _, h := range holders {
		remaining := 5 * time.Second
		if at, ok := ctx.Deadline(); ok {
			remaining = min(remaining, time.Until(at))
		}
		if !h.Wait(remaining) {
			if !h.Send(unix.SIGKILL) || !h.Wait(time.Second) {
				return fmt.Errorf("holder did not exit")
			}
		}
	}
	return nil
}

// daemon.json keeps the spelling each runtime was given (Python service.py new_record);
// the controller compares identities. stateDir is always absolute in both runtimes.
func (s *Service) sameStateDir(run Object) bool {
	recorded, err := os.Stat(text(get(run, "stateDir")))
	if err != nil {
		return false
	}
	own, err := os.Stat(s.Selection.Path)
	return err == nil && os.SameFile(recorded, own)
}

// sameSocket canonicalizes the recorded socket. A relative spelling resolved against
// the recording daemon's working directory, so it is read from a live, identity-checked
// holder and never from this controller's cwd. With no recorded process alive there
// is nothing to stop, and the Step 3 barrier alone proves the scope.
func (s *Service) sameSocket(run Object) bool {
	recorded := text(get(run, "socketPath"))
	if recorded == "" {
		return false
	}
	if !filepath.IsAbs(recorded) && !strings.HasPrefix(recorded, "~") {
		cwd, live := "", false
		for _, pair := range [][2]string{{"pid", "startTicks"}, {"workerPid", "workerStartTicks"}} {
			pid := num(get(run, pair[0]))
			if pid == 0 {
				continue
			}
			h := OpenProcess(pid)
			if h.Gone {
				_ = h.Close()
				continue
			}
			live = true
			if h.FD >= 0 && get(run, pair[1]) != nil && equal(get(run, pair[1]), StartTicks(pid)) {
				dir, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
				// The pidfd still names the verified process only while it has not exited.
				if err == nil && !h.Wait(0) {
					cwd = dir
				}
			}
			_ = h.Close()
			if cwd != "" {
				break
			}
		}
		if !live {
			return true
		}
		if cwd == "" {
			return false
		}
		recorded = cwd + "/" + recorded
	}
	canonical, err := store.CanonicalSocket(recorded)
	return err == nil && canonical == s.Socket
}

const candidateFDEnv = "CRW_TAKEOVER_CHANNEL_FD"

// channelTimeout bounds the start validation and, separately, the activation exchange
// after ready (decision 28); recovery before ready is bounded by the controller.
const channelTimeout = 20 * time.Second

type candidateMessage struct {
	Kind         string             `json:"kind"`
	Record       ownership.Record   `json:"record"`
	Identity     ownership.Identity `json:"identity"`
	StoreID      string             `json:"storeId"`
	Epoch        int64              `json:"epoch"`
	TransitionID string             `json:"transitionId"`
	Error        string             `json:"error,omitempty"`
}
type launchedCandidate struct {
	conn      net.Conn
	cmd       *exec.Cmd
	done      chan error
	identity  ownership.Identity
	activated bool
	record    ownership.Record
}

func (r *takeoverRuntime) Start(ctx context.Context, record ownership.Record) (ownership.CandidateProcess, error) {
	cmd, build, err := r.candidate(record)
	if err != nil {
		return nil, err
	}
	policy := r.service.ResolveLaunchPolicy()
	if refusal := LaunchRefusal(policy); refusal != nil {
		return nil, &ownership.Refused{Detail: text(get(refusal, "reason")) + ": " + text(get(refusal, "detail"))}
	}
	// Darwin lacks SOCK_CLOEXEC. Prevent concurrent exec from inheriting either
	// endpoint while installing close-on-exec on both descriptors.
	syscall.ForkLock.RLock()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(pair[0])
		unix.CloseOnExec(pair[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(pair[0]), "takeover-controller")
	child := os.NewFile(uintptr(pair[1]), "takeover-candidate")
	conn, err := net.FileConn(parent)
	err = errors.Join(err, parent.Close())
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	// No stale stop request may short-circuit the ready candidate's first tick.
	if err = os.Remove(r.service.path("stop.request")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(err, child.Close(), conn.Close())
	}
	cmd.Env = environmentSet(r.service.environment(), candidateFDEnv, "3")
	if p := text(get(policy, "path")); p != "" {
		cmd.Env = environmentSet(cmd.Env, text(get(policy, "variable")), p)
	}
	cmd.ExtraFiles = []*os.File{child}
	// The active holder outlives the controller's session and process group, as a
	// service start does; setsid keeps it this controller's direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = r.service.launch(cmd); err != nil {
		return nil, errors.Join(err, child.Close(), conn.Close())
	}
	if err = child.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = conn.Close()
		return nil, err
	}
	candidate := &launchedCandidate{conn: conn, cmd: cmd, done: make(chan error, 1), record: record}
	go func() { candidate.done <- cmd.Wait() }()
	fail := func(err error) (ownership.CandidateProcess, error) { return nil, errors.Join(err, candidate.Close()) }
	// Readiness follows recovery, so its bound is the controller's own, not the
	// 20-second channel bound.
	wait := r.options.ReadyTimeout
	if wait <= 0 {
		wait = DefaultReadyTimeout
	}
	deadline := time.Now().Add(wait)
	if at, ok := ctx.Deadline(); ok && at.Before(deadline) {
		deadline = at
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return fail(err)
	}
	if err = json.NewEncoder(conn).Encode(candidateMessage{Kind: "start", Record: record}); err != nil {
		return fail(err)
	}
	var reply candidateMessage
	if err = json.NewDecoder(conn).Decode(&reply); err != nil {
		return fail(fmt.Errorf("candidate gave no readiness (its output is in %s): %w", r.service.path("daemon.log"), err))
	}
	if reply.Kind != "ready" || reply.Error != "" || reply.StoreID != record.StoreID || reply.Epoch != record.Epoch || reply.TransitionID != record.Transition.ID || reply.Identity.PID != cmd.Process.Pid || reply.Identity != identityOf(cmd.Process.Pid, build) {
		return fail(fmt.Errorf("candidate readiness disagrees: %s", reply.Error))
	}
	candidate.identity = reply.Identity
	return candidate, nil
}
func identityOf(pid int, build string) ownership.Identity {
	boot, _ := BootID().(string)
	ticks, _ := StartTicks(pid).(int64)
	return ownership.Identity{BootID: boot, PID: pid, StartTicks: ticks, Build: build}
}
func (c *launchedCandidate) Identity() ownership.Identity { return c.identity }
func (c *launchedCandidate) Activate(ctx context.Context, r ownership.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.conn.SetDeadline(time.Now().Add(channelTimeout)); err != nil {
		return err
	}
	if err := json.NewEncoder(c.conn).Encode(candidateMessage{Kind: "active", Record: r}); err != nil {
		return err
	}
	var answer candidateMessage
	if err := json.NewDecoder(c.conn).Decode(&answer); err != nil {
		return err
	}
	if answer.Kind != "activated" {
		return fmt.Errorf("candidate did not accept activation")
	}
	c.activated = true
	return nil
}
func (c *launchedCandidate) Close() error {
	err := c.conn.Close()
	if !c.activated && c.identity.PID > 0 {
		current, e := ownership.ReadRecord(c.record.Database.RealPath)
		if e == nil && current.Phase == "active" && current.Holder != nil && *current.Holder == c.identity && current.Epoch == c.record.Epoch && current.Transition != nil && current.Transition.ID == c.record.Transition.ID {
			stamp, e := ownership.SnapshotMeta(context.Background(), c.record.Database.RealPath)
			if e == nil && ownership.Validate(c.record.Database.RealPath, current, stamp) == nil {
				return err
			}
		}
	}
	if !c.activated {
		h := OpenProcess(c.cmd.Process.Pid)
		defer h.Close()
		if !h.Wait(5 * time.Second) {
			if !h.Send(unix.SIGKILL) || !h.Wait(time.Second) {
				return errors.Join(err, fmt.Errorf("candidate did not quiesce"))
			}
		}
		exit := <-c.done
		// EOF after a durable active publication is allowed to leave the candidate
		// serving; a failed readiness candidate must exit, not hide a runtime error.
		if exit != nil {
			err = errors.Join(err, exit)
		}
	}
	return err
}

// CandidateChannel is inherited, never discoverable by an ordinary CLI client.
// It supplies a context-only starting permit, then observes active publication
// or channel closure before a candidate can perform its first scheduling tick.
type CandidateChannel struct {
	conn   net.Conn
	Record ownership.Record
}

func ReceiveCandidate(ctx context.Context) (context.Context, *CandidateChannel, error) {
	// takeover.receive_candidate: the selector is consumed whatever it names, so
	// workers inherit neither the channel nor its selector (decision 28).
	selector := os.Getenv(candidateFDEnv)
	if err := os.Unsetenv(candidateFDEnv); err != nil {
		return ctx, nil, err
	}
	if selector != "3" {
		return ctx, nil, &ownership.Refused{Detail: "candidate needs inherited controller channel"}
	}
	// Python's socket(fileno=3) and getpeername are host faults on a descriptor that
	// is closed, not a socket or not connected; a non-stream socket is a refusal.
	kind, err := unix.GetsockoptInt(3, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil {
		return ctx, nil, fmt.Errorf("candidate channel: %w", err)
	}
	if kind != unix.SOCK_STREAM {
		return ctx, nil, &ownership.Refused{Detail: "candidate channel must be a connected stream"}
	}
	if _, err = unix.Getpeername(3); err != nil {
		return ctx, nil, fmt.Errorf("candidate channel: %w", err)
	}
	f := os.NewFile(3, "takeover-channel")
	conn, err := net.FileConn(f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return ctx, nil, err
	}
	fail := func(err error) (context.Context, *CandidateChannel, error) {
		return ctx, nil, errors.Join(err, conn.Close())
	}
	line, received, err := receiveCandidateLine(conn, time.Now().Add(channelTimeout))
	if err != nil {
		return fail(err)
	}
	var msg candidateMessage
	if received {
		var value any
		if err = json.Unmarshal(line, &value); err != nil {
			return fail(fmt.Errorf("candidate message: %w", err))
		}
		if _, ok := value.(map[string]any); !ok {
			return fail(&ownership.Refused{Detail: "candidate message is not an object"})
		}
		if json.Unmarshal(line, &msg) != nil {
			return fail(&ownership.Refused{Detail: "invalid candidate authorization"})
		}
	}
	r := msg.Record
	if msg.Kind != "start" || r.Phase != "starting" || r.Owner != "go" || r.Controller == nil || r.Transition == nil {
		return fail(&ownership.Refused{Detail: "invalid candidate authorization"})
	}
	if r.Controller.PID != os.Getppid() || *r.Controller != identityOf(os.Getppid(), "") {
		return fail(&ownership.Refused{Detail: "controller identity disagrees"})
	}
	// Recovery runs between start and ready under the controller's own bound.
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	permit := ownership.Candidate{TransitionID: r.Transition.ID, Epoch: r.Epoch, Controller: *r.Controller}
	return ownership.WithCandidate(ctx, permit), &CandidateChannel{conn: conn, Record: r}, nil
}

// maxCandidateBytes is takeover.MAX_CANDIDATE_BYTES, the largest channel line.
const maxCandidateBytes = 64 << 20

// receiveCandidateLine is Python's CandidateChannel.receive: one line under an
// absolute deadline, read a byte at a time so nothing after it is consumed. EOF
// before any byte is no message; a partial line at EOF, a deadline already passed
// before the next byte and an oversized line are refusals; a read that times out
// (Python's socket timeout) and any other read failure are host faults.
func receiveCandidateLine(conn net.Conn, deadline time.Time) ([]byte, bool, error) {
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, false, err
	}
	var line []byte
	var b [1]byte
	for {
		if !time.Now().Before(deadline) {
			return nil, false, &ownership.Refused{Detail: "candidate channel deadline exceeded"}
		}
		n, err := conn.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return line, true, nil
			}
			if line = append(line, b[0]); len(line) > maxCandidateBytes {
				return nil, false, &ownership.Refused{Detail: "candidate message exceeds the size limit"}
			}
			continue
		}
		switch {
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, false, &ownership.Refused{Detail: "truncated candidate message"}
		case errors.Is(err, io.EOF):
			return nil, false, nil
		case err != nil:
			return nil, false, err
		}
	}
}

func (c *CandidateChannel) Ready(ctx context.Context, build string) error {
	r := c.Record
	msg := candidateMessage{Kind: "ready", Identity: ProcessIdentity(build), StoreID: r.StoreID, Epoch: r.Epoch, TransitionID: r.Transition.ID}
	if err := c.conn.SetDeadline(time.Now().Add(channelTimeout)); err != nil {
		return err
	}
	if err := json.NewEncoder(c.conn).Encode(msg); err != nil {
		return err
	}
	var answer candidateMessage
	err := json.NewDecoder(c.conn).Decode(&answer)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	// The DB stamp is reread even when the controller said active. EOF may only
	// preserve a candidate whose exact durable active record has been published.
	current, e := ownership.ReadRecord(r.Database.RealPath)
	if e != nil {
		return e
	}
	stamp, e := ownership.SnapshotMeta(ctx, r.Database.RealPath)
	if e != nil {
		return e
	}
	if e = ownership.Validate(r.Database.RealPath, current, stamp); e != nil {
		return e
	}
	if current.Phase != "active" || current.Holder == nil || *current.Holder != msg.Identity || current.Epoch != r.Epoch || stamp.TakeoverID != r.Transition.ID {
		return &ownership.Refused{Detail: "activation channel closed without matching active record"}
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	if answer.Kind != "active" {
		return &ownership.Refused{Detail: "invalid activation answer"}
	}
	return json.NewEncoder(c.conn).Encode(candidateMessage{Kind: "activated"})
}
func (c *CandidateChannel) Close() error { return c.conn.Close() }

func (s *Service) Draining() bool {
	r, err := ownership.ReadRecord(s.Selection.DBPath())
	return err != nil || r.Owner != "go" || r.Phase == "draining"
}

// ControlPath remains inside the validated, owner-only state directory.
func ControlPath(state string) string { return filepath.Join(state, "control.sock") }
