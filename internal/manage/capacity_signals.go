package manage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The seams every signal reads through: package variables, so this issue adds no field to a type
// another issue's file declares. A test replaces them; a failed command is unknown or unmeasured.
var (
	capacityExec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return out, nil
	}
	capacityHTTPGet = func(ctx context.Context, url string) ([]byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		response, err := (&http.Client{Timeout: capacitySignalTimeout}).Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: status %d", url, response.StatusCode)
		}
		return io.ReadAll(io.LimitReader(response.Body, capacitySignalBodyLimit))
	}
)

const (
	// capacitySignalBodyLimit bounds an HTTP answer; capacitySignalTimeout bounds one read.
	capacitySignalBodyLimit = 8 << 20
	capacitySignalTimeout   = 30 * time.Second
	// capacityMergePage asks gh for more merges than any lane threshold is likely to name: its
	// default page is 30, and a short answer would understate the lane.
	capacityMergePage = "1000"
	// capacityIntegrationBranch is the branch a merge counts against; the status page URL is the
	// public default the section overrides.
	capacityIntegrationBranch       = "dev"
	capacityDefaultActionsStatusURL = "https://www.githubstatus.com/api/v2/incidents/unresolved.json"
	// capacityDeferNoCapacity is the relay's reason for a node deferred for want of a slot.
	capacityDeferNoCapacity = "defer:no_capacity"
)

func capacityMergeCount(ctx context.Context, cfg *Config, since time.Time) (*int, error) {
	if cfg.Repository == "" {
		return nil, nil
	}
	out, err := capacityExec(ctx, "gh", "pr", "list", "--repo", cfg.Repository, "--state", "merged",
		"--base", capacityIntegrationBranch, "--search", "merged:>="+since.UTC().Format(time.RFC3339),
		"--limit", capacityMergePage, "--json", "number,mergedAt")
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	count := len(rows)
	return &count, nil
}

// capacityActionsRead reports the first unresolved incident naming Actions; a read that fails, or
// an answer with no incidents field, is unknown with no incident.
func capacityActionsRead(ctx context.Context, url string) (string, *string) {
	if url == "" {
		url = capacityDefaultActionsStatusURL
	}
	body, err := capacityHTTPGet(ctx, url)
	if err != nil {
		return capacityUnknown, nil
	}
	var doc struct {
		Incidents *[]struct {
			Name       string `json:"name"`
			Components []struct {
				Name string `json:"name"`
			} `json:"components"`
		} `json:"incidents"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return capacityUnknown, nil
	}
	// An answer with no incidents field is not the status document this reads, so it is unknown
	// rather than a report of no outage.
	if doc.Incidents == nil {
		return capacityUnknown, nil
	}
	for _, incident := range *doc.Incidents {
		names := []string{incident.Name}
		for _, component := range incident.Components {
			names = append(names, component.Name)
		}
		for _, name := range names {
			if strings.Contains(name, "Actions") {
				incidentName := incident.Name
				return capacityMeasured, &incidentName
			}
		}
	}
	return capacityMeasured, nil
}

// capacityUsageRecord is one line of the child model usage log, and one attempt inside it.
type capacityUsageRecord struct {
	Timestamp int64                  `json:"timestamp"`
	Attempts  []capacityUsageAttempt `json:"attempts"`
}

type capacityUsageAttempt struct {
	Status int    `json:"status"`
	Model  string `json:"model"`
}

// capacityChild429Count counts the attempts whose status is 429 and whose model contains one of the
// child model strings. A log that is absent, unreadable or undecodable is unmeasured, and a last
// line still being written is not a record, so the count stops before it.
func capacityChild429Count(logPath string, models []string, since time.Time) (string, *int) {
	if logPath == "" {
		return capacityUnmeasured, nil
	}
	file, err := os.Open(logPath)
	if err != nil {
		return capacityUnmeasured, nil
	}
	defer file.Close()
	count := 0
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			break
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var record capacityUsageRecord
			if json.Unmarshal(line, &record) != nil {
				return capacityUnmeasured, nil
			}
			if record.Timestamp >= since.UnixMilli() {
				for _, attempt := range record.Attempts {
					if attempt.Status != 429 {
						continue
					}
					for _, model := range models {
						if model != "" && strings.Contains(attempt.Model, model) {
							count++
							break
						}
					}
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	return capacityMeasured, &count
}

type capacityWaiting struct {
	Waiting []string
	// Nodes are the node ids the pass counts as waiting: the ready nodes and the nodes deferred for
	// want of a slot. The branch reading judges readiness by node id, because a plan may hold two
	// nodes with one issue key (a redefinition) and their states must not mix.
	WaitingNodes []string
	// PlanRevision is the plan revision the pass answered for. The branch reading compares it with the
	// revision of its own snapshot: when they differ the pass's readiness describes another plan, so
	// the reading takes its readiness from the snapshot instead (branchAttach).
	PlanRevision int64
	Held         int
	Ceiling      int
	HostMemory   string
}

// capacityWaitingFor asks the relay for one plan's ready set: the ready nodes and the nodes
// deferred for want of capacity, with the pass's slots and host memory bound. A relay that refuses
// or answers something unreadable is the read failure reported as exit 3, except when the store
// itself lacks the DAG zone (zoneReason): there the plan's own question has no answer to read,
// whatever shape the relay's failure takes — the refusal a store with no zone at all gives, or the
// raw table error a partially installed one gives — so the pass reads as empty and the branch
// reading reports the plan's branches as unmeasured rather than the command failing. A relay failure
// beside a whole zone stays the read failure it is.
func capacityWaitingFor(ctx context.Context, e *Env, cfg *Config, plan, zoneReason string, headerMissing bool) (capacityWaiting, error) {
	stdout, code, err := e.Relay(ctx, cfg, "dag-ready", "--plan", plan)
	if err != nil {
		return capacityWaiting{}, err
	}
	if code != 0 {
		if zoneReason != "" && capacityZoneFailure(stdout, headerMissing) {
			return capacityWaiting{}, nil
		}
		return capacityWaiting{}, fmt.Errorf("relay dag-ready --plan %s: exit %d", plan, code)
	}
	var reading struct {
		PlanRevision int64 `json:"plan_revision"`
		Pass         struct {
			Held       int `json:"held"`
			Ceiling    int `json:"ceiling"`
			HostMemory *struct {
				State string `json:"state"`
			} `json:"host_memory"`
		} `json:"pass"`
		Ready []struct {
			NodeID   string `json:"node_id"`
			IssueKey string `json:"issue_key"`
		} `json:"ready"`
		Nodes []struct {
			NodeID   string `json:"node_id"`
			IssueKey string `json:"issue_key"`
			Reason   string `json:"reason"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(stdout, &reading); err != nil {
		return capacityWaiting{}, fmt.Errorf("relay dag-ready --plan %s: %w", plan, err)
	}
	set := map[string]struct{}{}
	nodes := map[string]struct{}{}
	for _, node := range reading.Ready {
		if node.IssueKey != "" {
			set[node.IssueKey] = struct{}{}
		}
		if node.NodeID != "" {
			nodes[node.NodeID] = struct{}{}
		}
	}
	for _, node := range reading.Nodes {
		if node.Reason != capacityDeferNoCapacity {
			continue
		}
		if node.IssueKey != "" {
			set[node.IssueKey] = struct{}{}
		}
		if node.NodeID != "" {
			nodes[node.NodeID] = struct{}{}
		}
	}
	waiting := make([]string, 0, len(set))
	for key := range set {
		waiting = append(waiting, key)
	}
	sort.Strings(waiting)
	waitingNodes := make([]string, 0, len(nodes))
	for id := range nodes {
		waitingNodes = append(waitingNodes, id)
	}
	sort.Strings(waitingNodes)
	out := capacityWaiting{Waiting: waiting, WaitingNodes: waitingNodes, PlanRevision: reading.PlanRevision,
		Held: reading.Pass.Held, Ceiling: reading.Pass.Ceiling}
	if reading.Pass.HostMemory != nil {
		out.HostMemory = reading.Pass.HostMemory.State
	}
	return out, nil
}

// capacityZoneReason reads the store's DAG zone tables, read-only and without repairing anything, and
// returns why a branch reading cannot be measured: "" when every table the reading needs is present,
// otherwise the reason a store that predates the zone — or one an interrupted install left partial —
// gives. It runs before the relay is asked, because a partially installed zone makes dag-ready fail
// with a raw table error rather than a refusal, and that failure is the same missing zone. A store
// that is absent or cannot be opened is not this case, so a relay failure beside it stays the read
// failure it is rather than reading as an unmeasured answer.
func capacityZoneReason(ctx context.Context, stateDir string) (string, bool, error) {
	if stateDir == "" {
		return "", false, nil
	}
	handle, err := dagReviewOpenStore(ctx, stateDir)
	if err != nil {
		if errors.Is(err, ErrRelayStoreAbsent) {
			return "", false, nil
		}
		return "", false, err
	}
	defer handle.Close()
	var missing []string
	headerMissing := false
	for _, table := range branchZoneTables {
		present, err := handle.hasTable(ctx, table)
		if err != nil {
			return "", false, err
		}
		if !present {
			missing = append(missing, table)
			if table == "dag_plans" {
				headerMissing = true
			}
		}
	}
	return capacityZoneReasonText(missing), headerMissing, nil
}

// capacityWaitingNodeIDs is the node ids a reading counts as waiting: the ready nodes and the nodes
// deferred for want of a slot. The branch reading judges readiness by node id, and it takes these ids
// from the pass that answered for its own plan revision or, when that pass answered for another one,
// from its own reading of the snapshot, so the same extraction serves both.
func capacityWaitingNodeIDs(reading dagsched.Reading) []string {
	nodes := map[string]struct{}{}
	for _, node := range reading.Ready {
		if node.NodeID != "" {
			nodes[node.NodeID] = struct{}{}
		}
	}
	for _, node := range reading.Nodes {
		if node.Reason == capacityDeferNoCapacity && node.NodeID != "" {
			nodes[node.NodeID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// capacityZoneReasonText is the one wording of the missing-zone reason, so the preflight and the
// reading inside the snapshot say the same thing about the same store.
func capacityZoneReasonText(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return "the store predates the DAG zone (no " + strings.Join(missing, ", ") + ")"
}

// capacityZoneFailure reports whether the relay's failed answer is the one a missing DAG zone
// produces, so that a store the local check already found zone-less can be read as unmeasured rather
// than as a failure. Two shapes count, and both are the relay's own words for the missing zone: the
// host failure a partially installed zone gives, whose detail names the table SQLite could not find
// (the same test the relay's own reading makes, isMissingZone in internal/relay/dag), and — only when
// the plan header table itself is absent — the refusal a store with no zone at all gives (error
// refused, reason unregistered_scope).
//
// headerMissing is what keeps the second shape honest: unregistered_scope is also the refusal for a
// plan the store does not hold, so a store that keeps dag_plans but is missing another zone table
// would otherwise turn a mistyped plan id into a fabricated empty reading. When the header table is
// there, a refused lookup is the relay saying the plan is unknown, which stays the read failure it is.
func capacityZoneFailure(stdout []byte, headerMissing bool) bool {
	var answer struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(stdout, &answer); err != nil {
		return false
	}
	if answer.Error == "host" && strings.Contains(answer.Detail, "no such table: dag_") {
		return true
	}
	return headerMissing && answer.Error == "refused" && answer.Reason == string(contract.RefusalUnregisteredScope)
}

// capacityReceiptWaitFor reads the relay store read-only: the median and count of the acknowledged
// deliveries that reached one parent in the window, leaving out the interrupted ones. The store is
// opened through the relay-read helper, which resolves the path the way SQLite does, so a configured
// state spelling that carries a symlink or a .. component reads the same store dag-ready and the
// branch reading read rather than another one a cleaned path would name (CRW-865).
func capacityReceiptWaitFor(ctx context.Context, stateDir, parent string, since time.Time) (CapacityReceiptWait, error) {
	handle, err := relayReadOpenStore(ctx, stateDir)
	if err != nil {
		return CapacityReceiptWait{}, err
	}
	defer handle.Close()
	rows, err := handle.QueryContext(ctx,
		"SELECT d.created_at, a.ack_at FROM deliveries d"+
			" JOIN acks a ON a.event_id = d.event_id"+
			" JOIN events e ON e.event_id = d.event_id"+
			" WHERE d.recipient_task_id = ? AND d.created_at >= ? AND e.outcome != 'interrupted'",
		parent, capacityStamp(since))
	if err != nil {
		return CapacityReceiptWait{}, err
	}
	defer rows.Close()
	var waits []float64
	for rows.Next() {
		var createdAt, ackAt string
		if err := rows.Scan(&createdAt, &ackAt); err != nil {
			return CapacityReceiptWait{}, err
		}
		created, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			continue
		}
		if acked, err := time.Parse(time.RFC3339, ackAt); err == nil {
			waits = append(waits, acked.Sub(created).Minutes())
		}
	}
	if err := rows.Err(); err != nil {
		return CapacityReceiptWait{}, err
	}
	// The median, rounded as the judgement prints it.
	out := CapacityReceiptWait{Count: len(waits)}
	if len(waits) == 0 {
		return out, nil
	}
	sort.Float64s(waits)
	middle := len(waits) / 2
	median := waits[middle]
	if len(waits)%2 == 0 {
		median = (waits[middle-1] + waits[middle]) / 2
	}
	rounded := math.Round(median*10) / 10
	out.MedianMinutes = &rounded
	return out, nil
}

// capacityStamp formats a time exactly as the relay stores its timestamps, so the SQL window
// comparison is between like forms: the relay writes microseconds with a numeric UTC offset, and a
// whole-second stamp would sort before a stored one inside the same second.
func capacityStamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000+00:00") }
