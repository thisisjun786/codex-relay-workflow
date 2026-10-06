package manage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// The seams every signal reads through: package variables, so this issue adds no field to a type
// another issue's file declares. A test replaces them; a failed command is unknown or unmeasured
// to the caller, never a value.
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
	// capacityIntegrationBranch is the branch a merge counts against; the status page URL is the
	// public default the section overrides.
	capacityIntegrationBranch       = "dev"
	capacityDefaultActionsStatusURL = "https://www.githubstatus.com/api/v2/incidents/unresolved.json"
	// capacityDeferNoCapacity is the relay's reason for a node deferred for want of a slot.
	capacityDeferNoCapacity = "defer:no_capacity"
)

// capacityMergeCount is the merges into the integration branch inside the window.
func capacityMergeCount(ctx context.Context, cfg *Config, since time.Time) (*int, error) {
	if cfg.Repository == "" {
		return nil, nil
	}
	out, err := capacityExec(ctx, "gh", "pr", "list", "--repo", cfg.Repository, "--state", "merged",
		"--base", capacityIntegrationBranch, "--search", "merged:>="+since.UTC().Format(time.RFC3339),
		"--json", "number,mergedAt")
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

// capacityActionsRead reports the first unresolved incident naming Actions. A read that fails is
// unknown with no incident, which suppresses nothing.
func capacityActionsRead(ctx context.Context, url string) (string, *string) {
	if url == "" {
		url = capacityDefaultActionsStatusURL
	}
	body, err := capacityHTTPGet(ctx, url)
	if err != nil {
		return capacityUnknown, nil
	}
	var doc struct {
		Incidents []struct {
			Name       string `json:"name"`
			Components []struct {
				Name string `json:"name"`
			} `json:"components"`
		} `json:"incidents"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return capacityUnknown, nil
	}
	for _, incident := range doc.Incidents {
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

// capacityChild429Count counts the attempts whose status is 429 and whose model contains one of the
// child model strings. A log that is not configured, or cannot be read, is unmeasured with no count.
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
	decoder := json.NewDecoder(file)
	for {
		var record struct {
			Timestamp int64 `json:"timestamp"`
			Attempts  []struct {
				Status int    `json:"status"`
				Model  string `json:"model"`
			} `json:"attempts"`
		}
		if err := decoder.Decode(&record); err != nil {
			break
		}
		if record.Timestamp < since.UnixMilli() {
			continue
		}
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
	return capacityMeasured, &count
}

type capacityWaiting struct {
	Waiting    []string
	Held       int
	Ceiling    int
	HostMemory string
}

// capacityWaitingFor asks the relay for one plan's ready set: the ready nodes and the nodes deferred
// for want of capacity, with the pass's slots and host memory bound. A relay that refuses or answers
// something unreadable is the read failure reported as exit 3.
func capacityWaitingFor(ctx context.Context, e *Env, cfg *Config, plan string) (capacityWaiting, error) {
	stdout, code, err := e.Relay(ctx, cfg, "dag-ready", "--plan", plan)
	if err != nil {
		return capacityWaiting{}, err
	}
	if code != 0 {
		return capacityWaiting{}, fmt.Errorf("relay dag-ready --plan %s: exit %d", plan, code)
	}
	var reading struct {
		Pass struct {
			Held       int `json:"held"`
			Ceiling    int `json:"ceiling"`
			HostMemory *struct {
				State string `json:"state"`
			} `json:"host_memory"`
		} `json:"pass"`
		Ready []struct {
			IssueKey string `json:"issue_key"`
		} `json:"ready"`
		Nodes []struct {
			IssueKey string `json:"issue_key"`
			Reason   string `json:"reason"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(stdout, &reading); err != nil {
		return capacityWaiting{}, fmt.Errorf("relay dag-ready --plan %s: %w", plan, err)
	}
	set := map[string]struct{}{}
	for _, node := range reading.Ready {
		if node.IssueKey != "" {
			set[node.IssueKey] = struct{}{}
		}
	}
	for _, node := range reading.Nodes {
		if node.Reason == capacityDeferNoCapacity && node.IssueKey != "" {
			set[node.IssueKey] = struct{}{}
		}
	}
	waiting := make([]string, 0, len(set))
	for key := range set {
		waiting = append(waiting, key)
	}
	sort.Strings(waiting)
	out := capacityWaiting{Waiting: waiting, Held: reading.Pass.Held, Ceiling: reading.Pass.Ceiling}
	if reading.Pass.HostMemory != nil {
		out.HostMemory = reading.Pass.HostMemory.State
	}
	return out, nil
}

// capacityReceiptWaitFor reads the relay store read-only: the median and count of the deliveries
// that reached one parent inside the window, leaving out the interrupted ones.
func capacityReceiptWaitFor(ctx context.Context, stateDir, parent string, since time.Time) (CapacityReceiptWait, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(stateDir, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		return CapacityReceiptWait{}, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx,
		"SELECT d.created_at, a.ack_at FROM deliveries d"+
			" JOIN acks a ON a.event_id = d.event_id"+
			" JOIN events e ON e.event_id = d.event_id"+
			" WHERE d.recipient_task_id = ? AND d.created_at >= ? AND e.outcome != 'interrupted'",
		parent, since.UTC().Format(time.RFC3339))
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
