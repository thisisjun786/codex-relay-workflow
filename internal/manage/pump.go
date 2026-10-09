package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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

	// QueueLegacyChecked marks a queue thread whose queue was searched for the pre-change ledger
	// records that could have carried its notices and left nothing to answer for. The pre-change pump
	// no longer writes such records, so the search runs until it finds nothing once, and never again.
	// A state written before the key existed reads as no thread searched.
	QueueLegacyChecked map[string]bool `json:"queue_legacy_checked"`

	extra map[string]json.RawMessage
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
		extra:              map[string]json.RawMessage{},
	}
}

// pumpLoadState reads the state document. A document that is not there is the first run; one that
// cannot be decoded is reported, so a round never continues on a state it misread.
func pumpLoadState(cfg *Config) (pumpState, error) {
	st := pumpNewState()
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpStateFile))
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
		{"queue_refused", st.QueueRefused}, {"queue_legacy_checked", st.QueueLegacyChecked},
	} {
		if err := put(field.key, field.value); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("crw manage pump: encode the state: %w", err)
	}
	path := filepath.Join(cfg.StateDir, pumpStateFile)
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
	file, err := os.OpenFile(filepath.Join(cfg.StateDir, pumpLockFile), os.O_CREATE|os.O_RDWR, 0o600)
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
		if ev.Kind == pumpKindQuestion || ev.Kind == pumpKindDag {
			urgent = true
		}
		if ev.Kind != pumpKindLow {
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
		if ev.Kind == pumpKindQuestion || ev.Kind == pumpKindDag {
			ordered = append(ordered, ev)
		}
	}
	for _, ev := range events {
		if ev.Kind != pumpKindQuestion && ev.Kind != pumpKindDag {
			ordered = append(ordered, ev)
		}
	}
	return ordered
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
	for _, source := range pumpSources {
		res := source.Collect(ctx, e, cfg, st, s)
		st.Sources[source.Name()] = res.Status
		if res.Status != pumpSourceOK {
			pumpLog(cfg, fmt.Sprintf("source %s %s", source.Name(), res.Status))
			continue
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
	pumpCollect(ctx, e, cfg, &st, s, now)
	// pending reaches disk before anything is sent, so a crash at any point leaves the events
	// recoverable and an offset never advances past an undelivered event. A management thread that
	// is not configured gates only the management batch: the parent queue targets the parent
	// threads, so it is still flushed.
	if !dry {
		if err := st.pumpSave(cfg); err != nil {
			return 1, err
		}
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
		attempt := pumpAttempt{LogicalID: pumpBatchID(batch), IDs: pumpPendingIDs(batch), Text: body}
		st.Attempt = &attempt
		if err := st.pumpSave(cfg); err != nil {
			return 1, err
		}
	}
	attempt := *st.Attempt
	out, err := Deliver(ctx, e, cfg, Message{
		LogicalID: attempt.LogicalID, Thread: cfg.ManagementThread, Text: attempt.Text,
		Settings: cfg.Settings.Management,
	})
	if err != nil && out.Class == "" {
		return 1, err
	}
	pumpLog(cfg, fmt.Sprintf("deliver request=%s events=%d class=%s received=%v applied=%v",
		out.RequestID, len(attempt.IDs), out.Class, out.Class == deliverClassAccepted, false))
	if out.Class != deliverClassAccepted {
		if out.Class == deliverClassUnknown {
			// The attempt stays frozen so the next round reconciles it under the same id.
			return 0, err
		}
		// A refusal is terminal and nothing was sent, so the freeze lifts: a later batch may carry
		// these events again, and a new event changes the logical id as usual.
		st.Attempt = nil
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
	if len(remaining) == 0 {
		st.FirstAt = nil
	}
	if err := st.pumpSave(cfg); err != nil {
		return 1, err
	}
	return 0, nil
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
	file, err := os.OpenFile(filepath.Join(cfg.StateDir, pumpLogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
}

// pumpSources is the one registry. A later node adds its own source by adding a line here; the
// order fixes the order the events appear in the delivered body.
var pumpSources = []pumpSource{
	pumpRolloutSource{},
	pumpPRSource{},
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
