package service

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"time"
)

func (s *Service) foreignMarkers(r Object) []string {
	out := []string{}
	if b := get(r, "bootId"); b != nil && !equal(b, BootID()) {
		out = append(out, "recorded before a different boot")
	}
	if text(get(r, "installationId")) != s.InstallationID {
		out = append(out, "another installation owns it")
	}
	if s.StoreID != "" && get(r, "storeId") != nil && get(r, "storeId") != s.StoreID {
		out = append(out, "it is using a different store")
	}
	return out
}
func (s *Service) storeUnreadable(r Object) string {
	if s.StoreID != "" || !s.StoreUnidentified || !truth(get(r, "storeId")) {
		return ""
	}
	return "a store is present here and its identity could not be read, so the store this record names (" + text(get(r, "storeId")) + ") cannot be compared against it"
}
func (s *Service) Ownership(r Object) (string, *ProcessHandle, string) {
	orphanBoot := "no boot id is recorded, so this worker pid cannot be distinguished from one reused after a reboot"
	if !truth(get(r, "pid")) {
		if truth(get(r, "workerPid")) {
			if f := s.foreignMarkers(r); len(f) > 0 {
				return "foreign", nil, strings.Join(f, "; ")
			}
			if d := s.storeUnreadable(r); d != "" {
				return "unverifiable", nil, d
			}
			if get(r, "bootId") == nil && BootID() != nil {
				return "unverifiable", nil, orphanBoot
			}
		}
		return "none", nil, "no daemon record"
	}
	f := s.foreignMarkers(r)
	h := OpenProcess(num(get(r, "pid")))
	if len(f) > 0 {
		return "foreign", h, strings.Join(f, "; ")
	}
	if h.Gone {
		if d := s.storeUnreadable(r); d != "" {
			return "unverifiable", h, d
		}
		if truth(get(r, "workerPid")) && get(r, "bootId") == nil && BootID() != nil {
			return "unverifiable", h, orphanBoot
		}
		return "none", h, "the recorded process is gone"
	}
	if h.FD < 0 {
		return "unverifiable", h, h.Detail
	}
	if d := s.storeUnreadable(r); d != "" {
		return "unverifiable", h, d
	}
	if get(r, "bootId") == nil && BootID() != nil {
		return "unverifiable", h, "no boot id is recorded, so a pid from before a reboot cannot be ruled out"
	}
	ticks := StartTicks(h.PID)
	if get(r, "startTicks") == nil || ticks == nil {
		return "unverifiable", h, "no start time is available for this pid, so identity cannot be established"
	}
	if !equal(ticks, get(r, "startTicks")) {
		return "foreign", h, "the pid was reused by a different process"
	}
	return "ours", h, ""
}
func (s *Service) workerIdentified(r Object) bool {
	pid := num(get(r, "workerPid"))
	if pid == 0 || (get(r, "bootId") == nil && BootID() != nil) {
		return false
	}
	h := OpenProcess(pid)
	defer h.Close()
	return h.FD >= 0 && get(r, "workerStartTicks") != nil && equal(StartTicks(pid), get(r, "workerStartTicks"))
}
func (s *Service) holderRefusal() Object {
	r := s.Record()
	owner, h, detail := s.Ownership(r)
	if h != nil {
		_ = h.Close()
	}
	markers := []string{}
	if r != nil {
		markers = s.foreignMarkers(r)
	}
	if len(markers) == 0 && (owner == "ours" || (owner == "none" && s.workerIdentified(r))) {
		return nil
	}
	reason := "ownership_unverifiable"
	if owner == "foreign" || len(markers) > 0 {
		reason = "not_ours"
	}
	if detail == "" {
		detail = strings.Join(markers, "; ")
	}
	if detail == "" {
		detail = "the daemon lock is held but nothing here identifies its owner"
	}
	return obj("ok", false, "reason", reason, "detail", detail)
}
func (s *Service) Enable(actor string) (out Object, err error) {
	f, err := lockIfFree(s.path("daemon.lock"))
	if err != nil {
		return nil, err
	}
	if f != nil {
		defer func() { err = errors.Join(err, f.Close()) }()
	} else if r := s.holderRefusal(); r != nil {
		return set(r, "intent", s.Intent(), "note", "intent is shared with the owner of this state directory and was left unchanged"), nil
	}
	intent, err := s.writeIntent(true, actor)
	return obj("ok", true, "reason", nil, "intent", intent), err
}
func (s *Service) Disable(actor string, timeout time.Duration) (Object, error) {
	f, err := lockIfFree(s.path("daemon.lock"))
	if err != nil {
		return nil, err
	}
	if f == nil {
		if r := s.holderRefusal(); r != nil {
			return set(r, "intent", s.Intent(), "stop", obj("ok", false, "reason", "refused", "supervisor", "untouched", "worker", "untouched"), "note", "intent is shared with the owner of this state directory and was left unchanged"), nil
		}
	}
	written, err := s.writeIntent(false, actor)
	if f != nil {
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		return nil, err
	}
	stopped, err := s.Stop(actor, timeout)
	failed := !truth(get(stopped, "ok")) && get(stopped, "reason") != "not_running"
	var reason any
	if failed {
		reason = get(stopped, "reason")
	}
	return obj("ok", !failed, "reason", reason, "intent", written, "stop", stopped), err
}

// Python observes exit at its 100ms grace boundaries, not at the first pidfd
// readiness edge. That interval also lets a PDEATHSIG worker finish exiting
// before stop re-reads its identity (service.py:_terminate).
func waitTermination(h *ProcessHandle, timeout time.Duration) bool {
	return terminationCadence(timeout, time.Now, func() bool { return h.Wait(0) }, func(delay time.Duration) { timer := time.NewTimer(delay); <-timer.C })
}

func terminationCadence(timeout time.Duration, now func() time.Time, exited func() bool, pause func(time.Duration)) bool {
	deadline := now().Add(timeout)
	for now().Before(deadline) {
		if exited() {
			return true
		}
		pause(100 * time.Millisecond)
	}
	return false
}
func terminate(h *ProcessHandle, timeout time.Duration) string {
	if !h.Send(unix.SIGTERM) {
		return "signal_refused"
	}
	if waitTermination(h, timeout) {
		return "exited"
	}
	h.Send(unix.SIGKILL)
	if waitTermination(h, timeout) {
		return "exited"
	}
	return "still_running"
}
func (s *Service) stopWorker(r Object, timeout time.Duration) string {
	pid := num(get(r, "workerPid"))
	if pid == 0 {
		return "gone"
	}
	h := OpenProcess(pid)
	defer h.Close()
	if h.Gone {
		return "gone"
	}
	if h.FD < 0 {
		return "unverifiable"
	}
	current := StartTicks(pid)
	if get(r, "workerStartTicks") == nil || current == nil {
		return "unverifiable"
	}
	if !equal(current, get(r, "workerStartTicks")) {
		return "gone"
	}
	return terminate(h, timeout)
}
func launchIdentity(r Object) string {
	if get(r, "launchId") != nil {
		return "launchId:" + text(get(r, "launchId"))
	}
	if get(r, "startedAt") != nil {
		return "startedAt:" + text(get(r, "startedAt")) + ":" + stringNumber(get(r, "startTicks"))
	}
	return "pid:" + stringNumber(get(r, "pid"))
}
func (s *Service) RequestStop() error {
	if err := os.MkdirAll(s.Selection.Path, 0700); err != nil {
		return err
	}
	return os.WriteFile(s.path("stop.request"), []byte(stamp()), 0666)
}
func (s *Service) StopRequested() bool { _, err := os.Stat(s.path("stop.request")); return err == nil }
func (s *Service) Stop(actor string, timeout time.Duration) (out Object, err error) {
	r := s.Record()
	owner, h, detail := s.Ownership(r)
	defer func() {
		if h != nil {
			err = errors.Join(err, h.Close())
		}
	}()
	absent := func() Object {
		return obj("ok", false, "reason", "not_running", "detail", nullable(detail), "supervisor", "gone", "worker", "gone")
	}
	if owner == "none" && !truth(get(r, "workerPid")) {
		f, e := lockIfFree(s.path("daemon.lock"))
		if e != nil {
			return nil, e
		}
		if f != nil {
			return absent(), f.Close()
		}
		if h != nil {
			_ = h.Close()
		}
		r = s.Record()
		owner, h, detail = s.Ownership(r)
		if owner == "none" && !truth(get(r, "workerPid")) {
			return obj("ok", false, "reason", "ownership_unverifiable", "detail", "the daemon lock is held by a process that has not yet recorded its identity", "supervisor", "untouched", "worker", "untouched"), nil
		}
	}
	if owner == "foreign" || owner == "unverifiable" {
		reason := "not_ours"
		if owner == "unverifiable" {
			reason = "ownership_unverifiable"
		}
		return obj("ok", false, "reason", reason, "detail", nullable(detail), "supervisor", "untouched", "worker", "untouched"), nil
	}
	if err = s.RequestStop(); err != nil {
		return nil, err
	}
	outcome := "gone"
	if owner != "none" {
		outcome = terminate(h, timeout)
	}
	fresh := s.Record()
	replaced := fresh != nil && r != nil && launchIdentity(fresh) != launchIdentity(r)
	if fresh != nil && !replaced {
		r = fresh
	}
	worker := s.stopWorker(r, timeout)
	replacement := func(detail, worker string) Object {
		return obj("ok", false, "reason", "replaced_by_new_launch", "detail", detail, "supervisor", outcome, "worker", worker)
	}
	if replaced {
		return replacement("a new launch acquired the daemon lock during this stop; it was left untouched and is still running", "untouched"), nil
	}
	supervisorDone := outcome == "exited" || outcome == "gone"
	workerDone := worker == "exited" || worker == "gone"
	if r != nil {
		cleared := set(r, "stoppedBy", actor)
		if supervisorDone {
			cleared = set(cleared, "pid", nil)
		}
		if workerDone {
			cleared = set(cleared, "workerPid", nil, "workerStartTicks", nil)
		}
		if supervisorDone && workerDone {
			cleared = set(cleared, "stoppedAt", stamp())
		}
		f, e := lockIfFree(s.path("daemon.lock"))
		if e != nil {
			return nil, e
		}
		if f == nil {
			if supervisorDone && workerDone {
				return replacement("the daemon lock was taken during this stop; that launch was left untouched and is still running", worker), nil
			}
		} else {
			defer func() { err = errors.Join(err, f.Close()) }()
			latest := s.Record()
			if latest != nil && launchIdentity(latest) != launchIdentity(r) {
				return replacement("a new launch published its record during this stop; it was left untouched and is still running", worker), nil
			}
			if err = s.WriteRecord(cleared); err != nil {
				return nil, err
			}
		}
	}
	if owner == "none" && worker == "gone" {
		return absent(), nil
	}
	var reason any
	if !supervisorDone || !workerDone {
		reason = "did_not_exit"
	}
	return obj("ok", supervisorDone && workerDone, "reason", reason, "detail", nil, "supervisor", outcome, "worker", worker), nil
}
