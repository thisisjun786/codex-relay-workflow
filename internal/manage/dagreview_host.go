package manage

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The four host-record anomaly kinds this issue adds to the review.
const (
	dagHostKindParentSystemError       = "parent_system_error"
	dagHostKindDuplicateToolOutputs    = "duplicate_tool_outputs"
	dagHostKindChildTurnWithoutReceipt = "child_turn_without_receipt"
	dagHostKindParentDagRefusals       = "parent_dag_refusals"
)

// dagHostReceiptDefaultMinutes is how long a child's newest turn may stand ended with no accepted
// receipt in its generation before the review reports it; the dag_review section's receipt_minutes
// overrides it.
const dagHostReceiptDefaultMinutes = 20

// dagHostStateFile is the offset file below the state directory: one resume offset per rollout, so
// a refusal the review reported once is not reported again.
const dagHostStateFile = "dag-review-state.json"

// dagHostLineLimit bounds one rollout line this review reads; a longer line is a reading it cannot
// take and is reported as unmeasured rather than skipped silently.
const dagHostLineLimit = 8 << 20

// dagHostScope is what the host readings need and the source signature does not carry: the App
// Server socket, the parent threads to read, the state directory the offsets live under, the
// receipt threshold and whether offsets are used at all. DagReview binds it onto the context it
// runs the sources with, so one review's scope is its own and two reviews in one process cannot
// see each other's.
type dagHostScope struct {
	socket   string
	parents  map[string]string
	stateDir string
	receipt  time.Duration
	noState  bool
}

// The two context keys the host readings travel on.
type (
	dagHostScopeKey   struct{}
	dagHostNoStateKey struct{}
)

// dagHostNoState marks ctx as a review that reads and writes no offsets (--no-state). It is the one
// datum the review body does not already carry, so it rides the context beside the scope.
func dagHostNoState(ctx context.Context, noState bool) context.Context {
	return context.WithValue(ctx, dagHostNoStateKey{}, noState)
}

// dagHostBind attaches the host-reading scope to ctx, taking the --no-state flag from it.
func dagHostBind(ctx context.Context, cfg *Config) context.Context {
	section := dagHostSection{}
	_ = cfg.Section("dag_review", &section)
	receipt := dagHostReceiptDefaultMinutes
	if section.ReceiptMinutes > 0 {
		receipt = section.ReceiptMinutes
	}
	noState, _ := ctx.Value(dagHostNoStateKey{}).(bool)
	return context.WithValue(ctx, dagHostScopeKey{}, dagHostScope{
		socket: cfg.Relay.Socket, parents: cfg.Parents, stateDir: cfg.StateDir,
		receipt: time.Duration(receipt) * time.Minute, noState: noState,
	})
}

// dagHostScopeOf is the scope DagReview attached to a review; the second result is false when a
// source list ran without one.
func dagHostScopeOf(ctx context.Context) (dagHostScope, bool) {
	scope, ok := ctx.Value(dagHostScopeKey{}).(dagHostScope)
	return scope, ok
}

// dagHostSection is the dag_review section as the host readings read it.
type dagHostSection struct {
	ReceiptMinutes int `json:"receipt_minutes"`
}

// config is the configuration a host read runs with: the socket the review was given.
func (s dagHostScope) config() *Config {
	return &Config{Relay: coreRelay{Socket: s.socket}, raw: map[string]json.RawMessage{}}
}

// dagHostSources reads the four host-record anomalies this issue adds: a parent thread in
// systemError, duplicate tool outputs in a parent's rollout, an active child whose newest turn
// ended with no accepted receipt, and the relay dag- refusals a parent's rollout gained since the
// last check. A reading it cannot take is an unmeasured check, never an anomaly.
func dagHostSources(ctx context.Context, in *dagReviewInput) error {
	scope, ok := dagHostScopeOf(ctx)
	if !ok {
		in.review.Checks = append(in.review.Checks, Check{
			Name: "host_scope", State: dagReviewUnmeasured,
			Detail: "the review ran without a host-reading scope",
		})
		return nil
	}
	if err := dagHostParentsRead(ctx, in, scope); err != nil {
		return err
	}
	return dagHostChildrenRead(ctx, in, scope)
}

// dagHostParent is one configured parent thread: the thread id and the label the configuration
// gives it.
type dagHostParent struct {
	id    string
	label string
}

// dagHostSortedParents is the configured parents in a stable order.
func dagHostSortedParents(parents map[string]string) []dagHostParent {
	out := make([]dagHostParent, 0, len(parents))
	for id, label := range parents {
		out = append(out, dagHostParent{id: id, label: label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// dagHostParentsRead reads every configured parent's thread and rollout.
func dagHostParentsRead(ctx context.Context, in *dagReviewInput, scope dagHostScope) error {
	cfg := scope.config()
	offsets := dagHostOffsets{Offsets: map[string]int64{}, raw: map[string]json.RawMessage{}}
	offsetsRead := false
	if !scope.noState && scope.stateDir != "" {
		state, err := dagHostLoadOffsets(scope.stateDir)
		if err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "dag_review_state", State: dagReviewUnmeasured, Detail: err.Error(),
			})
		} else {
			offsets, offsetsRead = state, true
		}
	}
	for _, parent := range dagHostSortedParents(scope.parents) {
		raw, err := HostRead(ctx, cfg, "thread/read", map[string]any{"threadId": parent.id, "includeTurns": false})
		if err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "parent_thread:" + parent.id, State: dagReviewUnmeasured,
				Detail: dagHostReadDetail("thread/read", err),
			})
			continue
		}
		var read struct {
			Thread struct {
				Path   string `json:"path"`
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &read); err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "parent_thread:" + parent.id, State: dagReviewUnmeasured,
				Detail: "the thread/read answer is not readable: " + err.Error(),
			})
			continue
		}
		if read.Thread.Status.Type == "systemError" {
			in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
				Kind: dagHostKindParentSystemError, Issue: parent.label,
				Detail: "the parent thread is in systemError and takes no turn",
			})
		}
		if read.Thread.Path == "" {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "parent_rollout:" + parent.id, State: dagReviewUnmeasured,
				Detail: "the thread/read answer names no rollout path",
			})
			continue
		}
		dagHostParentRollout(in, parent, read.Thread.Path, &offsets, offsetsRead)
	}
	if offsetsRead {
		// A cancelled review writes no durable effect: the offset file is the one host state this
		// review touches, and a cancelled run must leave it as it was.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := dagHostSaveOffsets(scope.stateDir, offsets); err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "dag_review_state", State: dagReviewUnmeasured,
				Detail: "the offsets could not be written: " + err.Error(),
			})
		}
	}
	return nil
}

// dagHostParentRollout reads one parent's rollout: the call ids whose tool results repeat, and the
// relay dag- refusals written since this rollout's resume offset. The offsets map is updated with
// the offset the next check resumes from.
func dagHostParentRollout(in *dagReviewInput, parent dagHostParent, path string, offsets *dagHostOffsets, offsetsRead bool) {
	duplicates, err := dagHostDuplicateOutputs(path)
	if err != nil {
		in.review.Checks = append(in.review.Checks, Check{
			Name: "parent_rollout:" + parent.id, State: dagReviewUnmeasured, Detail: err.Error(),
		})
	} else {
		for _, duplicate := range duplicates {
			in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
				Kind: dagHostKindDuplicateToolOutputs, Issue: parent.label,
				Detail: fmt.Sprintf("call %s has %d tool results in the rollout; the next context compaction fails",
					duplicate.callID, duplicate.count),
			})
		}
	}
	start := int64(0)
	if offsetsRead {
		seen, ok := offsets.Offsets[path]
		if !ok {
			// A rollout this review has not seen before starts at its end: its earlier refusals were
			// reported by an earlier check or are history this run does not re-report.
			size, err := dagHostFileSize(path)
			if err != nil {
				in.review.Checks = append(in.review.Checks, Check{
					Name: "parent_refusals:" + parent.id, State: dagReviewUnmeasured, Detail: err.Error(),
				})
				return
			}
			offsets.Offsets[path] = size
			return
		}
		start = seen
	}
	refusals, resume, err := dagHostRolloutRefusals(path, start)
	if err != nil {
		in.review.Checks = append(in.review.Checks, Check{
			Name: "parent_refusals:" + parent.id, State: dagReviewUnmeasured, Detail: err.Error(),
		})
		return
	}
	for _, refusal := range refusals {
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagHostKindParentDagRefusals, Issue: parent.label,
			Detail: fmt.Sprintf("the relay command %s was refused: %s", strings.Join(refusal.commands, ","), refusal.reason),
		})
	}
	if offsetsRead {
		offsets.Offsets[path] = resume
	}
}

// dagHostDuplicate is one call id whose tool results repeat in a rollout.
type dagHostDuplicate struct {
	callID string
	count  int
}

// dagHostRolloutEntry is the part of a rollout line this review reads.
type dagHostRolloutEntry struct {
	Type    string `json:"type"`
	Payload struct {
		Type      string          `json:"type"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
		Output    json.RawMessage `json:"output"`
	} `json:"payload"`
}

// dagHostDuplicateOutputs counts the tool results of each call id in a rollout and reports every
// call id seen twice or more. History before the last compaction is no longer sent to the model, so
// the count restarts at a compaction line.
func dagHostDuplicateOutputs(path string) ([]dagHostDuplicate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), dagHostLineLimit)
	counts := map[string]int{}
	for scanner.Scan() {
		var entry dagHostRolloutEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry.Type == "compacted" {
			counts = map[string]int{}
			continue
		}
		if entry.Type != "response_item" || entry.Payload.CallID == "" {
			continue
		}
		if entry.Payload.Type == "function_call_output" || entry.Payload.Type == "custom_tool_call_output" {
			counts[entry.Payload.CallID]++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the rollout: %w", err)
	}
	duplicates := make([]dagHostDuplicate, 0)
	for _, callID := range dagReviewSortedKeys(counts) {
		if counts[callID] > 1 {
			duplicates = append(duplicates, dagHostDuplicate{callID: callID, count: counts[callID]})
		}
	}
	return duplicates, nil
}

// dagHostRefusal is one relay dag- command a parent's rollout shows refused.
type dagHostRefusal struct {
	callID   string
	commands []string
	reason   string
}

// The relay's own refusal shape in a dag- command's output: an "ok" of false and a refused reason.
var (
	dagHostDagCommand    = regexp.MustCompile("\\bdag-[a-z-]+")
	dagHostRefusedOutput = regexp.MustCompile("\"ok\"\\s*:\\s*false")
	dagHostRefusedReason = regexp.MustCompile("\"reason\"\\s*:\\s*\"([a-z_]+)\"")
)

// dagHostRolloutRefusals reads the rollout from start to its end and reports the relay dag-
// refusals it finds, with the offset the next check resumes from. The resume offset is the start of
// the last line read, so a command and its output split across two checks is still paired.
func dagHostRolloutRefusals(path string, start int64) ([]dagHostRefusal, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, start, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, start, err
	}
	if start > size {
		start = size
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, start, err
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	offset, resume := start, start
	calls := map[string]dagHostCall{}
	var refusals []dagHostRefusal
	for {
		lineStart := offset
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			offset += int64(len(line))
			resume = lineStart
			var entry dagHostRolloutEntry
			if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &entry) == nil && entry.Type == "response_item" {
				switch entry.Payload.Type {
				case "function_call", "custom_tool_call":
					text := entry.Payload.Arguments
					if text == "" {
						text = entry.Payload.Input
					}
					if dagHostDagCommand.MatchString(text) {
						calls[entry.Payload.CallID] = dagHostCall{commands: dagHostDagCommand.FindAllString(text, 3)}
					}
				case "function_call_output", "custom_tool_call_output":
					call, ok := calls[entry.Payload.CallID]
					if !ok {
						break
					}
					output := dagHostOutputText(entry.Payload.Output)
					if !dagHostRefusedOutput.MatchString(output) {
						break
					}
					reason := dagHostRefusedReason.FindStringSubmatch(output)
					if reason == nil {
						break
					}
					refusals = append(refusals, dagHostRefusal{callID: entry.Payload.CallID, commands: call.commands, reason: reason[1]})
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, start, fmt.Errorf("read the rollout: %w", readErr)
		}
	}
	return refusals, resume, nil
}

// dagHostCall is the relay command a rollout's tool call carried.
type dagHostCall struct {
	commands []string
}

// dagHostOutputText is a tool result as text: a JSON string is unquoted, anything else keeps its own
// JSON form, so the refusal markers are searched in the bytes the rollout holds.
func dagHostOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

// dagHostFileSize is a rollout's size in bytes.
func dagHostFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// dagHostOffsets is the offset file's content: the resume offset of each rollout, plus the
// document's other keys verbatim, so a rewrite does not drop a key this build does not name.
type dagHostOffsets struct {
	Offsets map[string]int64
	raw     map[string]json.RawMessage
}

// dagHostLoadOffsets reads the offset file; an absent file is no offsets yet, and an unreadable one
// is an error rather than an empty set, so a corrupt file cannot silently re-report every refusal
// or clobber the offsets it holds. The document's other keys are kept verbatim.
func dagHostLoadOffsets(stateDir string) (dagHostOffsets, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, dagHostStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return dagHostOffsets{Offsets: map[string]int64{}, raw: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return dagHostOffsets{}, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return dagHostOffsets{}, fmt.Errorf("the offset file %s is not readable: %w", dagHostStateFile, err)
	}
	state := dagHostOffsets{Offsets: map[string]int64{}, raw: raw}
	if held, ok := raw["offsets"]; ok {
		if err := json.Unmarshal(held, &state.Offsets); err != nil {
			return dagHostOffsets{}, fmt.Errorf("the offset file %s holds an unreadable offsets member: %w", dagHostStateFile, err)
		}
		if state.Offsets == nil {
			state.Offsets = map[string]int64{}
		}
	}
	return state, nil
}

// dagHostSaveOffsets writes the offset file atomically: a private temporary file in the same
// directory, synced, renamed over the target, then the directory synced, so a reader never sees a
// half-written file and a crash leaves either the old offsets or the new ones. Every key the file
// already held is written back unchanged, so a key this build does not name is not lost.
func dagHostSaveOffsets(stateDir string, state dagHostOffsets) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	offsets, err := json.Marshal(state.Offsets)
	if err != nil {
		return err
	}
	document := map[string]json.RawMessage{}
	for key, value := range state.raw {
		document[key] = value
	}
	document["offsets"] = offsets
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(stateDir, dagHostStateFile+".*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	fail := func(step string, err error) error {
		file.Close()
		os.Remove(temporary)
		return fmt.Errorf("%s: %w", step, err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fail("write the offset file", err)
	}
	if err := file.Sync(); err != nil {
		return fail("sync the offset file", err)
	}
	if err := file.Close(); err != nil {
		return fail("close the offset file", err)
	}
	if err := os.Rename(temporary, filepath.Join(stateDir, dagHostStateFile)); err != nil {
		return fail("commit the offset file", err)
	}
	handle, err := os.Open(stateDir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// dagHostChild is one active relay relationship whose child thread the review reads.
type dagHostChild struct {
	relationshipID string
	issueKey       string
	threadID       string
	generation     int
}

// dagHostActiveChildren reads the relay's active relationships and the child thread each names.
func (s *dagReviewStore) dagHostActiveChildren(ctx context.Context) ([]dagHostChild, error) {
	return dagReviewRows(ctx, s.db,
		"SELECT relationship_id, issue_key, child_task_id, execution_generation FROM relationships WHERE status = 'active' ORDER BY issue_key, relationship_id",
		nil, func(rows *sql.Rows) (dagHostChild, error) {
			var child dagHostChild
			err := rows.Scan(&child.relationshipID, &child.issueKey, &child.threadID, &child.generation)
			return child, err
		})
}

// dagHostReceiptExists reports whether the relationship's generation holds an accepted receipt: a
// child event the relay counts as deliverable, the predicate its own readers use
// (internal/relay/daemon/observe.go, internal/relay/dagsched/nodestate.go). A staged receipt is the
// child's own in-turn claim, promoted only when an independent observation sees the turn end
// normally, and a suppressed one belongs to a turn that did not end normally; counting either as
// accepted would let a stale claim hide a silent child.
func (s *dagReviewStore) dagHostReceiptExists(ctx context.Context, relationshipID string, generation int) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM events WHERE relationship_id = ? AND execution_generation = ? AND producer = 'child' AND stage = 'final' AND suppressed_reason IS NULL LIMIT 1",
		relationshipID, generation).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// dagHostChildrenRead reports an active child whose newest turn ended past the receipt threshold
// with no accepted receipt in its generation: the parent waits for an event that will not come.
func dagHostChildrenRead(ctx context.Context, in *dagReviewInput, scope dagHostScope) error {
	children, err := in.store.dagHostActiveChildren(ctx)
	if err != nil {
		in.review.Checks = append(in.review.Checks, Check{
			Name: "child_receipts", State: dagReviewUnmeasured, Detail: err.Error(),
		})
		return nil
	}
	if len(children) == 0 {
		return nil
	}
	cfg := scope.config()
	for _, child := range children {
		ended, measured, err := dagHostNewestTurnEnd(ctx, cfg, child.threadID)
		if err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "child_turn:" + child.threadID, State: dagReviewUnmeasured,
				Detail: dagHostReadDetail("thread/turns/list", err),
			})
			continue
		}
		if !measured || in.now.Sub(ended) <= scope.receipt {
			continue
		}
		receipt, err := in.store.dagHostReceiptExists(ctx, child.relationshipID, child.generation)
		if err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "child_receipt:" + child.issueKey, State: dagReviewUnmeasured, Detail: err.Error(),
			})
			continue
		}
		if receipt {
			continue
		}
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagHostKindChildTurnWithoutReceipt, Issue: child.issueKey,
			Detail: fmt.Sprintf("the newest turn ended %s ago and generation %d holds no accepted receipt",
				in.now.Sub(ended).Round(time.Minute), child.generation),
		})
	}
	return nil
}

// dagHostNewestTurnEnd reads the child thread's newest turn and reports when it ended. The second
// result is false when no end can be measured: an empty thread, a turn still in progress, or a
// terminal turn whose timestamps do not give an end all raise nothing, so a silent reading is never
// reported as a silent child.
func dagHostNewestTurnEnd(ctx context.Context, cfg *Config, threadID string) (time.Time, bool, error) {
	raw, err := HostRead(ctx, cfg, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 1, "itemsView": "summary"})
	if err != nil {
		return time.Time{}, false, err
	}
	var page struct {
		Data []struct {
			ID          string `json:"id"`
			Status      string `json:"status"`
			StartedAt   any    `json:"startedAt"`
			CompletedAt any    `json:"completedAt"`
			DurationMs  any    `json:"durationMs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return time.Time{}, false, fmt.Errorf("the thread/turns/list answer is not readable: %w", err)
	}
	if len(page.Data) == 0 {
		return time.Time{}, false, nil
	}
	turn := page.Data[0]
	if turn.Status != "completed" && turn.Status != "failed" && turn.Status != "interrupted" {
		return time.Time{}, false, nil
	}
	if end, ok := dagHostEpochSeconds(turn.CompletedAt); ok {
		return time.Unix(int64(end), 0).UTC(), true, nil
	}
	started, ok := dagHostEpochSeconds(turn.StartedAt)
	if !ok {
		return time.Time{}, false, nil
	}
	duration, ok := dagHostEpochSeconds(turn.DurationMs)
	if !ok {
		return time.Time{}, false, nil
	}
	return time.Unix(int64(started+duration/1000), 0).UTC(), true, nil
}

// dagHostEpochSeconds reads a host timestamp: epoch seconds as a number or a numeric string.
func dagHostEpochSeconds(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		number, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return number, true
	case string:
		var number float64
		if err := json.Unmarshal([]byte(v), &number); err != nil {
			return 0, false
		}
		return number, true
	}
	return 0, false
}

// dagHostReadDetail is a failed host read as a check's detail: the refusal reason and the host's own
// message where it gave one.
func dagHostReadDetail(method string, err error) string {
	reason, detail := hostReadReason(err)
	if detail == "" {
		return fmt.Sprintf("%s was refused: %s", method, reason)
	}
	return fmt.Sprintf("%s was refused: %s: %s", method, reason, detail)
}
