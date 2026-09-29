package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"golang.org/x/sys/unix"
)

// Owned holds both locks until Close. Workers adopt the descriptions and never
// publish over their supervisor's identity or unlock another holder's copy.
type Owned struct {
	service    *Service
	lock       *daemon.SingleInstance
	scope      *os.File
	supervised bool
	Record     Object
}

func (s *Service) Own(allow, requireIntent bool, lockFD, scopeFD *int) (*Owned, error) {
	gate := s.AuthorityCheck(allow)
	if !truth(get(gate, "ok")) {
		return nil, &Refused{text(get(gate, "reason")), text(get(gate, "detail"))}
	}
	if requireIntent && !truth(get(s.Intent(), "enabled")) {
		return nil, &Refused{"service_disabled", "this service is not enabled; running it would ignore the owner's intent"}
	}
	token, err := randomID()
	if err != nil {
		return nil, err
	}
	l, err := daemon.Acquire(s.Selection.Path, false, lockFD)
	if err != nil {
		return nil, err
	}
	o := &Owned{service: s, lock: l, supervised: lockFD != nil}
	if o.supervised {
		if scopeFD != nil && *scopeFD >= 0 {
			o.scope = os.NewFile(uintptr(*scopeFD), "scope.lock")
		}
		if s.Prepare != nil {
			if err = s.Prepare(); err != nil {
				_ = o.Close()
				return nil, err
			}
		}
		o.Record = s.Record()
		return o, nil
	}
	if s.Socket != "" {
		claim, e := s.Scope.claim(s.Socket, func() Object { return s.NewRecord(os.Getpid(), token) }, s.Prepare)
		if e != nil {
			_ = l.Close()
			return nil, e
		}
		if !truth(get(claim, "ok")) {
			_ = l.Close()
			return nil, &Refused{text(get(claim, "reason")), evidence.Dumps(get(claim, "held_by"), false, false, true)}
		}
	} else if s.Prepare != nil {
		if err = s.Prepare(); err != nil {
			_ = l.Close()
			return nil, err
		}
	}
	o.Record = s.NewRecord(os.Getpid(), token)
	if err = s.WriteRecord(o.Record); err != nil {
		_ = o.Close()
		return nil, err
	}
	return o, nil
}
func (o *Owned) Close() error {
	var err error
	if o.supervised {
		if o.scope != nil {
			err = o.scope.Close()
		}
	} else {
		if o.service.Socket != "" {
			err = o.service.Scope.Release(o.service.Socket)
		}
		r := o.service.Record()
		if r == nil {
			r = o.Record
		}
		err = errors.Join(err, o.service.WriteRecord(set(r, "pid", nil, "workerPid", nil, "stoppedAt", stamp())))
	}
	return errors.Join(err, o.lock.Close())
}

func (s *Service) Adopt(token *string, lockFD, scopeFD *int) error {
	if token == nil && lockFD == nil && scopeFD == nil {
		return nil
	}
	fail := func(reason, detail string) error { return &Refused{reason, detail} }
	if token == nil || lockFD == nil || scopeFD == nil {
		return fail("supervised_invocation_incomplete", "a supervised worker needs the token and both descriptors together")
	}
	r := s.Record()
	if !truth(get(r, "token")) || text(get(r, "token")) != *token {
		return fail("supervised_token_mismatch", "the token does not match this state directory")
	}
	if v := get(r, "stateDir"); v != nil && v != s.Selection.Path {
		return fail("supervised_state_mismatch", "the record names a different state directory")
	}
	if v := get(r, "socketPath"); v != nil && v != s.Socket {
		return fail("supervised_socket_mismatch", "the record names a different operating scope")
	}
	if v := get(r, "storeId"); v != nil && v != s.StoreID {
		return fail("supervised_store_mismatch", "the record names a different store than this worker opened")
	}
	var want, got unix.Stat_t
	if err := unix.Stat(s.path("daemon.lock"), &want); err != nil {
		return fail("supervised_fd_unreadable", fmt.Sprintf("OSError: %v", err))
	}
	if err := unix.Fstat(*lockFD, &got); err != nil {
		detail := fmt.Sprintf("OSError: %v", err)
		if errors.Is(err, unix.EBADF) {
			detail = "OSError: [Errno 9] Bad file descriptor"
		}
		return fail("supervised_fd_unreadable", detail)
	}
	if want.Dev != got.Dev || want.Ino != got.Ino {
		return fail("supervised_fd_mismatch", "the inherited descriptor is not this daemon lock")
	}
	// Go's prctl is thread-scoped. SysProcAttr.Pdeathsig arms the worker on the
	// exec thread; this check closes the parent-death-before-bootstrap race.
	if os.Getppid() != num(get(r, "pid")) {
		return fail("supervisor_already_gone", fmt.Sprintf("parent is %d, not the recorded supervisor %v", os.Getppid(), get(r, "pid")))
	}
	return nil
}
func (s *Service) PublishWorkerPolicy(policy Object) error {
	if err := awaitPublication(); err != nil {
		return err
	}
	r := s.Record()
	pid := os.Getpid()
	if (num(get(r, "pid")) != pid && num(get(r, "pid")) != os.Getppid()) || !truth(get(r, "token")) {
		return fmt.Errorf("worker policy publication needs this process's service run")
	}
	info, err := os.Stat(s.Selection.DBPath())
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	run := Object{}
	for _, key := range strings.Fields("token pid startTicks installationId stateDir socketPath storeId scopeRoot scopeAuthority") {
		run = set(run, key, get(r, key))
	}
	run = set(run, "dbDevice", int64(stat.Dev), "dbInode", int64(stat.Ino))
	// The worker's python_compatibility_build is null for the reason NewRecord gives.
	payload := obj("schemaVersion", 1, "policy", policy, "observedAt", stamp(), "worker", obj("pid", pid, "startTicks", StartTicks(pid), "bootId", BootID(), "python_compatibility_build", nil), "service", run)
	// json.dump's default separators, unlike the indented supervisor record.
	raw := evidence.Dumps(payload, false, false, true)
	f, err := temporaryRecord(s.path("worker-policy.json"))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(raw)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path("worker-policy.json"))
}
func sameObject(a, b Object) bool {
	plain := func(o Object) any { raw, _ := encoded(o); var v any; _ = json.Unmarshal(raw, &v); return v }
	return reflect.DeepEqual(plain(a), plain(b))
}
func existingLockHeld(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return false
	}
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return false
	}
	if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EACCES) {
		return false
	}
	after, err := os.Stat(path)
	return err == nil && os.SameFile(before, after)
}
func (s *Service) ReadWorkerPolicy(ctx context.Context) Object {
	absent := func(reason string) Object { return obj("observed", false, "reason", reason, "policy", nil) }
	r := s.Record()
	raw, err := os.ReadFile(s.path("worker-policy.json"))
	if err != nil || len(raw) > 65536 {
		return absent("worker_policy_unreadable")
	}
	receipt, err := parse(raw)
	if err != nil || r == nil {
		return absent("worker_policy_unreadable")
	}
	if n, ok := get(receipt, "schemaVersion").(json.Number); !ok || n != "1" {
		return absent("worker_policy_version_unknown")
	}
	worker, wok := get(receipt, "worker").(Object)
	run, rok := get(receipt, "service").(Object)
	policy, pok := get(receipt, "policy").(Object)
	if !wok || !rok || !pok {
		return absent("worker_policy_unreadable")
	}
	validInt := func(v any, minimum int) bool {
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		i, e := n.Int64()
		return e == nil && i >= int64(minimum)
	}
	if !validInt(get(r, "pid"), 1) || !validInt(get(r, "startTicks"), 0) || (get(r, "workerPid") != nil && !validInt(get(r, "workerPid"), 1)) {
		return absent("worker_policy_process_mismatch")
	}
	pid, ticks := get(r, "pid"), get(r, "startTicks")
	if truth(get(r, "workerPid")) {
		pid, ticks = get(r, "workerPid"), get(r, "workerStartTicks")
	}
	if !validInt(pid, 1) || !validInt(ticks, 0) || !validInt(get(worker, "pid"), 1) || !validInt(get(worker, "startTicks"), 0) || !equal(get(worker, "pid"), pid) || !equal(get(worker, "startTicks"), ticks) {
		return absent("worker_policy_process_mismatch")
	}
	if !truth(get(r, "bootId")) || !equal(get(worker, "bootId"), get(r, "bootId")) || !equal(get(r, "bootId"), BootID()) {
		return absent("worker_policy_boot_mismatch")
	}
	info, err := os.Stat(s.Selection.DBPath())
	if err != nil {
		return absent("worker_policy_unreadable")
	}
	stat := info.Sys().(*syscall.Stat_t)
	identity := Object{}
	for _, k := range strings.Fields("token pid startTicks installationId stateDir socketPath storeId scopeRoot scopeAuthority") {
		identity = set(identity, k, get(r, k))
	}
	identity = set(identity, "dbDevice", int64(stat.Dev), "dbInode", int64(stat.Ino))
	if !sameObject(run, identity) || !truth(get(r, "token")) || s.StoreID == "" || get(r, "storeId") != s.StoreID || get(r, "installationId") != s.InstallationID || get(r, "stateDir") != s.Selection.Path || !equal(get(r, "socketPath"), nullable(s.Socket)) || get(r, "scopeRoot") != s.Scope.Root || get(r, "scopeAuthority") != s.Scope.Authority {
		return absent("worker_policy_service_mismatch")
	}
	h := OpenProcess(num(pid))
	defer h.Close()
	unavailable := func() bool {
		st := ProcessState(num(pid))
		return h.FD < 0 || st == "" || strings.ContainsAny(st, "TtZXx") || !equal(StartTicks(num(pid)), ticks)
	}
	if unavailable() {
		return absent("worker_policy_process_unavailable")
	}
	locks := []string{s.path("daemon.lock")}
	var scope Object
	if s.Socket != "" {
		scope = s.Scope.Read(s.Socket)
		if scope == nil {
			return absent("worker_policy_scope_mismatch")
		}
		for _, k := range strings.Fields("token pid startTicks bootId storeId installationId stateDir socketPath") {
			if !equal(get(scope, k), get(r, k)) {
				return absent("worker_policy_scope_mismatch")
			}
		}
		locks = append(locks, s.Scope.path(s.Socket, ".lock"))
	}
	for _, path := range locks {
		if !existingLockHeld(path) {
			return absent("worker_policy_lock_unheld")
		}
	}
	after, err := os.Stat(s.Selection.DBPath())
	if err != nil {
		return absent("worker_policy_unreadable")
	}
	r1, _ := encoded(r)
	r2, _ := encoded(s.Record())
	if !bytes.Equal(r1, r2) || !os.SameFile(info, after) || (s.Socket != "" && !sameObject(scope, s.Scope.Read(s.Socket))) || unavailable() {
		return absent("worker_policy_observation_changed")
	}
	return obj("observed", true, "reason", nil, "policy", policy, "worker", worker, "service", run, "observedAt", get(receipt, "observedAt"))
}
