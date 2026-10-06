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

// pumpTruncate cuts text at limit characters on a rune boundary, so a cut never leaves a broken
// rune, and marks that it cut.
func pumpTruncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + pumpRolloutTruncated
}

// pumpRolloutSource reads the configured parents' rollout files: a finished turn's report and a
// parent's question to the user. A rollout seen for the first time only records its end offset,
// so no past history is sent.
type pumpRolloutSource struct{}

func (pumpRolloutSource) Name() string { return "rollout" }
func (pumpRolloutSource) Relay() bool  { return false }

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
	for _, thread := range threads {
		path, err := pumpRolloutPath(e, thread)
		if err != nil {
			return pumpSourceResult{Status: pumpSourceUnmeasured}
		}
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return pumpSourceResult{Status: pumpSourceUnmeasured}
		}
		start, known := st.Offsets[path]
		if !known {
			// First sight: the offset goes to the end and no history is sent.
			st.Offsets[path] = info.Size()
			continue
		}
		if info.Size() <= start {
			continue
		}
		lines, offset, err := pumpRolloutLines(path, start)
		if err != nil {
			return pumpSourceResult{Status: pumpSourceUnmeasured}
		}
		for i, line := range lines {
			if ev, ok := pumpRolloutEvent(thread, cfg.Parents[thread], path, start+int64(i), line); ok {
				events = append(events, ev)
			}
		}
		st.Offsets[path] = offset
	}
	return pumpSourceResult{Status: pumpSourceOK, Events: events}
}

// pumpPRSource reads the repository's pull requests and reports only the ones whose title carries
// an issue key and whose state changed since the last round.
type pumpPRSource struct{}

func (pumpPRSource) Name() string { return "pr" }
func (pumpPRSource) Relay() bool  { return false }

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
	key := regexp.MustCompile(pattern)
	current := map[string]string{}
	for _, row := range rows {
		match := key.FindString(row.Title)
		if match == "" {
			continue
		}
		current[fmt.Sprintf("%d", row.Number)] = fmt.Sprintf("%s #%d %s", match, row.Number, row.State)
	}
	previous := st.PRs
	seen := st.PRsSeen
	st.PRs, st.PRsSeen = current, true
	if !seen {
		// The first run stores the state only.
		return pumpSourceResult{Status: pumpSourceOK}
	}
	var changed []string
	lowOnly := true
	for number, value := range current {
		if previous[number] == value {
			continue
		}
		changed = append(changed, value)
		if !strings.HasSuffix(value, " OPEN") && !strings.HasSuffix(value, " MERGED") {
			lowOnly = false
		}
	}
	if len(changed) == 0 {
		return pumpSourceResult{Status: pumpSourceOK}
	}
	sort.Strings(changed)
	kind := pumpKindPR
	if lowOnly {
		kind = pumpKindLow
	}
	return pumpSourceResult{Status: pumpSourceOK, Events: []pumpEvent{{
		ID: "pr#" + strings.Join(changed, ","), Kind: kind,
		Text: "PR changes: " + strings.Join(changed, ", "),
	}}}
}
