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
	pumpSignalCapacityName = "signal_capacity"
	pumpSignalAuditName    = "signal_audit"

	// The Cursors keys. The read times are Unix seconds; the DAG set is the sorted anomaly tuples as
	// JSON; the change counter numbers each change of that set; the audit epoch counts the times the
	// alert file shrank.
	pumpSignalDagReadKey      = "signal.dag.read_at"
	pumpSignalDagSetKey       = "signal.dag.set"
	pumpSignalDagChangesKey   = "signal.dag.changes"
	pumpSignalCapacityReadKey = "signal.capacity.read_at"
	pumpSignalAuditEpochKey   = "signal.audit.epoch"

	// pumpSignalAuditOffsetKey is the alert file's byte offset in Offsets.
	pumpSignalAuditOffsetKey = "signal_audit"

	// An alert event names at most this many defects, each "what" at most this many characters.
	pumpSignalAuditDefects   = 3
	pumpSignalAuditWhatLimit = 200
)

// pumpSignalDagReview is the seam the DAG source reads through, so no test opens a relay store.
var pumpSignalDagReview = func(ctx context.Context, e *Env, cfg *Config) (Review, error) {
	return DagReview(ctx, e, cfg)
}

// pumpSignalCapacity is the seam the capacity source reads through. It returns the report and the
// commit of the alerted state; a dry read returns no commit and writes nothing.
var pumpSignalCapacity = pumpSignalCapacityRead

// pumpSignalCapacityRead is the real read: a dry read is Capacity(dry=true); otherwise the pure
// evaluation, whose Commit the pump calls once the alert events are durable.
func pumpSignalCapacityRead(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
	if dry {
		report, err := Capacity(ctx, e, cfg, true)
		return report, nil, err
	}
	ev, err := capacityEvaluate(ctx, e, cfg)
	if err != nil {
		return CapacityReport{}, nil, err
	}
	return ev.Report, ev.Commit, nil
}

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
	return t.Issue < o.Issue
}

func (t pumpSignalTuple) String() string {
	return strings.TrimSpace(strings.Join([]string{t.Kind, t.Plan, t.Node, t.Issue}, " "))
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

func (pumpSignalDagSource) Collect(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult {
	now := e.Now()
	if !pumpSignalDue(st, pumpSignalDagReadKey, now, pumpSettingInt(s.DagReviewSeconds, pumpDefaultDagReviewSeconds)) {
		return pumpSignalIdle(st, pumpSignalDagName)
	}
	// The review reads no offset state: DagReview saves the refusal offsets before it returns, so a
	// pump that ended before saving its pending events would lose the refusals. The repeat of a
	// standing anomaly is suppressed by the set below instead.
	review, err := pumpSignalDagReview(dagHostNoState(ctx, true), e, cfg)
	if err != nil {
		return pumpSignalUnmeasured(cfg, pumpSignalDagName, err)
	}
	details := map[pumpSignalTuple]string{}
	var current []pumpSignalTuple
	for _, anomaly := range review.Anomalies {
		tuple := pumpSignalTuple{Kind: anomaly.Kind, Plan: anomaly.Plan, Node: anomaly.Node, Issue: anomaly.Issue}
		if _, seen := details[tuple]; !seen {
			details[tuple] = anomaly.Detail
		}
		current = append(current, tuple)
	}
	current = pumpSignalTupleSet(current)
	measured := true
	for _, check := range review.Checks {
		if check.State == dagReviewUnmeasured {
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
	if pumpSignalTuplesEqual(effective, last) {
		return pumpSourceResult{Status: pumpSourceOK}
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
	return pumpSourceResult{Status: pumpSourceOK, Events: []pumpEvent{ev}}
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

// pumpSignalCapacityID is the id of one alert: the plan, its alert epoch and the waiting set. The
// epoch comes from the last committed state, so a judgement made again after a lost commit is the
// same id, and a realert_hours re-alert is the next epoch. The waiting set is in the id so a set
// that changed before a lost commit was repaired is a new alert, not one the sent set swallows.
func pumpSignalCapacityID(plan CapacityPlan) string {
	data, _ := json.Marshal(plan.Waiting)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s#%s@%d:%s", pumpSignalCapacityName, plan.Plan, plan.AlertEpoch, hex.EncodeToString(sum[:])[:8])
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
	report, commit, err := pumpSignalCapacity(ctx, e, cfg, st.dry)
	if err != nil {
		// A read nobody could take is unmeasured: it leaves the alerted state as it is and is made
		// again on the next round.
		return pumpSignalUnmeasured(cfg, pumpSignalCapacityName, err)
	}
	st.Cursors[pumpSignalCapacityReadKey] = strconv.FormatInt(now.Unix(), 10)
	res := pumpSourceResult{Status: pumpSourceOK, Commit: commit}
	for _, plan := range report.Plans {
		if plan.Alert {
			res.Events = append(res.Events, pumpEvent{ID: pumpSignalCapacityID(plan), Kind: pumpKindLow, Text: pumpSignalCapacityText(plan)})
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

// pumpSignalAuditLines is the complete lines of the queue after start and the offset after the last
// one. A half-written last line is not returned and the offset stays at its start, so it is read
// whole on a later round.
func pumpSignalAuditLines(path string, start int64) ([]pumpSignalAuditLine, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, start, err
	}
	defer file.Close()
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
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pumpSourceResult{Status: pumpSourceOK}
		}
		return pumpSignalUnmeasured(cfg, pumpSignalAuditName, err)
	}
	start := st.Offsets[pumpSignalAuditOffsetKey]
	epoch, _ := strconv.Atoi(st.Cursors[pumpSignalAuditEpochKey])
	if info.Size() < start {
		// The file is shorter than what was read: it was replaced. It is read from its start, and the
		// ids carry a new epoch so they cannot collide with the ones the old file produced.
		epoch++
		start = 0
		st.Cursors[pumpSignalAuditEpochKey] = strconv.Itoa(epoch)
		st.Offsets[pumpSignalAuditOffsetKey] = 0
	}
	if info.Size() == start {
		return pumpSourceResult{Status: pumpSourceOK}
	}
	lines, end, err := pumpSignalAuditLines(path, start)
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
