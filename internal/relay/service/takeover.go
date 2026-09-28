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
func NewTakeover(ctx context.Context, selection store.StateSelection, socket, build string) (*ownership.Controller, error) {
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
	return &ownership.Controller{Path: physical.RealPath, Socket: canonical, ScopeKey: s.Scope.Key(canonical), ScopeLock: s.Scope.path(canonical, ".lock"), Identity: ProcessIdentity(""), Runtime: &takeoverRuntime{s, build}, ValidateSchema: store.ValidateOwnershipSchema}, nil
}

type takeoverRuntime struct {
	service *Service
	build   string
}

// Drain uses pidfds and the boot/start identity, not installationId: the old
// owner deliberately has a different executable installation from this one.
func (r *takeoverRuntime) Drain(ctx context.Context, record ownership.Record) error {
	s := r.service
	run := s.Record()
	if run == nil {
		return nil
	} // the transfer locks still prove absence
	if get(run, "storeId") != record.StoreID || get(run, "stateDir") != s.Selection.Path || get(run, "socketPath") != s.Socket {
		return &ownership.Refused{Detail: "daemon identity disagrees with takeover store/scope"}
	}
	if !equal(get(run, "bootId"), BootID()) {
		return &ownership.Refused{Detail: "daemon boot identity unavailable or changed"}
	}
	// Request quiescence before opening any exclusive transfer lock.
	if err := s.RequestStop(); err != nil {
		return err
	}
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
		if get(run, pair[1]) == nil || !equal(get(run, pair[1]), StartTicks(pid)) {
			_ = h.Close()
			return &ownership.Refused{Detail: "daemon process identity changed"}
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(err, h.Close())
		}
		// A graceful interrupt is delivered to Go; retained Python uses SIGTERM.
		sig := unix.SIGTERM
		if record.Owner == "go" {
			sig = unix.SIGINT
		}
		if !h.Send(sig) {
			return errors.Join(fmt.Errorf("quiesce signal refused: %s", h.Detail), h.Close())
		}
		remaining := 5 * time.Second
		if at, ok := ctx.Deadline(); ok {
			remaining = min(remaining, time.Until(at))
		}
		if !h.Wait(remaining) {
			if !h.Send(unix.SIGKILL) || !h.Wait(time.Second) {
				return errors.Join(fmt.Errorf("holder did not exit"), h.Close())
			}
		}
		if err := h.Close(); err != nil {
			return err
		}
	}
	return nil
}

const candidateFDEnv = "CRW_TAKEOVER_CHANNEL_FD"

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
	if record.Owner == "python" {
		return nil, &ownership.Refused{Detail: "retained Python fence build does not yet implement designated starting admission/activation channel; ownership remains python starting"}
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
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.Join(err, child.Close(), conn.Close())
	}
	args := []string{"--state", r.service.Selection.Path, "--socket", r.service.Socket, "takeover", "candidate"}
	if filepath.Base(executable) != "codex-session-relay" {
		args = append([]string{"relay"}, args...)
	}
	// No stale stop request may short-circuit the ready candidate's first tick.
	if err = os.Remove(r.service.path("stop.request")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(err, child.Close(), conn.Close())
	}
	cmd := exec.Command(executable, args...)
	cmd.Env = environmentSet(r.service.environment(), candidateFDEnv, "3")
	cmd.ExtraFiles = []*os.File{child}
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
	deadline := time.Now().Add(20 * time.Second)
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
		return fail(err)
	}
	if reply.Kind != "ready" || reply.Error != "" || reply.StoreID != record.StoreID || reply.Epoch != record.Epoch || reply.TransitionID != record.Transition.ID || reply.Identity.PID != cmd.Process.Pid || reply.Identity != identityOf(cmd.Process.Pid, r.build) {
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
	if os.Getenv(candidateFDEnv) != "3" {
		return ctx, nil, &ownership.Refused{Detail: "candidate needs inherited controller channel"}
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
	if err = conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return fail(err)
	}
	var msg candidateMessage
	if err = json.NewDecoder(conn).Decode(&msg); err != nil {
		return fail(err)
	}
	r := msg.Record
	if msg.Kind != "start" || r.Phase != "starting" || r.Owner != "go" || r.Controller == nil || r.Transition == nil {
		return fail(&ownership.Refused{Detail: "invalid candidate authorization"})
	}
	if r.Controller.PID != os.Getppid() || *r.Controller != identityOf(os.Getppid(), "") {
		return fail(&ownership.Refused{Detail: "controller identity disagrees"})
	}
	permit := ownership.Candidate{TransitionID: r.Transition.ID, Epoch: r.Epoch, Controller: *r.Controller}
	return ownership.WithCandidate(ctx, permit), &CandidateChannel{conn: conn, Record: r}, nil
}
func (c *CandidateChannel) Ready(ctx context.Context, build string) error {
	r := c.Record
	msg := candidateMessage{Kind: "ready", Identity: ProcessIdentity(build), StoreID: r.StoreID, Epoch: r.Epoch, TransitionID: r.Transition.ID}
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
