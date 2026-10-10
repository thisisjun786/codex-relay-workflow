// group.go is the proof that a process group is a job's after the job's shell has ended (CRW-1155). A group id is handed out again once
// the group is empty, so "the leader is gone and a group of that number answers signal 0" says nothing of whose group it is. Neither
// does the age of its members: a process enters a group (setpgid) long after it started, so a group that took the number later can hold
// only processes older than any moment the record knows. What a record can prove is who was in the group: while the job's shell was
// alive (its start token matched before and after the listing, under the store lock) the group of its number was the job's, and cancel
// wrote its members, each by pid and start, into the record (cancelGroup). A process that is one of them (same pid, same start) or
// whose parent is one of them is the job's; any other member makes the group unproven.

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

// groupKey is the key of the record that holds the members of the job's process group as cancel saw them while the job's shell was
// alive. It is not one of the thirteen keys of the oracle's record, so it is kept and written back like any key this port does not name.
const groupKey = "cancelGroup"

// procEntry is a process as ps lists it; Start is its lstart with the spaces collapsed, empty when it did not read.
type procEntry struct {
	PID, PPID, PGID int
	Start           string
}

// groupMember is a member of the job's group as the record keeps it.
type groupMember struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

// listProcesses is every process of the host. It is a variable so that a check can make it fail.
var listProcesses = psProcesses

// lstartLayout is how ps prints lstart in the C locale, in local time.
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// psProcesses reads "ps -A -o pid=,ppid=,pgid=,lstart=" in the C locale.
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
		start := strings.Join(f[3:], " ")
		if _, err := time.Parse(lstartLayout, start); err != nil {
			start = ""
		}
		procs = append(procs, procEntry{pid, ppid, pgid, start})
	}
	return procs, nil
}

// observeGroup is the members of the group that the shell with that pid leads. The caller has just shown the shell alive (its start
// token matched, ownsGroup) and holds the store lock; the token is checked again after the listing, so the shell was alive throughout
// and the number cannot have passed to another group in between. False when the shell is not shown alive after the listing, the list
// does not read, or no member's start reads.
func observeGroup(pid int, token string) ([]groupMember, bool) {
	procs, err := listProcesses()
	if err != nil || !startsAt(pid, token) {
		return nil, false
	}
	var members []groupMember
	for _, p := range procs {
		if p.PGID == pid && p.Start != "" {
			members = append(members, groupMember{p.PID, p.Start})
		}
	}
	return members, len(members) > 0
}

// groupProven is whether every process of the group pgid is the job's: it is a member the record saw (the same pid with the same
// start), or its parent is such a process of the group. A group with no process, or with a member nothing connects to what the record
// saw, is not proven, however old that member is.
func groupProven(procs []procEntry, pgid int, seen []groupMember) bool {
	saw := map[int]string{}
	for _, m := range seen {
		saw[m.PID] = m.Start
	}
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
			if start, ok := saw[m.PID]; ok && m.Start != "" && m.Start == start || proven[m.PPID] {
				proven[m.PID], changed = true, true
			}
		}
	}
	return len(proven) == len(members)
}

// recordedGroup is the members of the job's group that the record holds, when it holds a list that reads.
func recordedGroup(rec BgRecord) ([]groupMember, bool) {
	i := slices.IndexFunc(rec.Extra, func(m Member) bool { return m.Key == groupKey })
	if i < 0 {
		return nil, false
	}
	raw, ok := rec.Extra[i].Value.(json.RawMessage)
	var members []groupMember
	if !ok || json.Unmarshal(raw, &members) != nil {
		return nil, false
	}
	return members, len(members) > 0
}

// withGroup is the record with the members of its group set.
func withGroup(rec BgRecord, members []groupMember) BgRecord {
	raw, _ := json.Marshal(members)
	extra := slices.DeleteFunc(slices.Clone(rec.Extra), func(m Member) bool { return m.Key == groupKey })
	rec.Extra = append(extra, Member{groupKey, json.RawMessage(raw)})
	return rec
}

// ownsGroup is whether the record proves the process group of its pid is the job's: the shell it started is still there (its start
// token matches), or its cancel was requested, the shell is gone now, and every process of the group is one cancel saw in it while the
// shell was alive or a child of one (groupProven).
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
	seen, ok := recordedGroup(rec)
	if !ok {
		return false
	}
	procs, err := listProcesses()
	return err == nil && groupProven(procs, pid, seen)
}

// launchingKey marks a reservation: the record of a job that is published and not yet started. Like groupKey it is not one of
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

// settledKeys is the record without the keys that only a running record needs: the reservation mark once it is no longer a reservation,
// and the members of the job's group that a cancel saw once the job is over. A record that has left running is the oracle's thirteen
// keys again.
func settledKeys(rec BgRecord) BgRecord {
	drop := func(m Member) bool {
		return m.Key == launchingKey && (rec.PID != nil || rec.Status != StatusRunning) || m.Key == groupKey && IsTerminal(rec.Status)
	}
	if !slices.ContainsFunc(rec.Extra, drop) {
		return rec
	}
	rec.Extra = slices.DeleteFunc(slices.Clone(rec.Extra), drop)
	if len(rec.Extra) == 0 {
		rec.Extra = nil
	}
	return rec
}
