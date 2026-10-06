package manage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// The pump's sources: the parent rollout reader and the PR reader. Each is registered in the one
// pumpSources list; a later node adds its own source the same way. Every helper name starts with
// pumpRollout or pumpPR so the package's single namespace stays collision free.
const (
	// pumpRolloutSessions is the glob below the Codex home that holds one parent's rollouts.
	pumpRolloutSessions = "sessions"
	// pumpRolloutTruncated marks a body the source cut at the issue's limit.
	pumpRolloutTruncated = "\n[truncated]"
)

// pumpExec is the seam the PR source reads gh through, so no test reaches the network. It is a
// package variable of the pump's own rather than a reuse of capacityExec, because a pump test
// must not have to know it shares a seam with capacity.
var pumpExec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// pumpTruncate cuts text to limit CHARACTERS, not bytes, and marks that it cut. A multibyte report
// is measured in runes so a 6000-character limit is 6000 characters; the marker is reserved inside
// the limit so the result never exceeds it.
func pumpTruncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	marker := utf8.RuneCountInString(pumpRolloutTruncated)
	room := limit - marker
	if room < 0 {
		room = 0
	}
	runes := []rune(text)
	if len(runes) > room {
		runes = runes[:room]
	}
	return string(runes) + pumpRolloutTruncated
}

// pumpRolloutSource reads the configured parents' rollout files: a finished turn's report and a
// parent's question to the user. A rollout seen for the first time only records its end offset,
// so no past history is sent.
type pumpRolloutSource struct{}

func (pumpRolloutSource) Name() string { return "rollout" }

// pumpRolloutPath is the last-by-name rollout file of one parent thread, or empty when there is
// none. The thread names one path component; a thread carrying a separator or a glob character is
// refused rather than allowed to widen the pattern.
func pumpRolloutPath(e *Env, thread string) (string, error) {
	if err := deliverPathComponentLimit(thread, "parent thread", deliverThreadLimit); err != nil {
		return "", err
	}
	if strings.ContainsAny(thread, "*?[]") {
		return "", fmt.Errorf("crw manage pump: parent thread %q carries a glob character", thread)
	}
	pattern := filepath.Join(coreHomeDir(e, "CODEX_HOME", ".codex"), pumpRolloutSessions, "*", "*", "*", "*"+thread+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("crw manage pump: the rollout glob: %w", err)
	}
	if len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches)
	return matches[len(matches)-1], nil
}

// pumpRolloutLines is the complete lines of a rollout file after start, and the offset after the
// last complete line. A half-written final line is left for the next round, so an event is never
// read from a line still being written.
func pumpRolloutLines(path string, start int64) ([]string, int64, error) {
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
	var lines []string
	for {
		line, readErr := reader.ReadString('\n')
		if !strings.HasSuffix(line, "\n") {
			// The last line is incomplete: leave it for the next round.
			break
		}
		offset += int64(len(line))
		lines = append(lines, strings.TrimRight(line, "\n"))
		if readErr != nil {
			break
		}
	}
	return lines, offset, nil
}

// pumpRolloutEvent turns one rollout line into an event, or reports that the line carries none.
// A report's id and body head both carry the parent thread id and the turn id, so a management
// session can match a rollout report with the supervisor channel's message for the same turn.
func pumpRolloutEvent(thread, label, path string, offset int64, line string) (pumpEvent, bool) {
	var record struct {
		Payload struct {
			Type             string `json:"type"`
			TurnID           string `json:"turn_id"`
			LastAgentMessage string `json:"last_agent_message"`
			Name             string `json:"name"`
			Arguments        string `json:"arguments"`
			Input            string `json:"input"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		return pumpEvent{}, false
	}
	switch {
	case record.Payload.Type == "task_complete":
		message := strings.TrimSpace(record.Payload.LastAgentMessage)
		body := pumpTruncate(message, pumpReportLimit)
		head := fmt.Sprintf("[%s] report thread=%s turn=%s", label, thread, record.Payload.TurnID)
		return pumpEvent{ID: thread + "#" + record.Payload.TurnID, Kind: pumpKindReport, Text: head + "\n" + body}, true
	case (record.Payload.Type == "function_call" || record.Payload.Type == "custom_tool_call") &&
		strings.Contains(record.Payload.Name, "request_user_input"):
		arguments := record.Payload.Arguments
		if arguments == "" {
			arguments = record.Payload.Input
		}
		body := pumpTruncate(strings.TrimSpace(arguments), pumpQuestionLimit)
		head := fmt.Sprintf("[%s] question thread=%s (%s)", label, thread, record.Payload.Name)
		id := fmt.Sprintf("%s#%d", path, offset)
		return pumpEvent{ID: id, Kind: pumpKindQuestion, Text: head + "\n" + body}, true
	}
	return pumpEvent{}, false
}

// Collect reads every configured parent's new rollout lines. rollout_reports=false opens no
// rollout file and produces no report or question event, while every other source is unaffected.
func (pumpRolloutSource) Collect(_ context.Context, e *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult {
	if s.RolloutReports != nil && !*s.RolloutReports {
		return pumpSourceResult{Status: pumpSourceOK}
	}
	threads := make([]string, 0, len(cfg.Parents))
	for thread := range cfg.Parents {
		threads = append(threads, thread)
	}
	sort.Strings(threads)
	var events []pumpEvent
	read, failed := 0, 0
	for _, thread := range threads {
		path, err := pumpRolloutPath(e, thread)
		if err != nil {
			// One parent the source cannot read must not lose the other parents' events or their
			// offsets, so it is counted and the walk continues.
			failed++
			continue
		}
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			failed++
			continue
		}
		start, known := st.Offsets[path]
		if !known {
			// First sight: the offset goes to the end and no history is sent.
			st.Offsets[path] = info.Size()
			read++
			continue
		}
		if info.Size() <= start {
			read++
			continue
		}
		lines, offset, err := pumpRolloutLines(path, start)
		if err != nil {
			failed++
			continue
		}
		for i, line := range lines {
			if ev, ok := pumpRolloutEvent(thread, cfg.Parents[thread], path, start+int64(i), line); ok {
				events = append(events, ev)
			}
		}
		st.Offsets[path] = offset
		read++
	}
	// The source is unmeasured only when every parent it tried failed; a partial read still
	// delivers the events it got.
	status := pumpSourceOK
	if read == 0 && failed > 0 {
		status = pumpSourceUnmeasured
	}
	return pumpSourceResult{Status: status, Events: events}
}

// pumpPRSource reads the repository's pull requests and reports only the ones whose title carries
// an issue key and whose state changed since the last round.
type pumpPRSource struct{}

func (pumpPRSource) Name() string { return "pr" }

// pumpPRStateOf is the state word a previously stored "KEY #n STATE" value ends with.
func pumpPRStateOf(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return "UNKNOWN"
	}
	return fields[len(fields)-1]
}

// pumpPRRow is one row of gh pr list's answer.
type pumpPRRow struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
}

// Collect runs gh pr list, keeps the titles matching the configured issue pattern, and compares
// them with the state. The first run stores the state and sends nothing. A gh read that fails is
// unmeasured and emits no event.
func (pumpPRSource) Collect(ctx context.Context, _ *Env, cfg *Config, st *pumpState, s pumpSettings) pumpSourceResult {
	if cfg.Repository == "" {
		return pumpSourceResult{Status: pumpSourceOK}
	}
	out, err := pumpExec(ctx, "gh", "pr", "list", "--repo", cfg.Repository, "--state", "all", "--limit", "40", "--json", "number,state,title")
	if err != nil {
		return pumpSourceResult{Status: pumpSourceUnmeasured}
	}
	var rows []pumpPRRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return pumpSourceResult{Status: pumpSourceUnmeasured}
	}
	pattern := s.IssuePattern
	if pattern == "" {
		pattern = pumpDefaultIssuePattern
	}
	// A pattern the product cannot compile is refused as an unusable setting rather than allowed
	// to panic the whole pump.
	key, err := regexp.Compile(pattern)
	if err != nil {
		return pumpSourceResult{Status: pumpSourceUnmeasured}
	}
	current := map[string]string{}
	states := map[string]string{}
	for _, row := range rows {
		match := key.FindString(row.Title)
		if match == "" {
			continue
		}
		number := fmt.Sprintf("%d", row.Number)
		current[number] = fmt.Sprintf("%s #%d %s", match, row.Number, row.State)
		states[number] = row.State
	}
	previous := st.PRs
	// A state written by the ported workflow already carries the prs baseline but not the
	// prs_seen flag, so a non-empty baseline counts as initialized and its changes are reported
	// rather than silently swallowed by a fresh first run.
	seen := st.PRsSeen || len(previous) > 0
	st.PRs, st.PRsSeen = current, true
	if !seen {
		// The first run stores the state only.
		return pumpSourceResult{Status: pumpSourceOK}
	}
	var changed []string
	ids := []string{}
	lowOnly := true
	for number, value := range current {
		if previous[number] == value {
			continue
		}
		changed = append(changed, value)
		// The id names the transition, so a PR that returns to an earlier state is a new event
		// rather than one the sent set already holds.
		ids = append(ids, fmt.Sprintf("%s:%s>%s", number, pumpPRStateOf(previous[number]), states[number]))
		if !strings.HasSuffix(value, " OPEN") && !strings.HasSuffix(value, " MERGED") {
			lowOnly = false
		}
	}
	if len(changed) == 0 {
		return pumpSourceResult{Status: pumpSourceOK}
	}
	sort.Strings(changed)
	sort.Strings(ids)
	kind := pumpKindPR
	if lowOnly {
		kind = pumpKindLow
	}
	return pumpSourceResult{Status: pumpSourceOK, Events: []pumpEvent{{
		ID: "pr#" + strings.Join(ids, ","), Kind: kind,
		Text: "PR changes: " + strings.Join(changed, ", "),
	}}}
}
