package manage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// The pump's signal sources (CRW-692): the DAG review, the capacity judgement and the audit alert
// queue, the management tools' dag_events, capacity_events and eval_events. Each is registered in
// the one pumpSources list. Every package-level name here starts with pumpSignal so the package's
// single namespace stays collision free.
//
// What a source keeps lives in the pump state, so it reaches disk in the same write as the pending
// events it produced: the last read time and the DAG's anomaly set and change counter under
// namespaced Cursors keys, and the audit queue's byte offset in Offsets. Sources[name] stays the
// read status alone.
//
// Durability. An event id is stable, so the pump's dedupe (pending, then the sent set) makes a
// repeated read harmless. The capacity judgement keeps its own alerted state, and that state must
// not move before the alert is durable in the pump's pending events: the source evaluates (a pure
// read), the pump saves the pending events, and only then runs the source's Commit. A crash or a
// failed commit in between leaves the alerted state as it was, so the next read judges the same
// alert under the same alert epoch and produces the same event id.
const (
	pumpDefaultDagReviewSeconds = 600
	pumpDefaultCapacitySeconds  = 1800

	pumpSignalDagName      = "signal_dag"
	pumpSignalRefusalsName = "signal_dag_refusals"
	pumpSignalCapacityName = "signal_capacity"
	pumpSignalAuditName    = "signal_audit"

	// pumpSignalDagPinFile, in the state directory, keeps each parent rollout's first-sight boundary
	// (see pumpSignalDagOffsetStore.pin) until the pump state holds the rollout's offsets.
	pumpSignalDagPinFile = "pump-dag-pins.json"

	// The Cursors keys. The read times are Unix seconds; the DAG set is the sorted anomaly tuples as
	// JSON; the change counter numbers each change of that set; the audit epoch counts the times the
	// alert file shrank.
	pumpSignalDagReadKey    = "signal.dag.read_at"
	pumpSignalDagSetKey     = "signal.dag.set"
	pumpSignalDagChangesKey = "signal.dag.changes"
	// pumpSignalDagOffsetsKey holds the refusal reading's rollout offsets and reported call ids
	// (dagHostOffsets as JSON), and pumpSignalDagRefusalsKey numbers the refusal events.
	pumpSignalDagOffsetsKey   = "signal.dag.refusal_offsets"
	pumpSignalDagRefusalsKey  = "signal.dag.refusals"
	pumpSignalCapacityReadKey = "signal.capacity.read_at"
	pumpSignalAuditEpochKey   = "signal.audit.epoch"
	pumpSignalAuditFileKey    = "signal.audit.file"
	// pumpSignalCapacityEpochPrefix is followed by a plan id: the alert epoch the pump last put in
	// that plan's event id, see pumpSignalCapacityRecord.
	pumpSignalCapacityEpochPrefix = "signal.capacity.epoch."
	// pumpSignalCapacityLifecyclePrefix is followed by a plan id: the lifecycle the pump proposed for
	// the plan's entry while the capacity state holds none, kept until a commit or a later read shows
	// the state holds the plan (see pumpSignalCapacityProposals).
	pumpSignalCapacityLifecyclePrefix = "signal.capacity.lifecycle."
	// pumpSignalCapacityUnusablePrefix starts the lifecycle the pump gives an alert judged on a
	// capacity state it cannot use (pumpSignalCapacityUnusable). A lifecycle a capacity state holds is
	// a random base32 name (rand.Text), which never holds a colon, so the two are never confused.
	pumpSignalCapacityUnusablePrefix = "unusable-state:"

	// pumpSignalAuditOffsetKey is the alert file's byte offset in Offsets.
	pumpSignalAuditOffsetKey = "signal_audit"

	// An alert event names at most this many defects, each "what" at most this many characters.
	pumpSignalAuditDefects   = 3
	pumpSignalAuditWhatLimit = 200
)

// pumpSignalDagReview is the seam the DAG source reads through, so no test opens a relay store.
var pumpSignalDagReview = pumpSignalDagReviewRead

// pumpSignalDagReviewRead is the real read. The relay doctor that names the store's state directory
// is asked first, under pumpSignalDagDoctorBudget; the answer is remembered for the Env
// (relayHelperState), so the review then runs with it and asks no doctor of its own. A doctor that
// does not answer in time is an error, and the source reports the review unmeasured.
func pumpSignalDagReviewRead(ctx context.Context, e *Env, cfg *Config) (Review, error) {
	doctorCtx, cancel := context.WithTimeout(ctx, pumpSignalDagDoctorBudget)
	_, err := e.relayHelperState(doctorCtx, cfg)
	cancel()
	if err != nil {
		return Review{}, fmt.Errorf("the relay state directory (doctor budget %v): %w", pumpSignalDagDoctorBudget, err)
	}
	return DagReview(ctx, e, cfg)
}

// pumpSignalCapacity is the seam the capacity source reads through. It returns the report and the
// commit of the alerted state; a dry read returns no commit and writes nothing.
var pumpSignalCapacity = pumpSignalCapacityRead

// pumpSignalCapacityRead is the real read: the pure evaluation, whose Commit the pump calls once the
// alert events are durable (a dry read returns no commit and writes nothing).
//
// The pump judges a plan whose entry carries no lifecycle on a capacity state with a lineage. A
// proposal made on no file, or on a file told only by its content, cannot be told from the same
// file lost and written again (capacityLifecycleProposal.holds); one made on a lineage can. So when
// plans are configured, the read first writes a state without a lineage again as it reads
// (capacityStateEstablishLineage). An evaluation that still read no lineage (the state was replaced
// while it was read) is a failed read.
//
// A state the pump cannot use is not a failed read: a write that fails (another writer holds the
// lock, the lock cannot be opened) over a state without a lineage, or a state nobody can read (a
// directory at its path, a file without read permission). Its plans are judged on the state as far
// as it could be read (an unreadable one reads as empty) and the report carries why
// (pumpSignalCapacityUnusable); no commit is returned, since none could be made over that state. The
// source then gives each alert the pump's own identity for an unusable state, so the alert is
// delivered once however often the state is read again (pumpSignalCapacityUnusableEpoch), and once
// the state can be used the alert continues under its lineage (pumpSignalCapacityEpoch).
//
// The pump settles its alert records and drops its lifecycle proposals on what the evaluation read,
// so that state is made durable first (capacityStateDurable): a commit renamed into place whose
// directory sync failed is not taken for made while a power loss can still take it back. A sync that
// fails is a failed read (the pump state, kept in the same directory, is synced on every save, so a
// directory that cannot be synced stops the pump's own saves first).
func pumpSignalCapacityRead(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
	statePath := crwconfig.JoinRoot(cfg.StateDir, capacityStateFile)
	settings := capacitySettings{}
	unusable := ""
	if !dry && cfg.Section("capacity", &settings) == nil && len(settings.Plans) > 0 {
		if err := capacityStateEstablishLineage(statePath); err != nil {
			unusable = err.Error()
		}
	}
	ev, err := capacityEvaluate(ctx, e, cfg)
	if err != nil {
		return CapacityReport{}, nil, err
	}
	if ev.base == capacityStampUnreadable {
		reason := "it changed while it was read"
		if _, err := os.ReadFile(ev.statePath); err != nil {
			reason = err.Error()
		}
		unusable = "the capacity state cannot be read: " + reason
	}
	if unusable != "" {
		return pumpSignalCapacityUnusable(ev.Report, unusable), nil, nil
	}
	if dry {
		return ev.Report, nil, nil
	}
	for _, plan := range ev.Report.Plans {
		if plan.proposal != nil && plan.proposal.Lineage == "" {
			return CapacityReport{}, nil, errors.New("the capacity state was replaced by one without a lineage while it was read")
		}
	}
	if err := capacityStateDurable(ev.statePath); err != nil {
		return CapacityReport{}, nil, err
	}
	return ev.Report, ev.Commit, nil
}

// pumpSignalCapacityUnusable marks a report judged on a capacity state the pump cannot use: the
// report carries why, and each plan the lifecycle the pump gives an alert on such a state, derived
// from the plan, its whole waiting digest and a fixed marker, instead of the random one the
// evaluation drew (no state keeps it) or a proposal (nothing stores it).
func pumpSignalCapacityUnusable(report CapacityReport, reason string) CapacityReport {
	report.unusable = reason
	plans := make([]CapacityPlan, len(report.Plans))
	for i, plan := range report.Plans {
		plan.lifecycle = pumpSignalCapacityUnusableLifecycle(plan.Plan, pumpSignalWaitingHash(plan.Waiting))
		plan.proposal = nil
		plans[i] = plan
	}
	report.Plans = plans
	return report
}

// pumpSignalCapacityUnusableLifecycle is the lifecycle of plan's alert of the waiting set whose
// digest is waiting, judged on a capacity state the pump cannot use.
func pumpSignalCapacityUnusableLifecycle(plan, waiting string) string {
	data, _ := json.Marshal([]string{plan, waiting}) // two strings: it cannot fail
	sum := sha256.Sum256(data)
	return pumpSignalCapacityUnusablePrefix + hex.EncodeToString(sum[:])
}

// pumpSignalCapacityBudget is the longest one capacity read may hold the pump round. The read
// runs before the round saves its pending events, so a read that does not return would hold the
// urgent events, the audit queue and the parent queue behind it; past the budget it is unmeasured,
// leaves everything as it was and is made again on the next round.
var pumpSignalCapacityBudget = 90 * time.Second

// pumpSignalDagDoctorBudget is the longest the DAG review's relay doctor call (the one that names
// the store's state directory) may hold the pump round. The doctor is a child process the deadline
// kills, and relayHelperRun's WaitDelay bounds a child it leaves holding the output; past the budget
// the review is unmeasured like any failed read and is made again on the next round.
var pumpSignalDagDoctorBudget = 30 * time.Second

// pumpSignalDue reports whether a source whose last read is kept under key is due after every
// seconds. No recorded read, an unreadable one, or one in the future is due.
func pumpSignalDue(st *pumpState, key string, now time.Time, every int) bool {
	at, err := strconv.ParseInt(st.Cursors[key], 10, 64)
	if err != nil {
		return true
	}
	last := time.Unix(at, 0)
	return now.Before(last) || now.Sub(last) >= time.Duration(every)*time.Second
}

// pumpSignalIdle is the result of a round in which a source is not due: it read nothing, so its
// status stays what the last read reported.
func pumpSignalIdle(st *pumpState, name string) pumpSourceResult {
	if status, ok := st.Sources[name]; ok {
		return pumpSourceResult{Status: status}
	}
	return pumpSourceResult{Status: pumpSourceOK}
}

// pumpSignalUnmeasured logs why a source could not read and reports it unmeasured. Nothing about
// the source's state moves, so the read is made again on the next round.
func pumpSignalUnmeasured(cfg *Config, name string, err error) pumpSourceResult {
	pumpLog(cfg, "source "+name+": "+err.Error())
	return pumpSourceResult{Status: pumpSourceUnmeasured}
}

// pumpSignalDagSource reports the DAG review's anomalies. The key of an event is the set of
// anomalies, not their wording, and a review that could not measure everything never clears.
type pumpSignalDagSource struct{}

func (pumpSignalDagSource) Name() string { return pumpSignalDagName }

// pumpSignalTuple is one anomaly as the set keeps it: what, where, which node and which issue.
type pumpSignalTuple struct {
	Kind  string `json:"kind"`
	Plan  string `json:"plan"`
	Node  string `json:"node"`
	Issue string `json:"issue"`
	// Ref is what tells two anomalies apart when the four above do not (the call id of one of a
	// parent's duplicated tool results).
	Ref string `json:"ref,omitempty"`
}

func (t pumpSignalTuple) less(o pumpSignalTuple) bool {
	if t.Kind != o.Kind {
		return t.Kind < o.Kind
	}
	if t.Plan != o.Plan {
		return t.Plan < o.Plan
	}
	if t.Node != o.Node {
		return t.Node < o.Node
	}
	if t.Issue != o.Issue {
		return t.Issue < o.Issue
	}
	return t.Ref < o.Ref
}

func (t pumpSignalTuple) String() string {
	return strings.TrimSpace(strings.Join([]string{t.Kind, t.Plan, t.Node, t.Issue, t.Ref}, " "))
}

// pumpSignalTupleSet sorts and de-duplicates tuples.
func pumpSignalTupleSet(tuples []pumpSignalTuple) []pumpSignalTuple {
	out := append([]pumpSignalTuple{}, tuples...)
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	unique := out[:0]
	for i, t := range out {
		if i == 0 || t != out[i-1] {
			unique = append(unique, t)
		}
	}
	return unique
}

// pumpSignalDagLast is the last successful set. A value that cannot be decoded reads as no set.
func pumpSignalDagLast(st *pumpState) []pumpSignalTuple {
	var tuples []pumpSignalTuple
	if raw := st.Cursors[pumpSignalDagSetKey]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &tuples)
	}
	return pumpSignalTupleSet(tuples)
}

// pumpSignalDagOffsetStore keeps the DAG review's refusal offsets in the pump's own Cursors. They
// reach disk in the same write as the pending events the review produced, so a pump that ends
// before that write reads the same refusals again, and one that completes it never reads them
// twice.
//
// A rollout the offsets do not hold yet is first seen, and its history (the refusals before a
// boundary) is not reported. That boundary is kept apart from the offsets, in pumpSignalDagPinFile
// under dir, and written durably before the first reading classifies a refusal against it: were it
// the size at each reading that finds no offsets, a pump that ended or failed to save before its
// offsets reached disk would take the next reading's larger size as the boundary, and a refusal
// that arrived in between (one the first reading even collected) would read as history. A boundary
// is kept with the file it was taken from (device and inode of the descriptor the reading scans),
// written once, and dropped once the saved pump state holds the rollout's offsets. A rollout only
// grows, so a boundary of the same file is never past what any first-sight reading's own size would
// be: writing it before the pending events can only make a reading report more, never less. A
// boundary is never applied to another file: when the path holds another file, or the same file cut
// below the boundary, it was replaced after the pump first saw it, nothing in it can be told to be
// older than that, and the boundary becomes 0 so every refusal it holds is reported. A dry run writes
// none.
type pumpSignalDagOffsetStore struct {
	st  *pumpState
	dir string
	dry bool
}

type pumpSignalDagOffsets struct {
	Offsets  map[string]int64    `json:"offsets"`
	Files    map[string]string   `json:"files,omitempty"`
	Reported map[string][]string `json:"reported"`
	Unparsed map[string][]string `json:"reported_unparsed"`
}

func (o pumpSignalDagOffsetStore) load() (dagHostOffsets, error) {
	state := dagHostOffsets{Offsets: map[string]int64{}, Files: map[string]string{}, Reported: map[string][]string{}, Unparsed: map[string][]string{}}
	var held pumpSignalDagOffsets
	// A value that cannot be decoded reads as no offsets: each rollout is then first seen, so its
	// history is not replayed.
	if raw := o.st.Cursors[pumpSignalDagOffsetsKey]; raw == "" || json.Unmarshal([]byte(raw), &held) != nil {
		return state, nil
	}
	if held.Offsets != nil {
		state.Offsets = held.Offsets
	}
	if held.Files != nil {
		state.Files = held.Files
	}
	if held.Reported != nil {
		state.Reported = held.Reported
	}
	if held.Unparsed != nil {
		state.Unparsed = held.Unparsed
	}
	return state, nil
}

func (o pumpSignalDagOffsetStore) save(state dagHostOffsets) error {
	encoded, err := json.Marshal(pumpSignalDagOffsets{Offsets: state.Offsets, Files: state.Files, Reported: state.Reported, Unparsed: state.Unparsed})
	if err != nil {
		return err
	}
	o.st.Cursors[pumpSignalDagOffsetsKey] = string(encoded)
	return nil
}

// pin is the rollout's kept first-sight boundary, or one kept now (see the type). file and size are
// the identity and the size of the descriptor the reading scans. A boundary kept for the same file
// and not past its size is the boundary. With none kept, the boundary is size. A boundary kept for
// another file, or past the end of this one, belongs to a file that was replaced or cut short; it
// would take the new content's refusals up to the old size for history, so the boundary becomes 0.
// A boundary kept before identities were (no file) is taken for this file while it is not past its
// size, and is kept again with the identity.
func (o pumpSignalDagOffsetStore) pin(path, file string, size int64) (int64, error) {
	pins, err := pumpSignalDagLoadPins(o.dir)
	if err != nil {
		return 0, err
	}
	boundary := size
	if kept, ok := pins[path]; ok {
		same := dagHostSameFile(kept.File, file)
		if same && kept.At <= size && (kept.File != "" || file == "") {
			return kept.At, nil
		}
		boundary = 0
		if same && kept.At <= size {
			boundary = kept.At
		}
	}
	if o.dry {
		return boundary, nil
	}
	pins[path] = pumpSignalDagPin{At: boundary, File: file}
	if err := pumpSignalDagSavePins(o.dir, pins); err != nil {
		return 0, err
	}
	return boundary, nil
}

// pumpSignalDagPin is one kept boundary: the size the rollout's history ends at and the file
// (device and inode) it was taken from. A boundary file written before identities were holds a bare
// number, which reads as a boundary without a file.
type pumpSignalDagPin struct {
	At   int64  `json:"at"`
	File string `json:"file,omitempty"`
}

func (p *pumpSignalDagPin) UnmarshalJSON(data []byte) error {
	var at int64
	if err := json.Unmarshal(data, &at); err == nil {
		*p = pumpSignalDagPin{At: at}
		return nil
	}
	type plain pumpSignalDagPin
	var held plain
	if err := json.Unmarshal(data, &held); err != nil {
		return err
	}
	*p = pumpSignalDagPin(held)
	return nil
}

// pumpSignalDagPinPath is the boundary file of a state directory.
func pumpSignalDagPinPath(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("no state directory to keep the first-sight boundary in")
	}
	return crwconfig.JoinRoot(dir, pumpSignalDagPinFile), nil
}

// pumpSignalDagLoadPins reads the boundaries. An absent file is none; one that cannot be read or
// decoded is an error rather than none, so a damaged file never moves a boundary later and lets a
// refusal pass as history: the refusal reading of a first-seen rollout waits until it is repaired.
func pumpSignalDagLoadPins(dir string) (map[string]pumpSignalDagPin, error) {
	path, err := pumpSignalDagPinPath(dir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]pumpSignalDagPin{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Pins map[string]pumpSignalDagPin `json:"pins"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("the boundary file %s is not readable: %w", pumpSignalDagPinFile, err)
	}
	if doc.Pins == nil {
		doc.Pins = map[string]pumpSignalDagPin{}
	}
	return doc.Pins, nil
}

// pumpSignalDagSavePins writes the boundaries atomically and durably (temporary file, fsync,
// rename, directory sync), never through a symlink.
func pumpSignalDagSavePins(dir string, pins map[string]pumpSignalDagPin) error {
	path, err := pumpSignalDagPinPath(dir)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("the boundary file is a symlink; refusing to write through it")
	}
	data, err := json.Marshal(struct {
		Pins map[string]pumpSignalDagPin `json:"pins"`
	}{pins})
	if err != nil {
		return err
	}
	return deliverWriteAtomic(path, append(data, 0x0a))
}

// pumpSignalDagDropPins drops the boundaries of the rollouts whose offsets the saved pump state
// holds (raw is the offsets as loaded, before this round's review). A failure only leaves the
// boundaries for a later round: a rollout with offsets never reads its boundary.
func pumpSignalDagDropPins(cfg *Config, raw string) {
	var held pumpSignalDagOffsets
	if raw == "" || json.Unmarshal([]byte(raw), &held) != nil || len(held.Offsets) == 0 {
		return
	}
	path, err := pumpSignalDagPinPath(cfg.StateDir)
	if err != nil {
		return
	}
	if _, err := os.Lstat(path); err != nil {
		return
	}
	pins, err := pumpSignalDagLoadPins(cfg.StateDir)
	if err != nil {
		pumpLog(cfg, "source "+pumpSignalDagName+": "+err.Error())
		return
	}
	dropped := false
	for rollout := range pins {
		if _, ok := held.Offsets[rollout]; ok {
			delete(pins, rollout)
			dropped = true
		}
	}
	if !dropped {
		return
	}
	if err := pumpSignalDagSavePins(cfg.StateDir, pins); err != nil {
		pumpLog(cfg, "source "+pumpSignalDagName+": drop the first-sight boundaries: "+err.Error())
	}
}

// pumpSignalDagRefusalsEvent is the one event of a round's new relay refusals. A refusal is an
// occurrence, not a standing condition: each is reported once, by the offsets above, and is not
// part of the anomaly set that a later round compares.
func pumpSignalDagRefusalsEvent(st *pumpState, refusals []DagReviewAnomaly) pumpEvent {
	count, _ := strconv.Atoi(st.Cursors[pumpSignalDagRefusalsKey])
	count++
	st.Cursors[pumpSignalDagRefusalsKey] = strconv.Itoa(count)
	lines := []string{fmt.Sprintf("[dag-review] new relay refusals: %d", len(refusals))}
	for _, refusal := range refusals {
		lines = append(lines, " - "+strings.TrimSpace(refusal.Kind+" "+refusal.Issue)+": "+refusal.Detail)
	}
	return pumpEvent{ID: fmt.Sprintf("%s#%d", pumpSignalRefusalsName, count), Kind: pumpKindDag, Text: pumpTruncate(strings.Join(lines, "\n"), pumpReportLimit)}
}

func (pumpSignalDagSource) Collect(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult {
	now := e.Now()
	if !pumpSignalDue(st, pumpSignalDagReadKey, now, pumpSettingInt(s.DagReviewSeconds, pumpDefaultDagReviewSeconds)) {
		return pumpSignalIdle(st, pumpSignalDagName)
	}
	// The review keeps its refusal offsets in the pump state, not in its own file: DagReview's file is
	// saved before it returns, so a pump that ended before saving its pending events would lose the
	// refusals. A review that fails after it moved the offsets gives them back.
	offsetsBefore, hadOffsets := st.Cursors[pumpSignalDagOffsetsKey]
	if !st.dry {
		// The offsets as loaded are the saved ones: no source but this one writes them.
		pumpSignalDagDropPins(cfg, offsetsBefore)
	}
	store := pumpSignalDagOffsetStore{st: st, dir: cfg.StateDir, dry: st.dry}
	review, err := pumpSignalDagReview(dagHostWithOffsetStore(ctx, store), e, cfg)
	if err != nil {
		if hadOffsets {
			st.Cursors[pumpSignalDagOffsetsKey] = offsetsBefore
		} else {
			delete(st.Cursors, pumpSignalDagOffsetsKey)
		}
		return pumpSignalUnmeasured(cfg, pumpSignalDagName, err)
	}
	details := map[pumpSignalTuple]string{}
	var current []pumpSignalTuple
	var refusals []DagReviewAnomaly
	for _, anomaly := range review.Anomalies {
		if anomaly.Kind == dagHostKindParentDagRefusals {
			refusals = append(refusals, anomaly)
			continue
		}
		tuple := pumpSignalTuple{Kind: anomaly.Kind, Plan: anomaly.Plan, Node: anomaly.Node, Issue: anomaly.Issue, Ref: anomaly.ref}
		if _, seen := details[tuple]; !seen {
			details[tuple] = anomaly.Detail
		}
		current = append(current, tuple)
	}
	current = pumpSignalTupleSet(current)
	measured := true
	for _, check := range review.Checks {
		// The check that is unmeasured for good (CRW-1272) is no round's gap: counting it would keep every
		// set from being cleared once the store holds a plan.
		if check.State == dagReviewUnmeasured && !dagReviewCheckIsStandingGap(check.Name) {
			measured = false
		}
	}
	last := pumpSignalDagLast(st)
	effective := current
	if !measured {
		// What a review could not measure it cannot clear: the anomalies it still sees are added to
		// the last set, and nothing is dropped.
		effective = pumpSignalTupleSet(append(append([]pumpSignalTuple{}, last...), current...))
	}
	st.Cursors[pumpSignalDagReadKey] = strconv.FormatInt(now.Unix(), 10)
	var events []pumpEvent
	if len(refusals) > 0 {
		events = append(events, pumpSignalDagRefusalsEvent(st, refusals))
	}
	if pumpSignalTuplesEqual(effective, last) {
		return pumpSourceResult{Status: pumpSourceOK, Events: events}
	}
	changes, _ := strconv.Atoi(st.Cursors[pumpSignalDagChangesKey])
	changes++
	encoded, err := json.Marshal(effective)
	if err != nil {
		return pumpSignalUnmeasured(cfg, pumpSignalDagName, err)
	}
	st.Cursors[pumpSignalDagChangesKey] = strconv.Itoa(changes)
	st.Cursors[pumpSignalDagSetKey] = string(encoded)
	ev := pumpEvent{ID: fmt.Sprintf("%s#%d", pumpSignalDagName, changes)}
	if len(effective) == 0 {
		ev.Kind = pumpKindInfo
		lines := []string{fmt.Sprintf("[dag-review] the %d anomalies reported before are gone", len(last))}
		for _, tuple := range last {
			lines = append(lines, " - "+tuple.String())
		}
		ev.Text = pumpTruncate(strings.Join(lines, "\n"), pumpReportLimit)
	} else {
		ev.Kind = pumpKindDag
		lines := []string{fmt.Sprintf("[dag-review] anomalies: %d", len(effective))}
		for _, tuple := range effective {
			detail, ok := details[tuple]
			if !ok {
				detail = "not measured this round, kept from the last read"
			}
			lines = append(lines, " - "+tuple.String()+": "+detail)
		}
		ev.Text = pumpTruncate(strings.Join(lines, "\n"), pumpReportLimit)
	}
	return pumpSourceResult{Status: pumpSourceOK, Events: append(events, ev)}
}

func pumpSignalTuplesEqual(a, b []pumpSignalTuple) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pumpSignalCapacitySource sends one low event for every plan the capacity judgement alerts on.
type pumpSignalCapacitySource struct{}

func (pumpSignalCapacitySource) Name() string { return pumpSignalCapacityName }

// pumpSignalCapacityRecord is what the pump keeps, under its own Cursors and in the same write as
// the pending events, about the last alert event it made for a plan: the epoch the capacity state
// had when the alert was judged (Base), the lifecycle of the plan's entry in the capacity state
// (Lifecycle, see capacityPlanState), the epoch put in the event id (Epoch), the waiting set
// (Waiting), whether the alert is settled (Committed): its alerted state was committed, a later
// read found it reflected in the state, or the record was superseded because the plan left the
// settings, and when the pump first judged the alert of this epoch (At, Unix seconds; cleared when
// the plan leaves the settings, which ends its alert generation).
//
// The capacity state file alone cannot give an event id that is stable and also never reused: a
// plan removed from the settings and added again, or a state file that is gone, restarts its epoch
// at 1 and would rebuild the id of an alert already sent, which the sent set then swallows. The
// record is what keeps the id apart. Only a retry of the same alert reuses its epoch: an unsettled
// record, the same lifecycle of the plan's entry, the same base and the same waiting set. That is
// how a lost or refused commit converges on the same id, also when another writer (the command
// line) saved the state in between without alerting this plan: the commit's conflict check is
// against every write of the file (capacityCommitState), the alert's identity only against the
// plan's own lifecycle and alert epoch. A lifecycle is a random name drawn when the plan's entry is
// made, never derived from a stamp of the file; for an entry the state does not hold yet the pump
// keeps the name it proposed (pumpSignalCapacityProposals) so the retry stores the same one while
// the proposal holds. A plan registered again or a state file lost or written again without the
// plan therefore starts a new lifecycle. Any other alert takes an epoch above the record's, so the
// epoch of a plan's events only grows.
//
// An alert judged on a capacity state the pump cannot use carries the pump's own lifecycle for such
// a state (pumpSignalCapacityUnusableLifecycle) and is matched by its waiting set
// (pumpSignalCapacityUnusableEpoch); the first judgement on a usable state takes its record over
// (pumpSignalCapacityEpoch).
type pumpSignalCapacityRecord struct {
	Base      int    `json:"base"`
	Lifecycle string `json:"lifecycle"`
	Epoch     int    `json:"epoch"`
	Waiting   string `json:"waiting"`
	Committed bool   `json:"committed"`
	At        int64  `json:"at,omitempty"`
}

// unusable reports whether the record is of an alert judged on a capacity state the pump could not
// use.
func (rec pumpSignalCapacityRecord) unusable() bool {
	return strings.HasPrefix(rec.Lifecycle, pumpSignalCapacityUnusablePrefix)
}

// recent reports whether the record's alert was first judged less than realert before now: inside
// the window in which the capacity judgement does not alert the same waiting set again. A record
// without a time (the plan left the settings since, or a pump that did not keep one wrote it) is
// not recent.
func (rec pumpSignalCapacityRecord) recent(now time.Time, realert time.Duration) bool {
	return rec.At > 0 && now.Sub(time.Unix(rec.At, 0)) < realert
}

func pumpSignalCapacityLoad(st *pumpState, plan string) (pumpSignalCapacityRecord, bool) {
	var rec pumpSignalCapacityRecord
	raw := st.Cursors[pumpSignalCapacityEpochPrefix+plan]
	if raw == "" || json.Unmarshal([]byte(raw), &rec) != nil {
		return pumpSignalCapacityRecord{}, false
	}
	return rec, true
}

func pumpSignalCapacityStore(st *pumpState, plan string, rec pumpSignalCapacityRecord) {
	if encoded, err := json.Marshal(rec); err == nil {
		st.Cursors[pumpSignalCapacityEpochPrefix+plan] = string(encoded)
	}
}

// pumpSignalWaitingHash is the digest of a waiting set, whole: it decides whether a record and an
// id are the same alert, so two different sets must never share it (a prefix of it can).
func pumpSignalWaitingHash(waiting []string) string {
	data, _ := json.Marshal(waiting)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pumpSignalCapacityEpoch is the epoch the plan's alert event takes, judged on a capacity state the
// pump can use, and the record to keep.
//
// A retry of the record's alert (unsettled, the same lifecycle, base and waiting set) keeps its
// epoch. So does the first judgement on a usable state of an alert the pump judged while the state
// could not be used: an unsettled record of such an alert with the same waiting set, first judged
// inside the realert window, on a state that recorded no removal of the plan's entry since it last
// made it, is taken over: the alert keeps its id (pending or delivered already) and the record takes
// the state's lifecycle and base, so the commit and every retry continue under the state's lineage.
// A plan whose entry was removed (the command line made and removed it, or removed it before) is a
// new generation, as on a usable state, and alerts under a new epoch; so does an unusable state's
// alert past the realert window, which is the periodic re-alert falling due. Any other alert takes
// an epoch above the record's.
func pumpSignalCapacityEpoch(st *pumpState, plan CapacityPlan, now time.Time, realert time.Duration) (int, pumpSignalCapacityRecord) {
	base, hash := plan.AlertEpoch-1, pumpSignalWaitingHash(plan.Waiting)
	epoch, at := plan.AlertEpoch, now.Unix()
	if rec, ok := pumpSignalCapacityLoad(st, plan.Plan); ok {
		switch {
		case !rec.Committed && rec.Lifecycle == plan.lifecycle && rec.Base == base && rec.Waiting == hash:
			epoch, at = rec.Epoch, rec.At
		case !rec.Committed && rec.unusable() && rec.Waiting == hash && rec.recent(now, realert) && plan.removed == 0:
			epoch, at = rec.Epoch, rec.At
		case rec.Epoch >= epoch:
			epoch = rec.Epoch + 1
		}
	}
	if at == 0 {
		// A retry of a record written before records kept the time is taken as judged now.
		at = now.Unix()
	}
	return epoch, pumpSignalCapacityRecord{Base: base, Lifecycle: plan.lifecycle, Epoch: epoch, Waiting: hash, At: at}
}

// pumpSignalCapacityUnusableEpoch is the epoch the plan's alert event takes, judged on a capacity
// state the pump cannot use, and the record to keep.
//
// Such a state keeps no alerted set, so the judgement alerts the plan's waiting set on every read;
// the pump's record is what makes it one alert. A record of the same waiting set that is unsettled
// (this alert judged on an earlier read, or the pump's alert whose commit has not landed) or was
// first judged inside the realert window (an alert committed before the state became unusable)
// gives its epoch, and the record stays as it is: the event carries the id already pending or
// delivered, and the pump's dedupe (pending, then the sent set) keeps it from being delivered again.
// Any other alert (no record, another waiting set, or one whose record the plan's leaving the
// settings ended) takes an epoch above the record's under the unusable state's lifecycle, first
// judged now; while the state stays unusable it is not alerted again, whatever realert_hours says.
func pumpSignalCapacityUnusableEpoch(st *pumpState, plan CapacityPlan, now time.Time, realert time.Duration) (int, pumpSignalCapacityRecord) {
	hash, epoch := pumpSignalWaitingHash(plan.Waiting), plan.AlertEpoch
	if rec, ok := pumpSignalCapacityLoad(st, plan.Plan); ok {
		if rec.Waiting == hash && (!rec.Committed || rec.recent(now, realert)) {
			return rec.Epoch, rec
		}
		if rec.Epoch >= epoch {
			epoch = rec.Epoch + 1
		}
	}
	return epoch, pumpSignalCapacityRecord{Base: plan.AlertEpoch - 1, Lifecycle: plan.lifecycle, Epoch: epoch, Waiting: hash, At: now.Unix()}
}

// pumpSignalCapacityProposals is the lifecycles the pump proposed for plans the capacity state did
// not hold, as the evaluation takes them (capacityWithLifecycles).
//
// A plan the state holds no lifecycle for (it does not hold the plan, or holds an entry written
// before entries carried one) gets a new random lifecycle from its evaluation. The pump writes that
// name, with the generation of the state it was given on, to its own state, in the same save as the
// pending events and so before the commit that would store it, and proposes it to every later
// evaluation, which takes it only while it holds (capacityLifecycleProposal.holds): on the same
// generation, or on a later write of it that neither lost the file nor made and removed the plan's
// entry. A commit refused (another writer saved in between without touching the plan's entry) or
// never made (a crash, a failed save) is therefore retried under the same lifecycle, and the alert
// it carries converges on its pending id; once the plan's entry was made and removed, or the state
// file lost or written again from nothing, the proposal no longer holds, the evaluation draws a fresh
// lifecycle and the alert takes a new id instead of being absorbed into one already sent. The
// proposal is dropped once the state holds a lifecycle for the plan (the commit succeeded, or a
// later read finds one) and when the plan leaves the settings; each measured read writes it again
// with the generation it was taken on.
func pumpSignalCapacityProposals(st *pumpState) map[string]capacityLifecycleProposal {
	proposed := map[string]capacityLifecycleProposal{}
	for key, value := range st.Cursors {
		name, ok := strings.CutPrefix(key, pumpSignalCapacityLifecyclePrefix)
		var p capacityLifecycleProposal
		if ok && json.Unmarshal([]byte(value), &p) == nil && p.Lifecycle != "" {
			proposed[name] = p
		}
	}
	return proposed
}

// pumpSignalCapacitySettle settles the record of every judged plan whose state already reflects a
// later alert of the same lifecycle: the commit was made (by this pump, whose record of it was lost,
// or by the command line). The record is then not reused, whatever happens to the state file later.
// The record of an alert judged while the state could not be used is settled once its realert window
// has passed, so a later state the pump cannot use alerts the set again. A plan whose entry carries a
// lifecycle in the state needs no proposal any more, and one whose entry carries none keeps the
// lifecycle its evaluation gave it as the proposal for its retry. It runs only on a report judged on
// a state the pump could use.
func pumpSignalCapacitySettle(st *pumpState, report CapacityReport, now time.Time, realert time.Duration) {
	for _, plan := range report.Plans {
		if rec, ok := pumpSignalCapacityLoad(st, plan.Plan); ok && !rec.Committed && rec.unusable() && !rec.recent(now, realert) {
			rec.Committed = true
			pumpSignalCapacityStore(st, plan.Plan, rec)
		}
		if plan.proposal != nil {
			encoded, _ := json.Marshal(plan.proposal) // two plain fields: it cannot fail
			st.Cursors[pumpSignalCapacityLifecyclePrefix+plan.Plan] = string(encoded)
		} else {
			delete(st.Cursors, pumpSignalCapacityLifecyclePrefix+plan.Plan)
		}
		if rec, ok := pumpSignalCapacityLoad(st, plan.Plan); ok && !rec.Committed &&
			rec.Lifecycle == plan.lifecycle && plan.committedEpoch > rec.Base {
			rec.Committed = true
			pumpSignalCapacityStore(st, plan.Plan, rec)
		}
	}
}

// pumpSignalCapacitySupersede settles the record of every plan the report no longer judges and
// clears its time: a plan that left the settings ends its alert generation, so when it is registered
// again its next alert takes a new epoch rather than the uncommitted one of its last alert, also on
// a state the pump cannot use, and its entry a new lifecycle rather than the one proposed before.
func pumpSignalCapacitySupersede(st *pumpState, report CapacityReport) {
	judged := map[string]bool{}
	for _, plan := range report.Plans {
		judged[plan.Plan] = true
	}
	for key := range st.Cursors {
		if name, ok := strings.CutPrefix(key, pumpSignalCapacityLifecyclePrefix); ok && !judged[name] {
			delete(st.Cursors, key)
			continue
		}
		name, ok := strings.CutPrefix(key, pumpSignalCapacityEpochPrefix)
		if !ok || judged[name] {
			continue
		}
		if rec, ok := pumpSignalCapacityLoad(st, name); ok && (!rec.Committed || rec.At != 0) {
			rec.Committed, rec.At = true, 0
			pumpSignalCapacityStore(st, name, rec)
		}
	}
}

// pumpSignalCapacityID is the id of one alert: the plan, its event epoch and the waiting set. The
// waiting set is in the id so a set that changed before a lost commit was repaired is a new alert,
// not one the sent set swallows.
func pumpSignalCapacityID(plan CapacityPlan, epoch int) string {
	return fmt.Sprintf("%s#%s@%d:%s", pumpSignalCapacityName, plan.Plan, epoch, pumpSignalWaitingHash(plan.Waiting))
}

// pumpSignalCandidateKeys is the issue keys of the plan's detachable branch candidates.
func pumpSignalCandidateKeys(plan CapacityPlan) []string {
	seen := map[string]bool{}
	var keys []string
	if plan.Branches != nil {
		for _, candidate := range *plan.Branches {
			for _, node := range candidate.Nodes {
				if node.IssueKey != "" && !seen[node.IssueKey] {
					seen[node.IssueKey] = true
					keys = append(keys, node.IssueKey)
				}
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func pumpSignalCapacityText(plan CapacityPlan) string {
	candidates := "none"
	if keys := pumpSignalCandidateKeys(plan); len(keys) > 0 {
		candidates = strings.Join(keys, ", ")
	} else if plan.BranchesUnmeasured != "" {
		candidates = "unmeasured (" + plan.BranchesUnmeasured + ")"
	}
	return pumpTruncate(fmt.Sprintf("[capacity] plan %s (parent %s, family %s) has room for another branch: %d waiting [%s] for %.0f minutes; candidate issue_keys: %s",
		plan.Plan, plan.Parent, plan.Family, len(plan.Waiting), strings.Join(plan.Waiting, ", "), plan.WaitingMinutes, candidates), pumpReportLimit)
}

func (pumpSignalCapacitySource) Collect(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult {
	now := e.Now()
	if !pumpSignalDue(st, pumpSignalCapacityReadKey, now, pumpSettingInt(s.CapacitySeconds, pumpDefaultCapacitySeconds)) {
		return pumpSignalIdle(st, pumpSignalCapacityName)
	}
	readCtx, cancel := context.WithTimeout(ctx, pumpSignalCapacityBudget)
	defer cancel()
	report, commit, err := pumpSignalCapacity(capacityWithLifecycles(readCtx, pumpSignalCapacityProposals(st)), e, cfg, st.dry)
	if err == nil && readCtx.Err() != nil {
		// A read that returns after its budget ran out (or after the round was cancelled) is not a
		// judgement: the evaluation absorbs a failed signal read as an unknown signal, so a report
		// built past the deadline may rest on reads the deadline cut off. It is unmeasured like a
		// failed read: no event, no read time, no record and no commit, and it is made again.
		err = fmt.Errorf("the capacity read ran past its budget of %v: %w", pumpSignalCapacityBudget, readCtx.Err())
	}
	if err != nil {
		// A read nobody could take is unmeasured: it leaves the alerted state as it is and is made
		// again on the next round.
		return pumpSignalUnmeasured(cfg, pumpSignalCapacityName, err)
	}
	st.Cursors[pumpSignalCapacityReadKey] = strconv.FormatInt(now.Unix(), 10)
	realert := time.Duration(report.Limits.RealertHours * float64(time.Hour))
	pumpSignalCapacitySupersede(st, report)
	epochOf := pumpSignalCapacityEpoch
	if report.unusable != "" {
		// Judged on a state the pump cannot use: no commit came with it, the proposals and records
		// stay as they are, and each alert is the pump's own until the state can be used again.
		pumpLog(cfg, "source "+pumpSignalCapacityName+": "+report.unusable+"; judged without a usable capacity state, nothing committed")
		epochOf = pumpSignalCapacityUnusableEpoch
	} else {
		pumpSignalCapacitySettle(st, report, now, realert)
	}
	res := pumpSourceResult{Status: pumpSourceOK}
	var alerted, proposed []string
	for _, plan := range report.Plans {
		if plan.proposal != nil {
			proposed = append(proposed, plan.Plan)
		}
		if !plan.Alert {
			continue
		}
		epoch, rec := epochOf(st, plan, now, realert)
		pumpSignalCapacityStore(st, plan.Plan, rec)
		alerted = append(alerted, plan.Plan)
		res.Events = append(res.Events, pumpEvent{ID: pumpSignalCapacityID(plan, epoch), Kind: pumpKindLow, Text: pumpSignalCapacityText(plan)})
	}
	if commit != nil {
		// Once the alerted state is committed the record says so, and the pump saves that again: a
		// state that is later lost or reset then takes a higher epoch instead of repeating this one,
		// and a plan whose proposed lifecycle the state now holds proposes none any more.
		res.Commit = func() error {
			if err := commit(); err != nil {
				return err
			}
			for _, name := range proposed {
				delete(st.Cursors, pumpSignalCapacityLifecyclePrefix+name)
				st.dirty = true
			}
			for _, name := range alerted {
				if rec, ok := pumpSignalCapacityLoad(st, name); ok {
					rec.Committed = true
					pumpSignalCapacityStore(st, name, rec)
					st.dirty = true
				}
			}
			return nil
		}
	}
	return res
}

// pumpSignalAuditSource sends one low event for every complete line of the audit alert queue.
type pumpSignalAuditSource struct{}

func (pumpSignalAuditSource) Name() string { return pumpSignalAuditName }

// pumpSignalAuditLine is one complete line of the queue and the byte offset it starts at.
type pumpSignalAuditLine struct {
	start int64
	text  string
}

// pumpSignalAuditLines is the complete lines of the open queue file after start and the offset
// after the last one. A half-written last line is not returned and the offset stays at its start,
// so it is read whole on a later round.
func pumpSignalAuditLines(file *os.File, start int64) ([]pumpSignalAuditLine, int64, error) {
	if _, err := file.Seek(start, 0); err != nil {
		return nil, start, err
	}
	reader := bufio.NewReader(file)
	offset := start
	var lines []pumpSignalAuditLine
	for {
		line, readErr := reader.ReadString('\n')
		if !strings.HasSuffix(line, "\n") {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return lines, offset, readErr
			}
			break
		}
		lines = append(lines, pumpSignalAuditLine{start: offset, text: strings.TrimSpace(line)})
		offset += int64(len(line))
	}
	return lines, offset, nil
}

func pumpSignalAuditText(row auditAlertRow) string {
	score := "none"
	if row.Score != nil {
		score = strconv.Itoa(*row.Score)
	}
	lines := []string{fmt.Sprintf("[audit] alert mode=%s subject=%s issue=%s pair=%s phase=%s round=%s score=%s",
		row.Mode, row.Subject, row.Issue, row.Pair, row.Phase, row.Round, score)}
	// The first pumpSignalAuditDefects defects, each cut short, as the management predecessor did;
	// the queue file has the rest.
	for i, defect := range row.Defects {
		if i == pumpSignalAuditDefects {
			lines = append(lines, fmt.Sprintf(" (+%d more defects in the alert file)", len(row.Defects)-i))
			break
		}
		lines = append(lines, fmt.Sprintf(" - %s %s (%s)", defect.Severity, pumpTruncate(defect.What, pumpSignalAuditWhatLimit), defect.Where))
	}
	return pumpTruncate(strings.Join(lines, "\n"), pumpReportLimit)
}

func (pumpSignalAuditSource) Collect(_ context.Context, e *Env, cfg *Config, st *pumpState, _ pumpSettings) pumpSourceResult {
	path := crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", auditAlertFile)
	// The size, the identity and the lines all come from the one open file, so a replacement between
	// a stat and an open cannot split them.
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pumpSourceResult{Status: pumpSourceOK}
		}
		return pumpSignalUnmeasured(cfg, pumpSignalAuditName, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pumpSignalUnmeasured(cfg, pumpSignalAuditName, err)
	}
	start := st.Offsets[pumpSignalAuditOffsetKey]
	epoch, _ := strconv.Atoi(st.Cursors[pumpSignalAuditEpochKey])
	identity, known := dagHostFileIdentity(info), st.Cursors[pumpSignalAuditFileKey]
	// A different file, or one shorter than what was read, is a replacement: it is read from its
	// start, and the ids carry a new epoch so they cannot collide with the ones the old file
	// produced. A state that predates the identity adopts the file it finds.
	if (known != "" && identity != "" && identity != known) || info.Size() < start {
		epoch++
		start = 0
		st.Cursors[pumpSignalAuditEpochKey] = strconv.Itoa(epoch)
		st.Offsets[pumpSignalAuditOffsetKey] = 0
	}
	if identity != "" {
		st.Cursors[pumpSignalAuditFileKey] = identity
	}
	if info.Size() == start {
		return pumpSourceResult{Status: pumpSourceOK}
	}
	lines, end, err := pumpSignalAuditLines(file, start)
	if err != nil {
		return pumpSignalUnmeasured(cfg, pumpSignalAuditName, err)
	}
	var events []pumpEvent
	for _, line := range lines {
		var row auditAlertRow
		if line.text == "" || json.Unmarshal([]byte(line.text), &row) != nil {
			continue
		}
		id := fmt.Sprintf("%s#%d", pumpSignalAuditName, line.start)
		if epoch > 0 {
			id = fmt.Sprintf("%s@%d#%d", pumpSignalAuditName, epoch, line.start)
		}
		events = append(events, pumpEvent{ID: id, Kind: pumpKindLow, Text: pumpSignalAuditText(row)})
	}
	st.Offsets[pumpSignalAuditOffsetKey] = end
	return pumpSourceResult{Status: pumpSourceOK, Events: events}
}
