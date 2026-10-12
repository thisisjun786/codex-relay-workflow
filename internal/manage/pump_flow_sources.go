package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The pump's sources of the upward half of a flow run (CRW-1204). Both read the relay store read-only: the flow rows are written by the
// run and by the `crw flow` commands, never by the pump (a delivery the pump settles is told to `crw flow report-delivered`).
//
//   - flow_runs: a run's escalations (urgent kind run_escalation) and a run that died - it started, did not finish and has written no row of
//     its own for run_dead_seconds (urgent kind run_dead). A resumed run writes rows again, so a second death is another event.
//   - flow_reports: a decision only Jun can make (urgent kind jun_decision) and a report nobody read for low_priority_seconds (low kind
//     report_unread: report id, age, the last delivery result).
//
// The report_undelivered kind is raised by the supervisor queue flush (pump_supervisor.go), not by a source.
const (
	pumpFlowRunsName    = "flow_runs"
	pumpFlowReportsName = "flow_reports"

	pumpFlowEscalationKey = "flow.escalation.rowid"

	pumpFlowRunDeadDefault = 1800
	// pumpFlowOpenCallGrace is how long a run with an open agent call (prepared or thread_started, no terminal row) may write no row
	// before it is dead: a turn writes nothing of the run while it is in flight and can outlast run_dead_seconds. A longer
	// run_dead_seconds still wins.
	pumpFlowOpenCallGrace = 6 * time.Hour
	// pumpFlowRunDeadWindow is how long after its last row a dead run is still reported: a run abandoned long ago is not news.
	pumpFlowRunDeadWindow = 7 * 24 * time.Hour

	// foreignSeqBase is engine.ForeignSeqBase: the sequence numbers from here on are rows a command wrote, which say nothing about
	// whether the run's process is alive.
	pumpFlowForeignSeq = int64(1) << 40
)

// pumpFlowEscalation is an escalation row of a run.
type pumpFlowEscalation struct {
	Row                                int64
	Run, Node, ID, Reason, Class, Text string
}

// pumpFlowRunRow is the liveness of one run.
type pumpFlowRunRow struct {
	Run, Actor string
	Finished   bool
	// OpenCall is set when the run has an agent call that began and has no terminal row: a turn is (or was, if the process died) in flight.
	OpenCall bool
	Last     time.Time
}

// pumpFlowReport is one report with what happened to it.
type pumpFlowReport struct {
	ID, Kind, Run, Milestone, Subject string
	At                                time.Time
	Read                              bool
	LastDelivery                      string
}

// pumpFlowData is what the sources read from the relay store in one snapshot.
type pumpFlowData struct {
	Escalations []pumpFlowEscalation
	Runs        []pumpFlowRunRow
	Reports     []pumpFlowReport
}

// pumpFlowRead is the seam the flow sources read through, so a test needs no relay store; the real read opens the store read-only.
var pumpFlowRead = pumpFlowReadFromRelay

func pumpFlowReadFromRelay(ctx context.Context, e *Env, cfg *Config) (pumpFlowData, error) {
	doctorCtx, cancel := context.WithTimeout(ctx, pumpSignalDagDoctorBudget)
	state, err := e.relayHelperState(doctorCtx, cfg)
	cancel()
	if err != nil {
		return pumpFlowData{}, fmt.Errorf("the relay state directory: %w", err)
	}
	return pumpFlowReadAt(ctx, state)
}

type pumpFlowRunsSource struct{}

func (pumpFlowRunsSource) Name() string { return pumpFlowRunsName }

// Collect reads the escalations written since the last round (the first read only records where the rows end, so no history is sent) and
// the runs that died.
func (pumpFlowRunsSource) Collect(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult {
	data, err := pumpFlowRead(ctx, e, cfg)
	if err != nil {
		return pumpSourceResult{Status: pumpSourceUnmeasured}
	}
	var events []pumpEvent
	cursor, known := int64(0), false
	if raw, ok := st.Cursors[pumpFlowEscalationKey]; ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			cursor, known = n, true
		}
	}
	top := cursor
	for _, esc := range data.Escalations {
		top = max(top, esc.Row)
		if !known || esc.Row <= cursor {
			continue
		}
		events = append(events, pumpEvent{ID: "run_escalation:" + esc.Run + ":" + esc.ID, Kind: pumpKindRunEscalation,
			Text: fmt.Sprintf("[flow] run escalation run=%s node=%s reason=%s class=%s%s", esc.Run, firstNonEmpty(esc.Node, "-"), esc.Reason, firstNonEmpty(esc.Class, "-"), pumpFlowDetail(esc.Text))})
	}
	if !st.dry {
		st.Cursors[pumpFlowEscalationKey] = strconv.FormatInt(top, 10)
	}
	dead := time.Duration(pumpSettingInt(s.RunDeadSeconds, pumpFlowRunDeadDefault)) * time.Second
	now := e.Now()
	for _, run := range data.Runs {
		if run.Finished || run.Last.IsZero() {
			continue
		}
		idle := now.Sub(run.Last)
		limit := dead
		if run.OpenCall {
			limit = max(dead, pumpFlowOpenCallGrace)
		}
		if idle < limit || idle > pumpFlowRunDeadWindow {
			continue
		}
		events = append(events, pumpEvent{ID: fmt.Sprintf("run_dead:%s:%d", run.Run, run.Last.Unix()), Kind: pumpKindRunDead,
			Text: fmt.Sprintf("[flow] run dead run=%s actor=%s: it has written no row for %s and did not finish; `crw flow resume` goes on from its journal", run.Run, firstNonEmpty(run.Actor, "-"), idle.Round(time.Second))})
	}
	return pumpSourceResult{Status: pumpSourceOK, Events: events}
}

func pumpFlowDetail(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	return "\n" + pumpTruncate(text, 600)
}

type pumpFlowReportsSource struct{}

func (pumpFlowReportsSource) Name() string { return pumpFlowReportsName }

// Collect reads the reports: a decision request for Jun is told at once, a report nobody read is told once it is low_priority_seconds old.
func (pumpFlowReportsSource) Collect(ctx context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult {
	data, err := pumpFlowRead(ctx, e, cfg)
	if err != nil {
		return pumpSourceResult{Status: pumpSourceUnmeasured}
	}
	var events []pumpEvent
	now := e.Now()
	unread := time.Duration(pumpSettingInt(s.LowPrioritySeconds, pumpDefaultLowPrioritySeconds)) * time.Second
	for _, r := range data.Reports {
		switch {
		case r.Kind == "jun_decision_request":
			events = append(events, pumpEvent{ID: "jun_decision:" + r.ID, Kind: pumpKindJunDecision,
				Text: fmt.Sprintf("[flow] decision for Jun report=%s run=%s%s: crw manage decisions records the question and the answer", r.ID, r.Run, pumpFlowDetail(r.Subject))})
		case !r.Read && now.Sub(r.At) >= unread:
			events = append(events, pumpEvent{ID: "report_unread:" + r.ID, Kind: pumpKindReportUnread,
				Text: fmt.Sprintf("[flow] report unread report=%s kind=%s run=%s age=%s last delivery=%s", r.ID, r.Kind, r.Run, now.Sub(r.At).Round(time.Second), firstNonEmpty(r.LastDelivery, "none"))})
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return pumpSourceResult{Status: pumpSourceOK, Events: events}
}

// pumpFlowReadAt reads the flow rows from the relay store below state, in one read-only snapshot.
func pumpFlowReadAt(ctx context.Context, state string) (pumpFlowData, error) {
	handle, err := relayReadOpenStore(ctx, state)
	if err != nil {
		return pumpFlowData{}, err
	}
	defer handle.Close()
	var data pumpFlowData
	err = handle.ReadSnapshot(ctx, func(ctx context.Context, st *store.Store) error {
		q := st.Q(ctx)
		rows, err := q.QueryContext(ctx, `SELECT rowid, run_id, node_id, payload_json FROM flow_events WHERE kind = 'escalation' ORDER BY rowid`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var esc pumpFlowEscalation
			var payload string
			if err := rows.Scan(&esc.Row, &esc.Run, &esc.Node, &payload); err != nil {
				_ = rows.Close()
				return err
			}
			var doc map[string]any
			_ = json.Unmarshal([]byte(payload), &doc)
			if esc.Node == "" {
				esc.Node, _ = doc["node"].(string)
			}
			esc.ID, _ = doc["escalation_id"].(string)
			esc.Reason, _ = doc["reason"].(string)
			esc.Class, _ = doc["class"].(string)
			esc.Text, _ = doc["detail"].(string)
			if esc.ID == "" {
				esc.ID = "row" + strconv.FormatInt(esc.Row, 10)
			}
			data.Escalations = append(data.Escalations, esc)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		runs, err := q.QueryContext(ctx, `SELECT r.run_id, r.actor_task_id, r.started_at,
 EXISTS (SELECT 1 FROM flow_events e WHERE e.run_id = r.run_id AND e.kind = 'finished'),
 COALESCE((SELECT MAX(recorded_at) FROM flow_calls c WHERE c.run_id = r.run_id), ''),
 EXISTS (SELECT 1 FROM flow_calls c WHERE c.run_id = r.run_id AND c.kind = 'agent' AND NOT EXISTS (SELECT 1 FROM flow_calls t WHERE t.run_id = c.run_id AND t.call_id = c.call_id AND t.event IN ('completed', 'null_model', 'held', 'refused', 'outcome_unknown', 'superseded'))),
 COALESCE((SELECT MAX(recorded_at) FROM flow_events e WHERE e.run_id = r.run_id AND e.seq < ?), '')
 FROM flow_runs r ORDER BY r.rowid`, pumpFlowForeignSeq)
		if err != nil {
			return err
		}
		for runs.Next() {
			var run pumpFlowRunRow
			var started, calls, events string
			var finished, open int
			if err := runs.Scan(&run.Run, &run.Actor, &started, &finished, &calls, &open, &events); err != nil {
				_ = runs.Close()
				return err
			}
			run.Finished, run.OpenCall = finished != 0, open != 0
			for _, stamp := range []string{started, calls, events} {
				if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil && t.After(run.Last) {
					run.Last = t
				}
			}
			data.Runs = append(data.Runs, run)
		}
		if err := runs.Close(); err != nil {
			return err
		}
		reports, err := q.QueryContext(ctx, `SELECT kind, run_id, payload_json, recorded_at FROM flow_events WHERE kind IN ('report', 'report_delivery', 'report_read') ORDER BY rowid`)
		if err != nil {
			return err
		}
		byID := map[string]*pumpFlowReport{}
		var order []string
		for reports.Next() {
			var kind, run, payload, at string
			if err := reports.Scan(&kind, &run, &payload, &at); err != nil {
				_ = reports.Close()
				return err
			}
			var doc map[string]any
			_ = json.Unmarshal([]byte(payload), &doc)
			id, _ := doc["report_id"].(string)
			if id == "" {
				continue
			}
			switch kind {
			case "report":
				if _, dup := byID[id]; dup {
					continue
				}
				r := &pumpFlowReport{ID: id, Run: run}
				r.Kind, _ = doc["report_kind"].(string)
				r.Milestone, _ = doc["milestone"].(string)
				r.Subject, _ = doc["subject"].(string)
				r.At, _ = time.Parse(time.RFC3339Nano, at)
				byID[id] = r
				order = append(order, id)
			case "report_delivery":
				if r := byID[id]; r != nil {
					r.LastDelivery, _ = doc["result"].(string)
				}
			case "report_read":
				if r := byID[id]; r != nil {
					r.Read = true
				}
			}
		}
		if err := reports.Close(); err != nil {
			return err
		}
		for _, id := range order {
			data.Reports = append(data.Reports, *byID[id])
		}
		return nil
	})
	return data, err
}
