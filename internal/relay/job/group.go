// group.go is the proof that a process group is a job's after the job's shell has ended (CRW-1155). A group id is handed out again once
// the group is empty, so "the leader is gone and a group of that number answers signal 0" says nothing of whose group it is. What a
// record can prove is a time: while the job's shell was alive (its start token matched, under the store lock) its group existed, and
// cancel wrote that moment into the record (cancelRequestedAt). A group that took the number later was formed after the job's group had
// emptied, so every one of its members started after that moment; every member of the job's own group either started before it or was
// started by a member that did.

package job

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// requestedAtKey is the key of the record that holds the moment a cancel was requested for a shell it had shown alive. It is not one
// of the thirteen keys of the oracle's record, so it is kept and written back like any key this port does not name.
const requestedAtKey = "cancelRequestedAt"

// procEntry is a process as ps lists it; a zero Start is a start time that did not read.
type procEntry struct {
	PID, PPID, PGID int
	Start           time.Time
}

// listProcesses is every process of the host. It is a variable so that a check can make it fail.
var listProcesses = psProcesses

// psProcesses reads "ps -A -o pid=,ppid=,pgid=,lstart=" in the C locale, where lstart is "Mon Jan _2 15:04:05 2006" in local time.
func psProcesses() ([]procEntry, error) {
	cmd := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var procs []procEntry
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		start, _ := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(f[3:], " "), time.Local)
		procs = append(procs, procEntry{pid, ppid, pgid, start})
	}
	return procs, nil
}

// groupProven is whether every process of the group pgid is the job's: it started no later than the second of the moment seen (ps
// prints whole seconds), or its parent is such a process. A group that took the number afterwards was formed after the job's group had
// emptied, and a cancel that reaches this point has seen the job's group alive for cancelWait after that moment, so its members start
// at least that much later. A group with no process, or with a member whose start does not read, is not proven.
func groupProven(procs []procEntry, pgid int, seen time.Time) bool {
	limit := seen.Truncate(time.Second)
	var members []procEntry
	for _, p := range procs {
		if p.PGID == pgid {
			members = append(members, p)
		}
	}
	if len(members) == 0 {
		return false
	}
	proven := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for _, m := range members {
			if proven[m.PID] {
				continue
			}
			if !m.Start.IsZero() && !m.Start.After(limit) || proven[m.PPID] {
				proven[m.PID], changed = true, true
			}
		}
	}
	return len(proven) == len(members)
}

// requestedAt is the moment the record's cancel was requested for a shell shown alive, when it has one.
func requestedAt(rec BgRecord) (time.Time, bool) {
	i := slices.IndexFunc(rec.Extra, func(m Member) bool { return m.Key == requestedAtKey })
	if i < 0 {
		return time.Time{}, false
	}
	var s string
	switch v := rec.Extra[i].Value.(type) {
	case json.RawMessage:
		if json.Unmarshal(v, &s) != nil {
			return time.Time{}, false
		}
	case string:
		s = v
	}
	ms, ok := dateMs(s)
	return time.UnixMilli(ms), ok
}

// withRequestedAt is the record with the moment of its cancel request set.
func withRequestedAt(rec BgRecord, at string) BgRecord {
	extra := slices.DeleteFunc(slices.Clone(rec.Extra), func(m Member) bool { return m.Key == requestedAtKey })
	rec.Extra = append(extra, Member{requestedAtKey, at})
	return rec
}

// ownsGroup is whether the record proves the process group of its pid is the job's: the shell it started is still there (its start
// token matches), or its cancel was requested while the shell was, the shell is gone now, and every process of the group is shown to be
// the job's (groupProven).
func ownsGroup(rec BgRecord) bool {
	if rec.PID == nil || rec.StartToken == nil {
		return false
	}
	pid := *rec.PID
	if startsAt(pid, *rec.StartToken) {
		return true
	}
	if rec.Status != StatusCancelRequested || !PidGone(pid) {
		return false
	}
	seen, ok := requestedAt(rec)
	if !ok {
		return false
	}
	procs, err := listProcesses()
	return err == nil && groupProven(procs, pid, seen)
}

// launchingKey marks a reservation: the record of a job that is published and not yet started. Like requestedAtKey it is not one of
// the thirteen keys of the oracle's record. The record that names the job's pid, or that has left running, does not carry it.
const launchingKey = "launching"

// isReservation is whether the record is a reservation that has no pid yet.
func isReservation(rec BgRecord) bool {
	return rec.PID == nil && rec.Status == StatusRunning && slices.ContainsFunc(rec.Extra, func(m Member) bool {
		if raw, ok := m.Value.(json.RawMessage); ok {
			return m.Key == launchingKey && strings.TrimSpace(string(raw)) == "true"
		}
		return m.Key == launchingKey && m.Value == true
	})
}

// settledKeys is the record without the reservation mark once it is no longer a reservation.
func settledKeys(rec BgRecord) BgRecord {
	if rec.PID == nil && rec.Status == StatusRunning {
		return rec
	}
	rec.Extra = slices.DeleteFunc(slices.Clone(rec.Extra), func(m Member) bool { return m.Key == launchingKey })
	if len(rec.Extra) == 0 {
		rec.Extra = nil
	}
	return rec
}
