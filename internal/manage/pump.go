package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"golang.org/x/sys/unix"
)

// crw manage pump collects management events from a registered set of sources, batches them by
// the issue's rules, delivers one batch to the management thread through Deliver, and flushes the
// parent notification queue. Its lock, state and ledger live under the manage state directory, so
// they open even when the relay store is missing or stopped.
const (
	pumpStateFile = "pump-state.json"
	pumpLockFile  = "pump.lock"
	pumpLogFile   = "pump.log"

	// pumpLockReason is the refusal the issue fixes for a second pump on one state directory.
	pumpLockReason = "pump_locked"

	// The defaults the issue fixes; Section "pump" overrides each of them.
	pumpDefaultBatchSeconds       = 120
	pumpDefaultLowPrioritySeconds = 900
	pumpDefaultMaxQueueSeconds    = 3600
	pumpDefaultIssuePattern       = "[A-Z]+-\\d+"
	pumpDefaultInterval           = 30 * time.Second

	// pumpLockedExit is the status of a refused lock; usageExit (2) is the shared usage status.
	pumpLockedExit = 1

	// The event kinds. question and dag are urgent; low is the low-priority kind the batching
	// rule waits low_priority_seconds for.
	pumpKindReport   = "report"
	pumpKindQuestion = "question"
	pumpKindPR       = "pr"
	pumpKindLow      = "low"
	pumpKindDag      = "dag"
	pumpKindInfo     = "info"

	// The kinds of the upward half of a flow run (CRW-1204): a run's escalation, a run that died, a report the initiative lead's thread
	// could not be told, and a decision only Jun can make are urgent; a report nobody read is low.
	pumpKindRunEscalation     = "run_escalation"
	pumpKindRunDead           = "run_dead"
	pumpKindReportUndelivered = "report_undelivered"
	pumpKindJunDecision       = "jun_decision"
	pumpKindReportUnread      = "report_unread"

	// The two states a source reports: it read, or it could not. An unmeasured source emits no
	// event and is never turned into a "resolved" or "no anomaly" one.
	pumpSourceOK         = "ok"
	pumpSourceUnmeasured = "unmeasured"

	// The body limits the issue fixes for a rollout report and question.
	pumpReportLimit   = 6000
	pumpQuestionLimit = 3000
)

// errPumpLocked is the refusal a second pump reports; its text is the issue's reason word.
var errPumpLocked = errors.New(pumpLockReason)

// pumpSettings is the Section "pump" document. The scalars are pointers so an absent key keeps
// the issue's default, the pattern capacitySettings establishes.
type pumpSettings struct {
	BatchSeconds       *int   `json:"batch_seconds"`
	LowPrioritySeconds *int   `json:"low_priority_seconds"`
	MaxQueueSeconds    *int   `json:"max_queue_seconds"`
	IssuePattern       string `json:"issue_pattern"`
	Footer             string `json:"footer"`
	RolloutReports     *bool  `json:"rollout_reports"`
	// DagReviewSeconds is the seconds between DAG reviews (default 600) and CapacitySeconds the
	// seconds between capacity judgements (default 1800); see pump_signals.go.
	DagReviewSeconds *int `json:"dag_review_seconds"`
	CapacitySeconds  *int `json:"capacity_seconds"`
	// RunDeadSeconds is how long a flow run that did not finish may write no row before the pump reports it dead (default 1800).
	RunDeadSeconds *int `json:"run_dead_seconds"`
}

// pumpSettingsFrom reads the pump section of a configuration. An absent key leaves the default in
// place; a nil configuration yields the defaults alone.
func pumpSettingsFrom(cfg *Config) pumpSettings {
	var s pumpSettings
	if cfg != nil {
		_ = cfg.Section("pump", &s)
	}
	return s
}

// pumpSettingInt is one scalar setting with its default.
func pumpSettingInt(value *int, fallback int) int {
	if value == nil || *value <= 0 {
		return fallback
	}
	return *value
}

// pumpEvent is one collected event: a stable id (the batch hash and the dedup key), its kind and
// the text the management thread reads.
type pumpEvent struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// pumpState is the state document: the issue's four keys plus the per-source status, the relay
// cursors and the already-sent ids the parent contract adds. Unknown top-level keys a later node
// wrote are preserved verbatim on write.
type pumpState struct {
	Offsets map[string]int64  `json:"offsets"`
	Pending []pumpEvent       `json:"pending"`
	FirstAt *float64          `json:"first_at"`
	PRs     map[string]string `json:"prs"`
	Sources map[string]string `json:"sources"`
	Cursors map[string]string `json:"cursors"`
	Sent    map[string]bool   `json:"sent"`
	PRsSeen bool              `json:"prs_seen"`

	// Attempt is the batch an unsettled delivery was tried under. While it is set, the batch's
	// ids and text are frozen, so a newly collected event cannot change the logical id and skip
	// the reconciliation the issue requires.
	Attempt *pumpAttempt `json:"attempt"`
	// QueueAccepted is a queue thread's notices an accepted delivery carried but whose move to
	// sent/ did not finish. The next round completes the move before anything else, so an
	// accepted notice is never sent twice.
	QueueAccepted map[string][]string `json:"queue_accepted"`
	// PRSeq is the counter a detected PR change is numbered with. The event id carries it, so a
	// transition that happens twice mints two ids and the second is not dropped as one the sent
	// set already holds.
	PRSeq int `json:"pr_seq"`
	// QueueAttempt is a queue thread's batch whose delivery ended neither accepted nor refused.
	// While it is set the batch's notice names, body and logical id are frozen, so the next round
	// reconciles the same request id instead of forming a new batch around a notice that may
	// already have gone.
	QueueAttempt map[string]pumpReview776QueuePin `json:"queue_attempt"`

	// QueueRefused is a queue thread's count of the refusals one batch id has taken. The next batch
	// of that id is sent under the count as an ordinal, so the ledger's refusal of the earlier id is
	// not replayed. A state written before the key existed reads as no refusals.
	QueueRefused map[string]pumpQueueRefusal `json:"queue_refused"`

	// MgmtRefused is the count of refusals the management batch of one id has taken (CRW-1273), the
	// queue's rule for the management thread: the next attempt of that batch is sent under
	// <id>-m<count>, so neither the ledger's refusal of the earlier id nor a body whose HH:MM head
	// moved on is met again. A state without the key reads as no refusals.
	MgmtRefused pumpQueueRefusal `json:"mgmt_refused"`
	// Parked holds the events of a management batch the bridge refused pumpQueueRefusalLimit times
	// under one id. They leave pending so the events behind them flow, are never sent again, and stay
	// in the state with the reason, as the queue keeps its notices in refused/.
	Parked []pumpParkedEvent `json:"parked"`

	// QueueLegacyChecked marks a queue thread whose queue was searched for the pre-change ledger
	// records that could have carried its notices and left nothing to answer for beyond a pin that holds
	// the whole evidence: no record, only records that settled as never delivered, the one provable record
	// the pin adopts, a provable pin that answers for every record found, or an overlap pin whose records
	// all settled with nothing left waiting. A thread with a ledger record that could have carried a text
	// it may still send and that neither the search nor the pin answers for is never marked. The
	// pre-change pump no longer writes such records, so the search runs until then, and never
	// again; a thread holding a pin it has not been marked for (one another build left) is searched before
	// that pin is acted on. A state written before the key existed reads as no thread searched.
	QueueLegacyChecked map[string]bool `json:"queue_legacy_checked"`

	// Supervisor is what the supervisor queue keeps between rounds (pump_supervisor.go): the attempt ordinals of each notice, the notices
	// whose report_undelivered event was sent, and the deliveries whose report-delivered call is still to be made.
	Supervisor pumpSupervisorState `json:"supervisor"`

	// dry is set for the round being collected when it is a dry run, so a source reads without
	// writing anything of its own. commits are the steps the sources asked to run once the round's
	// pending events are durable (a source with a state of its own that must not move first).
	// dirty is set by a commit that changed this state again, which the round then saves once more.
	dry     bool
	commits []pumpCommitStep
	dirty   bool

	extra map[string]json.RawMessage
}

// pumpCommitStep is one source's step to run after the pending events are saved.
type pumpCommitStep struct {
	source string
	run    func() error
}

// pumpReview776QueuePin is one frozen queue batch: the notice names it carried, the body it sent
// and the logical id it was tried under. The names are what an accepted retry records as the
// membership, so a notice queued after the pin was taken is not moved to sent/. SHA256 is each
// member's delivered body digest, so an accepted batch moves only the members still carrying the
// text it sent.
type pumpReview776QueuePin struct {
	LogicalID string            `json:"logical_id"`
	Names     []string          `json:"names"`
	Body      string            `json:"body"`
	SHA256    map[string]string `json:"sha256,omitempty"`
	Accepted  bool              `json:"accepted,omitempty"`
	// Base is the batch id the pin's logical id carries before any refusal ordinal is appended. A
	// refusal is counted against it. A pin without it (written before the ordinal existed) is counted
	// against its logical id.
	Base string `json:"base,omitempty"`
	// Legacy marks a pin taken for pre-change ledger records whose body the ledger does not store. An
	// overlap pin carries it with digests that only tell a notice the producer wrote again from the one
	// the records were matched against; a notice is completed only when a record that proves its text
	// shows it delivered. A legacy pin without an overlap (one an earlier build of the queue wrote) cannot
	// prove which notices its attempt carried, so it holds the thread.
	Legacy bool `json:"legacy,omitempty"`
	// Held marks a pin whose pre-change attempt the ledger accepted but whose text is not
	// recoverable. The attempt covered the pin's names, so completing them by name could archive a
	// notice it never carried, and sending them under a new id could deliver one twice; the thread
	// waits while the pin holds, which is the queue's rule for a pin.
	Held bool `json:"held,omitempty"`
	// Overlap is the evidence set of a pin taken over the pre-change records that carried queued
	// notices, when there are several or one cannot prove its text. Each is reconciled on its own
	// receipt every round, and the pin's names and digests are the notices any of them carried. A notice
	// is completed once a provable record that carried it shows a delivery, and nothing is sent while a
	// record that carried an unproven notice is undetermined. A record that cannot prove its text marks
	// the pin Held: the thread sends nothing until an operator settles it, and the provable records are
	// still reconciled under the hold.
	Overlap []pumpReview776QueueLegacyRef `json:"overlap,omitempty"`
	// Judged lists the pre-change records the search that took an overlap pin matched over the queue
	// and found to answer for nothing queued: refused with proof of its text, or written before every
	// member it names was written again. An overlap pin never replays a text, so they stay answered while
	// it holds, and a later round that cannot match them by name (a member completed, or a queue past the
	// search's limit) does not hold on them as records nothing answers for.
	Judged []string `json:"judged,omitempty"`
}

// pumpReview776QueueLegacyRef is one pre-change record of an overlap pin: its logical id, the names
// its id hashes (Members) and the notices it carried (Names), those of its members that were not
// written again after it. Unprovable marks a record that cannot prove which text it carried -- one that
// carried only part of its members, or one whose message digest is not the digest of its members' text
// on disk -- with the Reason for the operator. Such a record holds the thread: its answer never
// completes a notice and never lets one be sent, while a notice another, provable record shows
// delivered is still completed.
type pumpReview776QueueLegacyRef struct {
	LogicalID  string   `json:"logical_id"`
	Names      []string `json:"names"`
	Members    []string `json:"members,omitempty"`
	Unprovable bool     `json:"unprovable,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	// Accepted marks the attempt of a pin the queue already held as accepted when the search folded that
	// pin into the evidence set: the pin's own accepted mark is its answer, whatever the ledger says.
	Accepted bool `json:"accepted,omitempty"`
}

// pumpQueueRefusal is a queue thread's count of the refusals one batch id has taken. The next batch
// of that id is sent under the ordinal, so the ledger's refusal of the earlier id is not replayed. A
// state without the key reads as no refusals.
type pumpQueueRefusal struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// pumpParkedEvent is an event that left pending after its batch was refused too often: the event,
// the bridge's last answer for the batch in one line, and when it was parked.
type pumpParkedEvent struct {
	pumpEvent
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// pumpAttempt is one frozen batch: the logical id it was tried under, the ids it carried, and its
// text. Freezing is what makes a second round reconcile the same request id.
type pumpAttempt struct {
	LogicalID string   `json:"logical_id"`
	IDs       []string `json:"ids"`
	Text      string   `json:"text"`
}

// pumpNewState is an empty state with every map allocated, so a source never writes into a nil map.
func pumpNewState() pumpState {
	return pumpState{
		Offsets: map[string]int64{}, PRs: map[string]string{}, Sources: map[string]string{},
		Cursors: map[string]string{}, Sent: map[string]bool{},
		QueueAccepted: map[string][]string{}, QueueAttempt: map[string]pumpReview776QueuePin{},
		QueueRefused:       map[string]pumpQueueRefusal{},
		QueueLegacyChecked: map[string]bool{},
		Supervisor:         newPumpSupervisorState(),
		extra:              map[string]json.RawMessage{},
	}
}

// pumpLoadState reads the state document. A document that is not there is the first run; one that
// cannot be decoded is reported, so a round never continues on a state it misread.
func pumpLoadState(cfg *Config) (pumpState, error) {
	st := pumpNewState()
	raw, err := os.ReadFile(crwconfig.JoinRoot(cfg.StateDir, pumpStateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, fmt.Errorf("crw manage pump: read the state: %w", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return st, fmt.Errorf("crw manage pump: decode the state: %w", err)
	}
	for key, value := range all {
		var err error
		switch key {
		case "offsets":
			err = json.Unmarshal(value, &st.Offsets)
		case "pending":
			err = json.Unmarshal(value, &st.Pending)
		case "first_at":
			err = json.Unmarshal(value, &st.FirstAt)
		case "prs":
			err = json.Unmarshal(value, &st.PRs)
		case "sources":
			err = json.Unmarshal(value, &st.Sources)
		case "cursors":
			err = json.Unmarshal(value, &st.Cursors)
		case "sent":
			err = json.Unmarshal(value, &st.Sent)
		case "prs_seen":
			err = json.Unmarshal(value, &st.PRsSeen)
		case "attempt":
			err = json.Unmarshal(value, &st.Attempt)
		case "queue_accepted":
			err = json.Unmarshal(value, &st.QueueAccepted)
		case "pr_seq":
			err = json.Unmarshal(value, &st.PRSeq)
		case "queue_attempt":
			err = json.Unmarshal(value, &st.QueueAttempt)
		case "queue_refused":
			err = json.Unmarshal(value, &st.QueueRefused)
		case "queue_legacy_checked":
			err = json.Unmarshal(value, &st.QueueLegacyChecked)
		case "supervisor":
			err = json.Unmarshal(value, &st.Supervisor)
		case "mgmt_refused":
			err = json.Unmarshal(value, &st.MgmtRefused)
		case "parked":
			err = json.Unmarshal(value, &st.Parked)
		default:
			// A key this file does not name belongs to a later node; it is preserved on write.
			st.extra[key] = value
		}
		if err != nil {
			return st, fmt.Errorf("crw manage pump: decode the state key %q: %w", key, err)
		}
	}
	if st.Offsets == nil {
		st.Offsets = map[string]int64{}
	}
	if st.PRs == nil {
		st.PRs = map[string]string{}
	}
	if st.Sources == nil {
		st.Sources = map[string]string{}
	}
	if st.Cursors == nil {
		st.Cursors = map[string]string{}
	}
	if st.Sent == nil {
		st.Sent = map[string]bool{}
	}
	if st.QueueAccepted == nil {
		st.QueueAccepted = map[string][]string{}
	}
	if st.QueueAttempt == nil {
		st.QueueAttempt = map[string]pumpReview776QueuePin{}
	}
	if st.QueueRefused == nil {
		st.QueueRefused = map[string]pumpQueueRefusal{}
	}
	if st.QueueLegacyChecked == nil {
		st.QueueLegacyChecked = map[string]bool{}
	}
	st.Supervisor.fill()
	return st, nil
}

// pumpSave writes the state document atomically: the known keys plus every unknown key preserved
// verbatim, through the package's temp-file, fsync, rename and directory-sync write.
func (st pumpState) pumpSave(cfg *Config) error {
	out := map[string]json.RawMessage{}
	for key, value := range st.extra {
		out[key] = value
	}
	put := func(key string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("crw manage pump: encode the state key %q: %w", key, err)
		}
		out[key] = raw
		return nil
	}
	for _, field := range []struct {
		key   string
		value any
	}{
		{"offsets", st.Offsets}, {"pending", st.Pending}, {"first_at", st.FirstAt},
		{"prs", st.PRs}, {"sources", st.Sources}, {"cursors", st.Cursors}, {"sent", st.Sent},
		{"prs_seen", st.PRsSeen}, {"attempt", st.Attempt}, {"queue_accepted", st.QueueAccepted},
		{"pr_seq", st.PRSeq}, {"queue_attempt", st.QueueAttempt},
		{"queue_refused", st.QueueRefused}, {"queue_legacy_checked", st.QueueLegacyChecked}, {"supervisor", st.Supervisor},
		{"mgmt_refused", st.MgmtRefused}, {"parked", st.Parked},
	} {
		if err := put(field.key, field.value); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("crw manage pump: encode the state: %w", err)
	}
	path := crwconfig.JoinRoot(cfg.StateDir, pumpStateFile)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("crw manage pump: the state file is a symlink; refusing to write through it")
	}
	if err := deliverWriteAtomic(path, append(data, 0x0a)); err != nil {
		return fmt.Errorf("crw manage pump: write the state: %w", err)
	}
	return nil
}

// pumpLock takes the state directory's pump lock. The kernel releases it when the process ends,
// so a crash cannot leave it held. A lock already held is errPumpLocked.
func pumpLock(cfg *Config) (func(), error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("crw manage pump: the state directory: %w", err)
	}
	file, err := os.OpenFile(crwconfig.JoinRoot(cfg.StateDir, pumpLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("crw manage pump: the lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errPumpLocked
		}
		return nil, fmt.Errorf("crw manage pump: lock: %w", err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

// pumpLockOrReport takes the lock and, when another pump holds it, writes the issue's JSON
// refusal to stdout and returns exit 1. A held lock is the only refusal that is not an error: it
// is the specified behaviour of a second pump.
func pumpLockOrReport(e *Env, cfg *Config) (func(), int) {
	release, err := pumpLock(cfg)
	if err != nil {
		if errors.Is(err, errPumpLocked) {
			fmt.Fprintln(e.Stdout, "{\"ok\":false,\"reason\":\"pump_locked\"}")
			return nil, pumpLockedExit
		}
		fmt.Fprintf(e.Stderr, "crw manage pump: error: %v\n", err)
		return nil, 1
	}
	return release, 0
}

// pumpLockRefusedExit takes the lock the way the command does and reports the exit the refusal
// means, writing the issue's JSON to the environment's stdout. It is the lock gate a round runs
// behind, exposed so a test pins the refusal without driving the whole command.
func pumpLockRefusedExit(e *Env, cfg *Config) int {
	release, code := pumpLockOrReport(e, cfg)
	if release == nil {
		return code
	}
	release()
	return 0
}

// pumpBatchID is one batch's logical id: the first 16 hex characters of sha256 over the JSON
// array of the pending event ids. The array is an unambiguous encoding, so no id boundary can
// collide; the same batch reuses the same id, which is what lets Deliver reconcile instead of
// re-send.
func pumpBatchID(events []pumpEvent) string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return pumpBatchIDStrings(ids)
}

// pumpBatchIDStrings is the same id over a list of names, for the queue's file names.
func pumpBatchIDStrings(names []string) string {
	data, err := json.Marshal(names)
	if err != nil {
		data = []byte(strings.Join(names, "\x00"))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// pumpDue reports whether the pending set should be delivered now, and whether it is urgent:
// question or dag immediately, any other non-low after batch_seconds, low-only after
// low_priority_seconds. It is a pure function of the injected clock, so tests pin the rule.
func pumpDue(pending []pumpEvent, firstAt *float64, now time.Time, s pumpSettings) (bool, bool) {
	if len(pending) == 0 || firstAt == nil {
		return false, false
	}
	urgent, normal := false, false
	for _, ev := range pending {
		if pumpKindUrgent(ev.Kind) {
			urgent = true
		}
		if !pumpKindLowPriority(ev.Kind) {
			normal = true
		}
	}
	if urgent {
		return true, true
	}
	age := now.Sub(time.Unix(int64(*firstAt), 0)).Seconds()
	if normal && age >= float64(pumpSettingInt(s.BatchSeconds, pumpDefaultBatchSeconds)) {
		return true, false
	}
	if age >= float64(pumpSettingInt(s.LowPrioritySeconds, pumpDefaultLowPrioritySeconds)) {
		return true, false
	}
	return false, false
}

// pumpBatchLimit bounds one batch's body below the delivery core's message limit, so a burst of
// events is split into stable prefixes rather than forming a batch that can never be accepted.
const pumpBatchLimit = 90000

// pumpBatchPrefix is the longest prefix of the delivery order whose body stays inside the batch
// limit. An urgent round delivers the urgent events (question, dag) first in their collected
// order, then the rest in their collected order, so an urgent event collected last still lands in
// the first batch. The order is deterministic for a given pending set, so the frozen attempt and
// its logical id are stable across rounds.
func pumpBatchPrefix(events []pumpEvent, now time.Time, urgent bool, footer string) []pumpEvent {
	if urgent {
		events = pumpReview776UrgentFirst(events)
	}
	for n := len(events); n > 0; n-- {
		if len(pumpBody(events[:n], now, urgent, footer)) <= pumpBatchLimit {
			return events[:n]
		}
	}
	return events[:1]
}

// pumpReview776UrgentFirst is the urgent delivery order: the urgent events in their collected
// order first, then the rest in their collected order. It is a stable partition, so the same
// pending set always produces the same order and therefore the same logical id.
func pumpReview776UrgentFirst(events []pumpEvent) []pumpEvent {
	ordered := make([]pumpEvent, 0, len(events))
	for _, ev := range events {
		if pumpKindUrgent(ev.Kind) {
			ordered = append(ordered, ev)
		}
	}
	for _, ev := range events {
		if !pumpKindUrgent(ev.Kind) {
			ordered = append(ordered, ev)
		}
	}
	return ordered
}

// pumpKindUrgent says whether an event of the kind is delivered at once: a question, a DAG anomaly, and the upward kinds of a flow run
// that need somebody now.
func pumpKindUrgent(kind string) bool {
	switch kind {
	case pumpKindQuestion, pumpKindDag, pumpKindRunEscalation, pumpKindRunDead, pumpKindReportUndelivered, pumpKindJunDecision:
		return true
	}
	return false
}

// pumpKindLowPriority says whether an event of the kind waits low_priority_seconds.
func pumpKindLowPriority(kind string) bool {
	return kind == pumpKindLow || kind == pumpKindReportUnread
}

// pumpBody is the delivered text: the issue's first line, the event bodies, then the configured
// footer.
func pumpBody(events []pumpEvent, now time.Time, urgent bool, footer string) string {
	head := fmt.Sprintf("[management events %s] %d events", now.Format("15:04"), len(events))
	if urgent {
		head += " (urgent)"
	}
	parts := make([]string, 0, len(events)+1)
	parts = append(parts, head)
	for _, ev := range events {
		parts = append(parts, ev.Text)
	}
	body := strings.Join(parts, "\n\n")
	if footer != "" {
		body += "\n\n" + footer
	}
	return body
}

// pumpCollect runs every registered source, records each one's status, and merges the new events
// into pending. An event whose id is already pending or already sent is not added again, so a
// re-read after a restart or a recovery never duplicates it. first_at is the batching clock: it
// is set when the first undelivered event arrives and cleared when the batch is accepted.
func pumpCollect(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings, now time.Time) {
	seen := map[string]bool{}
	for _, ev := range st.Pending {
		seen[ev.ID] = true
	}
	// A parked event was given up after its batch was refused: it is not collected again.
	for _, ev := range st.Parked {
		seen[ev.ID] = true
	}
	for _, source := range pumpSources {
		res := source.Collect(ctx, e, cfg, st, s)
		st.Sources[source.Name()] = res.Status
		if res.Status != pumpSourceOK {
			pumpLog(cfg, fmt.Sprintf("source %s %s", source.Name(), res.Status))
			continue
		}
		if res.Commit != nil {
			st.commits = append(st.commits, pumpCommitStep{source: source.Name(), run: res.Commit})
		}
		for _, ev := range res.Events {
			if ev.ID == "" || seen[ev.ID] || st.Sent[ev.ID] {
				continue
			}
			seen[ev.ID] = true
			st.Pending = append(st.Pending, ev)
			if st.FirstAt == nil {
				first := float64(now.Unix())
				st.FirstAt = &first
			}
		}
	}
}

// pumpRound is one pump cycle: load the state, collect, persist pending BEFORE delivering,
// deliver the due batch, and flush the parent queue. A dry run prints the body and saves no
// state.
func pumpRound(ctx context.Context, e *Env, cfg *Config, s pumpSettings, dry bool) (int, error) {
	now := e.Now()
	if err := ctx.Err(); err != nil {
		// A cancelled round makes no durable change: nothing is collected, saved or delivered.
		return 1, err
	}
	st, err := pumpLoadState(cfg)
	if err != nil {
		return 1, err
	}
	st.dry = dry
	pumpCollect(ctx, e, cfg, &st, s, now)
	// pending reaches disk before anything is sent, so a crash at any point leaves the events
	// recoverable and an offset never advances past an undelivered event. A management thread that
	// is not configured gates only the management batch: the parent queue targets the parent
	// threads, so it is still flushed.
	if !dry {
		if err := st.pumpSave(cfg); err != nil {
			return 1, err
		}
		// A source's own state moves only now that its events are durable. A failure is that
		// source's alone: it is logged, and its next read converges on the same event ids.
		for _, step := range st.commits {
			if err := step.run(); err != nil {
				pumpLog(cfg, "source "+step.source+" commit: "+err.Error())
			}
		}
		// What a commit recorded about itself is saved too. Losing that save only makes the next
		// read treat the commit as not made, which converges on the same ids.
		if st.dirty {
			if err := st.pumpSave(cfg); err != nil {
				pumpLog(cfg, "state save after the commits: "+err.Error())
			}
		}
	}
	st.commits, st.dirty = nil, false
	// The supervisor queue (CRW-1204) is flushed before the management batch, so an event it raises (a report nobody could tell the
	// initiative lead's thread about) is in this round's batch.
	if err := pumpSupervisorFlush(ctx, e, cfg, &st, s, now, dry); err != nil {
		pumpLog(cfg, "supervisor queue flush: "+err.Error())
	}
	switch {
	case cfg.ManagementThread == "":
		pumpLog(cfg, "no management_thread; the management batch waits, the parent queue still runs")
	case st.Attempt != nil:
		// An unsettled attempt is reconciled first, under its own frozen id, before any new batch
		// is formed.
		if dry {
			fmt.Fprintln(e.Stdout, st.Attempt.Text)
		} else if code, err := pumpDeliver(ctx, e, cfg, &st, nil, ""); err != nil {
			return code, err
		}
	default:
		if due, urgent := pumpDue(st.Pending, st.FirstAt, now, s); due {
			batch := pumpBatchPrefix(st.Pending, now, urgent, s.Footer)
			body := pumpBody(batch, now, urgent, s.Footer)
			if dry {
				fmt.Fprintln(e.Stdout, body)
			} else if code, err := pumpDeliver(ctx, e, cfg, &st, batch, body); err != nil {
				return code, err
			}
		}
	}
	if err := pumpQueueFlush(ctx, e, cfg, &st, s, dry); err != nil {
		// A failed flush keeps the notices queued for the next round; it never fails the round.
		pumpLog(cfg, "queue flush: "+err.Error())
	}
	return 0, nil
}

// pumpDeliver sends the pending batch and clears it only on accepted. On unknown or refused the
// batch stays pending, and the next round reconciles it under the same logical id.
func pumpDeliver(ctx context.Context, e *Env, cfg *Config, st *pumpState, batch []pumpEvent, body string) (int, error) {
	// The attempt is frozen before the send: an event collected while it is in flight cannot change
	// the logical id, so a later round reconciles the same request id instead of resending a batch
	// that may already have been accepted. An attempt already frozen is reused as it stands.
	if st.Attempt == nil {
		// A batch the ledger refused under this id is tried again under the next ordinal of the id
		// (the rule of the parent queue, CRW-991): the refusal of the id itself is final in the
		// ledger, and the body's HH:MM head moves on, so the same id would meet either the replayed
		// refusal or "already belongs to another message". The ordinal is -m<count>, never -r<count>:
		// Deliver's own retries go out under <logical id>-r<n> request ids, so after an outage that
		// spent B, B-r1 and B-r2 a logical id B-r1 would reach the ledger as the request id of a
		// retry that already holds another body, and the bridge would answer ErrConflict for ever
		// (CRW-1273).
		base := pumpBatchID(batch)
		logicalID := base
		if refusal := st.MgmtRefused; refusal.ID == base && refusal.Count > 0 {
			logicalID = pumpMgmtOrdinalID(base, refusal.Count)
		}
		attempt := pumpAttempt{LogicalID: logicalID, IDs: pumpPendingIDs(batch), Text: body}
		st.Attempt = &attempt
		if err := st.pumpSave(cfg); err != nil {
			return 1, err
		}
	}
	attempt := *st.Attempt
	// The attempts the outbox holds before the call tell, afterwards, whether its last answer is this
	// call's own (pumpDeliverReason).
	attemptsBefore := pumpOutboxAttempts(cfg, attempt.LogicalID)
	message := Message{
		LogicalID: attempt.LogicalID, Thread: cfg.ManagementThread, Text: attempt.Text,
		Settings: cfg.Settings.Management,
	}
	var out Outcome
	var err error
	if deliverRecordStranded(cfg, message) {
		// The frozen attempt's own outbox record belongs to another message now (the management
		// thread was changed), so Deliver would refuse it locally in every round and the attempt
		// would never leave the freeze. The bridge's answer for its request id does not depend on the
		// thread: it settles the attempt, which is then accepted, counted, closed as stranded or kept.
		out, err = deliverReconcileStranded(ctx, e, cfg, message)
	} else {
		out, err = Deliver(ctx, e, cfg, message)
	}
	if err != nil && out.Class == "" {
		return 1, err
	}
	line := fmt.Sprintf("deliver request=%s events=%d class=%s received=%v applied=%v",
		out.RequestID, len(attempt.IDs), out.Class, out.Class == deliverClassAccepted, false)
	if out.Class != deliverClassAccepted {
		// The bridge's answer rides on the line, in one line: the outbox holds it too, but a person
		// reading the pump log sees why a batch did not go (CRW-1273).
		line += " reason=" + pumpDeliverReason(cfg, attempt.LogicalID, attemptsBefore, err)
	}
	pumpLog(cfg, line)
	if out.Class != deliverClassAccepted {
		if out.Class == deliverClassUnknown {
			// The attempt stays frozen so the next round reconciles it under the same id.
			return 0, err
		}
		// A refusal clears the freeze only when the ledger proves the frozen attempt itself was
		// refused (the parent queue's rule for its pin). The delivery core reports a local refusal (an
		// unset execution policy, for example) before it reads the ledger, and a terminal refusal
		// whose outbox write failed leaves the record unsettled: neither says anything about an
		// attempt that may already have gone, so the attempt stays frozen and the next round
		// reconciles the same id and body.
		if !pumpReview776QueueRefusalSettled(cfg, attempt.LogicalID) {
			return 0, err
		}
		// A refusal the ledger proves is terminal and nothing was sent, so the freeze lifts: a later
		// batch may carry these events again under the next ordinal of its id. It is counted against
		// the batch; the pumpQueueRefusalLimit-th refusal of one id parks its events.
		st.Attempt = nil
		if status, _ := out.Receipt["status"].(string); status == deliverStranded {
			// A record of another thread whose receipt places nothing was closed: the message may have
			// reached that thread, the configured one has never seen these events. They go to it under
			// the next ordinal of the batch (or in a batch with new events). The close spends the
			// ordinal but is no refusal of the bridge, so it never parks the events by itself.
			old, _ := out.Receipt["thread"].(string)
			pumpLog(cfg, fmt.Sprintf("management attempt %s closed as stranded: it may have reached thread %s; its %d events go to %s under a new id",
				attempt.LogicalID, old, len(attempt.IDs), cfg.ManagementThread))
			pumpMgmtStrandedCount(st, attempt)
		} else {
			pumpMgmtRefusalCount(cfg, st, attempt)
		}
		if err := st.pumpSave(cfg); err != nil {
			return 1, err
		}
		return 0, err
	}
	delivered := map[string]bool{}
	for _, id := range attempt.IDs {
		st.Sent[id] = true
		delivered[id] = true
	}
	// Only the attempted events clear; an event that arrived while the batch was in flight stays
	// pending under a fresh batch.
	remaining := make([]pumpEvent, 0, len(st.Pending))
	for _, ev := range st.Pending {
		if !delivered[ev.ID] {
			remaining = append(remaining, ev)
		}
	}
	st.Pending, st.Attempt = remaining, nil
	st.MgmtRefused = pumpQueueRefusal{}
	if len(remaining) == 0 {
		st.FirstAt = nil
	}
	if err := st.pumpSave(cfg); err != nil {
		return 1, err
	}
	return 0, nil
}

// pumpMgmtOrdinalID is the logical id of a management batch after count refusals of its base id. The
// form carries no "-r": deliverRetryRequestID mints <logical id>-r<n> for Deliver's own retries, and
// an ordinal of that form would be the request id one of them already used. Base and ordinal stay
// well inside the 128 characters the outbox allows.
func pumpMgmtOrdinalID(base string, count int) string {
	return fmt.Sprintf("%s-m%d", base, count)
}

// pumpOutboxAttempts is how many attempts the outbox holds for a logical id, zero when there is no
// record.
func pumpOutboxAttempts(cfg *Config, logicalID string) int {
	record, known, err := deliverLoad(cfg, logicalID)
	if err != nil || !known {
		return 0
	}
	return len(record.Attempts)
}

// pumpDeliverReason is why a batch did not go, on one line for the pump log: the bridge's last answer
// the ledger keeps for the logical id when this call added one (or answered a settled record from it),
// else the delivery core's own error for this call (a refusal before any bridge call, such as an unset
// execution policy, or a bridge that did not start), cut short. The ledger's older excerpt is never
// the reason of a call that did not write one.
func pumpDeliverReason(cfg *Config, logicalID string, attemptsBefore int, cause error) string {
	reason := pumpQueueRefusalReason(cfg, logicalID)
	if cause != nil && pumpOutboxAttempts(cfg, logicalID) <= attemptsBefore {
		reason = strings.Join(strings.Fields(cause.Error()), " ")
	} else if (reason == "no receipt excerpt" || reason == "refused") && cause != nil {
		reason = strings.Join(strings.Fields(cause.Error()), " ")
	}
	if len(reason) > 200 {
		reason = strings.ToValidUTF8(reason[:200], "") + "..."
	}
	return reason
}

// pumpMgmtStrandedCount spends the next ordinal of the batch's base id for an attempt closed as
// stranded (deliverReconcileStranded), so its events go out under an id of their own, without the park
// a bridge refusal at the limit takes.
func pumpMgmtStrandedCount(st *pumpState, attempt pumpAttempt) {
	base := pumpBatchIDStrings(attempt.IDs)
	count := 1
	if st.MgmtRefused.ID == base {
		count = st.MgmtRefused.Count + 1
	}
	st.MgmtRefused = pumpQueueRefusal{ID: base, Count: count}
}

// pumpMgmtRefusalCount counts one proven refusal against the management batch's base id, as
// pumpQueueRefusedLift does for the queue. Below the limit the batch stays pending and goes out
// under its next ordinal; the limit-th refusal of one id parks the batch's events, with a log line
// naming each, so the events behind them flow. A count of another id restarts at one.
func pumpMgmtRefusalCount(cfg *Config, st *pumpState, attempt pumpAttempt) {
	base := pumpBatchIDStrings(attempt.IDs)
	count := 1
	if st.MgmtRefused.ID == base {
		count = st.MgmtRefused.Count + 1
	}
	if count < pumpQueueRefusalLimit {
		st.MgmtRefused = pumpQueueRefusal{ID: base, Count: count}
		return
	}
	reason := pumpQueueRefusalReason(cfg, attempt.LogicalID)
	parked := map[string]bool{}
	for _, id := range attempt.IDs {
		parked[id] = true
	}
	remaining := make([]pumpEvent, 0, len(st.Pending))
	at := time.Now().UTC().Format(time.RFC3339)
	for _, ev := range st.Pending {
		if !parked[ev.ID] {
			remaining = append(remaining, ev)
			continue
		}
		st.Parked = append(st.Parked, pumpParkedEvent{pumpEvent: ev, Reason: reason, At: at})
		pumpLog(cfg, fmt.Sprintf("management event %s parked after %d refusals: %s", ev.ID, count, reason))
	}
	st.Pending, st.MgmtRefused = remaining, pumpQueueRefusal{}
	if len(remaining) == 0 {
		st.FirstAt = nil
	}
}

// pumpPendingIDs is the ids of one batch, in order.
func pumpPendingIDs(events []pumpEvent) []string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return ids
}

// pumpLog appends one line to the pump log. It is best effort: a log that cannot be written
// never fails a round. The vocabulary is accepted, received and applied; "read" and
// "acknowledged" belong to the relay channel alone.
func pumpLog(cfg *Config, message string) {
	if cfg == nil {
		return
	}
	line := time.Now().UTC().Format(time.RFC3339) + " " + message + "\n"
	dir := cfg.StateDir
	if cfg.logDir != "" {
		// paths.log_root names where this host keeps logs; the directory is made on first use
		dir = cfg.logDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return
		}
	}
	file, err := os.OpenFile(crwconfig.JoinRoot(dir, pumpLogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(line)
	_ = file.Close()
}

// pumpSource is one event source. Every event carries a stable id, and the pump dedups all of them
// through the sent set, so an id a relay owns is sent once across a restart exactly like the ids
// the pump's own sources mint.
type pumpSource interface {
	Name() string
	Collect(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult
}

// pumpSourceResult is one source's reading: its status and the events it produced. An unmeasured
// status carries no events.
type pumpSourceResult struct {
	Status string
	Events []pumpEvent
	// Commit, when set, persists a state the source keeps outside the pump's. The pump runs it
	// after the round's pending events are durable and never in a dry run, so an event is never
	// lost to a state that moved first; a Commit that fails or is never reached is repeated by the
	// next read, which produces the same event ids.
	Commit func() error
}

// pumpSources is the one registry. A later node adds its own source by adding a line here; the
// order fixes the order the events appear in the delivered body.
var pumpSources = []pumpSource{
	pumpRolloutSource{},
	pumpPRSource{},
	pumpSignalDagSource{},
	pumpSignalCapacitySource{},
	pumpSignalAuditSource{},
	pumpFlowRunsSource{},
	pumpFlowReportsSource{},
}

// pumpQueueSortedNames lists one queue directory's notice file names, sorted, so the body and
// the logical id are stable across rounds.
func pumpQueueSortedNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".txt") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

var pumpCommand = Command{Name: "pump", Summary: "collect management events and deliver them to the management thread", Run: pumpRun}

func init() { Register(pumpCommand) }

// pumpRun is crw manage pump [--once] [--dry-run] [--interval N]. A second pump on the same
// state directory is refused with the issue's JSON and exit 1.
func pumpRun(ctx context.Context, e *Env, args []string) int {
	const usage = "usage: crw manage pump [--once] [--dry-run] [--interval N]"
	once, dry := false, false
	interval := pumpDefaultInterval
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--once":
			once = true
		case args[i] == "--dry-run":
			dry = true
		case args[i] == "--interval":
			if i+1 >= len(args) {
				fmt.Fprintln(e.Stderr, usage)
				fmt.Fprintln(e.Stderr, "crw manage pump: error: argument --interval: expected one argument")
				return usageExit
			}
			i++
			seconds, err := strconv.Atoi(args[i])
			if err != nil || seconds < 1 {
				fmt.Fprintln(e.Stderr, usage)
				fmt.Fprintf(e.Stderr, "crw manage pump: error: argument --interval: %q is not a positive number of seconds\n", args[i])
				return usageExit
			}
			interval = time.Duration(seconds) * time.Second
		case args[i] == "-h" || args[i] == "--help":
			fmt.Fprintln(e.Stdout, usage)
			return 0
		default:
			fmt.Fprintln(e.Stderr, usage)
			fmt.Fprintf(e.Stderr, "crw manage pump: error: unexpected argument %q\n", args[i])
			return usageExit
		}
	}
	cfg := coreDefaults(e)
	if problem := coreDeliverSettingsProblem(cfg); problem != "" {
		// A pump that cannot deliver would collect events and see every send refused: it does not
		// start. A dry run sends nothing, so it only warns.
		if !dry {
			fmt.Fprintf(e.Stderr, "crw manage pump: error: %s\n", problem)
			return usageExit
		}
		fmt.Fprintf(e.Stderr, "crw manage pump: warning: %s\n", problem)
	}
	release, code := pumpLockOrReport(e, cfg)
	if release == nil {
		return code
	}
	defer release()
	for {
		if code, err := pumpRound(ctx, e, cfg, pumpSettingsFrom(cfg), dry); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage pump: error: %v\n", err)
			if once || dry {
				// A one-round run reports the failure; the long-running pump logs it and keeps
				// its loop, so one failed delivery never stops the pump as if it had succeeded.
				return code
			}
		}
		if once || dry {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(interval):
		}
	}
}
