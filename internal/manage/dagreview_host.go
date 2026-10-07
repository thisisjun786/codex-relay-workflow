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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
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

// dagHostStateFile is the offset file below the state directory: one resume offset per rollout plus
// the call ids of the refusals already reported for it, so a refusal the review reported once is not
// reported again.
const dagHostStateFile = "dag-review-state.json"

// dagHostLineLimit bounds one rollout line this review reads; a longer line is a reading it cannot
// take and is reported as unmeasured rather than skipped silently.
const dagHostLineLimit = 8 << 20

// dagHostScope is what the host readings need and the source signature does not carry: the App
// Server socket, the parent threads to read, the state directory the offsets live under, the
// receipt threshold, the plan filter the rest of the review narrows by, and whether offsets are
// used at all. DagReview binds it onto the context it runs the sources with, so one review's scope
// is its own and two reviews in one process cannot see each other's.
type dagHostScope struct {
	socket   string
	parents  map[string]string
	stateDir string
	receipt  time.Duration
	plans    []string
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
		receipt: time.Duration(receipt) * time.Minute, plans: section.Plans, noState: noState,
	})
}

// dagHostScopeOf is the scope DagReview attached to a review; the second result is false when a
// source list ran without one.
func dagHostScopeOf(ctx context.Context) (dagHostScope, bool) {
	scope, ok := ctx.Value(dagHostScopeKey{}).(dagHostScope)
	return scope, ok
}

// dagHostSection is the dag_review section as the host readings read it: the receipt threshold and
// the same plan filter the store readings narrow by.
type dagHostSection struct {
	ReceiptMinutes int      `json:"receipt_minutes"`
	Plans          []string `json:"plans"`
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

// dagHostParentsRead reads every configured parent's thread and rollout. The refusal reading needs
// the persisted offsets: with --no-state it deliberately scans from the start, and when the offset
// file cannot be read it is unmeasured rather than treated as no offsets, because scanning from the
// start there would re-report refusals an earlier check already reported.
func dagHostParentsRead(ctx context.Context, in *dagReviewInput, scope dagHostScope) error {
	cfg := scope.config()
	offsets := dagHostOffsets{Offsets: map[string]int64{}, Reported: map[string][]string{}, raw: map[string]json.RawMessage{}}
	offsetsRead, refusalsMeasured := false, false
	if scope.noState {
		refusalsMeasured = true
	} else if scope.stateDir != "" {
		state, err := dagHostLoadOffsets(scope.stateDir)
		if err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "dag_review_state", State: dagReviewUnmeasured, Detail: err.Error(),
			})
		} else {
			offsets, offsetsRead, refusalsMeasured = state, true, true
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
		dagHostParentRollout(in, parent, read.Thread.Path, &offsets, offsetsRead, refusalsMeasured)
	}
	if offsetsRead {
		if err := dagHostSaveOffsets(ctx, scope.stateDir, offsets); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			in.review.Checks = append(in.review.Checks, Check{
				Name: "dag_review_state", State: dagReviewUnmeasured,
				Detail: "the offsets could not be written: " + err.Error(),
			})
		}
	}
	return nil
}

// dagHostParentRollout reads one parent's rollout: the call ids whose tool results repeat, and the
// relay dag- refusals this check has not reported yet. The offsets map is updated with the offset
// the next check resumes from and with the call ids that offset will read again, so a refusal is
// reported once even while an earlier call of the same rollout still waits for its answer.
func dagHostParentRollout(in *dagReviewInput, parent dagHostParent, path string, offsets *dagHostOffsets, offsetsRead, refusalsMeasured bool) {
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
	if !refusalsMeasured {
		in.review.Checks = append(in.review.Checks, Check{
			Name: "parent_refusals:" + parent.id, State: dagReviewUnmeasured,
			Detail: "the rollout offsets could not be read, so the refusal reading was not taken",
		})
		return
	}
	start := int64(0)
	firstSight := false
	if offsetsRead {
		seen, ok := offsets.Offsets[path]
		if !ok {
			// A rollout this review has not seen before is read once from its start: the call whose
			// answer has not arrived is then remembered, so a refusal that arrives after this check is
			// still reported. The refusals already in its history are not.
			firstSight = true
		} else {
			start = seen
		}
	}
	reading, err := dagHostRolloutRefusals(path, start)
	if err != nil {
		in.review.Checks = append(in.review.Checks, Check{
			Name: "parent_refusals:" + parent.id, State: dagReviewUnmeasured, Detail: err.Error(),
		})
		return
	}
	if reading.tooLong {
		// A line over the limit is a reading this check could not take; the reading still continues
		// after it, so a refusal later in the rollout is reported and the offset still advances.
		in.review.Checks = append(in.review.Checks, Check{
			Name: "parent_refusals:" + parent.id, State: dagReviewUnmeasured,
			Detail: fmt.Sprintf("a rollout line over %d bytes was not read", dagHostLineLimit),
		})
	}
	reported := map[string]bool{}
	for _, callID := range offsets.Reported[path] {
		reported[callID] = true
	}
	for _, refusal := range dagHostNewRefusals(reading, reported, firstSight) {
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagHostKindParentDagRefusals, Issue: parent.label,
			Detail: fmt.Sprintf("the relay command %s was refused: %s", strings.Join(refusal.commands, ","), refusal.reason),
		})
	}
	if offsetsRead {
		offsets.Offsets[path] = reading.resume
		offsets.Reported[path] = dagHostResumeRefusalIDs(reading)
	}
}

// dagHostNewRefusals is the refusals of one reading this check has not reported yet. A rollout seen
// for the first time reports only the refusals whose OUTPUT was written after the reading began
// (the file size at that moment), so its history is not replayed while a refusal the parent appended
// during the scan still is; the call's own position does not matter, so the answer of a call that
// already existed before the reading is reported when it arrives. A known rollout reports every
// refusal the reading found that is not in the reported list. The ids the next check re-reads stay
// in that list, so a refusal is reported once.
func dagHostNewRefusals(reading dagHostRolloutReading, reported map[string]bool, firstSight bool) []dagHostRefusal {
	out := []dagHostRefusal{}
	for _, refusal := range reading.refusals {
		if reported[refusal.callID] {
			continue
		}
		if firstSight && refusal.outputStart < reading.boundary {
			continue
		}
		out = append(out, refusal)
	}
	return out
}

// dagHostResumeRefusalIDs is the call ids of the refusals a check resuming at reading.resume reads
// again: the refusals whose call line is at or after that offset. A refusal before the offset is
// never read again, so it drops out of the list and keeps the file bounded; the ids the next check
// reads are exactly the ones it must not report twice.
func dagHostResumeRefusalIDs(reading dagHostRolloutReading) []string {
	ids := []string{}
	for _, refusal := range reading.refusals {
		if refusal.callStart < reading.resume {
			continue
		}
		ids = append(ids, refusal.callID)
	}
	sort.Strings(ids)
	return ids
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

// dagHostRefusal is one relay dag- command a parent's rollout shows refused. callStart is the line
// the call started on, which is what decides whether a later check reads that call again;
// outputStart is the line the refusal's answer started on, which is what decides whether a
// first-sight reading treats the refusal as history or as one the parent appended during the scan.
type dagHostRefusal struct {
	callID      string
	commands    []string
	reason      string
	callStart   int64
	outputStart int64
}

// dagHostRolloutReading is one reading of a rollout: the refusals it shows, the offset the next
// check resumes from, the size the rollout had when the reading began, and whether a line was too
// long to read. boundary is what separates a first-sight rollout's history from a refusal the parent
// appended while this reading ran.
type dagHostRolloutReading struct {
	refusals []dagHostRefusal
	resume   int64
	boundary int64
	tooLong  bool
}

// A relay dag- invocation names the relay program and a dag- subcommand at the subcommand position;
// the review requires both, so a filename or a search term that merely contains "dag-" is not a
// relay command. The relay answers a refusal in two shapes: a command that reports its own answer as
// {"ok": false, "reason": ...}, and the scheduler's own envelope {"error": "refused", "reason": ...}
// (internal/relay/dispatch/answer.go emit). Either shape with a reason is a refusal.
var (
	dagHostDagSubcommand   = regexp.MustCompile(`^dag-[a-z-]+$`)
	dagHostVariableWord    = regexp.MustCompile(`^\$\{?[A-Za-z_][A-Za-z0-9_]*\}?$`)
	dagHostAssignmentWord  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	dagHostCommandSplitter = regexp.MustCompile(`\$\(|&&|\|\||;|\||\n`)
	dagHostRefusedAnswer   = regexp.MustCompile(`"ok"\s*:\s*false`)
	dagHostRefusedEnvelope = regexp.MustCompile(`"error"\s*:\s*"refused"`)
	dagHostRefusedReason   = regexp.MustCompile(`"reason"\s*:\s*"([a-z_]+)"`)
)

// dagHostRelaySubcommands reads the dag- subcommands of the relay invocations in a tool call's
// argument text. The text is split into simple command units (a newline, ;, &&, ||, | or $( starts
// one) and each into words; leading VAR=value assignments and exec are skipped; the program word
// must be the relay (codex-session-relay by basename, crw relay, or a variable expansion). The
// words after the program are read with the relay's own root parser, so an option's value is not
// mistaken for the subcommand: --state, --socket and --kind-module take a value (--flag=value too)
// and --json takes none. The subcommand is the parser's first remaining word, and the call is a dag
// call only when the parser read the line and that word matches ^dag-[a-z-]+$; a line the root
// parser refuses (an unknown option, a missing value) is not a dag call, and reading a relay
// document with cat or searching for a dag- word with rg is never a relay invocation.
func dagHostRelaySubcommands(text string) []string {
	var subcommands []string
	for _, unit := range dagHostCommandSplitter.Split(text, -1) {
		words := strings.Fields(unit)
		for len(words) > 0 && (dagHostAssignmentWord.MatchString(words[0]) || words[0] == "exec") {
			words = words[1:]
		}
		if len(words) == 0 || !dagHostRelayProgram(words) {
			continue
		}
		if path.Base(words[0]) == "crw" {
			words = words[2:]
		} else {
			words = words[1:]
		}
		// The relay's root parser knows which options take a value, so a dag- word given as an
		// option's value is not read as the subcommand, and a line the parser refuses is not a dag
		// call. Remaining is what the parser did not consume: for the root, the subcommand and the
		// words after it.
		parsed := argparse.Parse("", words)
		if parsed.Message != "" || len(parsed.Remaining) == 0 {
			continue
		}
		if dagHostDagSubcommand.MatchString(parsed.Remaining[0]) {
			subcommands = append(subcommands, parsed.Remaining[0])
		}
	}
	return subcommands
}

// dagHostRelayProgram reports whether the first word names the relay: codex-session-relay by
// basename, crw (or its absolute path) followed by relay, or a variable expansion a shell would
// substitute. The absolute form is the relay's own recovery command, which names this binary's
// resolved executable followed by relay (internal/relay/cli/status.go relayProgram).
func dagHostRelayProgram(words []string) bool {
	switch {
	case path.Base(words[0]) == "codex-session-relay":
		return true
	case path.Base(words[0]) == "crw":
		return len(words) > 1 && words[1] == "relay"
	case dagHostVariableWord.MatchString(words[0]):
		return true
	}
	return false
}

// dagHostCallText is a tool call's command line: a function_call's arguments is a JSON object whose
// cmd member carries it; any other text (a custom_tool_call's input, or a rollout that stores the
// command line directly) is used as it stands.
func dagHostCallText(arguments, input string) string {
	if arguments != "" {
		var parsed struct {
			Cmd string `json:"cmd"`
		}
		if err := json.Unmarshal([]byte(arguments), &parsed); err == nil && parsed.Cmd != "" {
			return parsed.Cmd
		}
		return arguments
	}
	return input
}

// dagHostAfterBoundary is a test seam: it is called after a reading has taken the rollout's size as
// its boundary and before it scans, so a test can append the answer of a call that already existed
// and check the boundary rule against the real reading rather than against a reading built by hand.
// It is nil outside tests.
var dagHostAfterBoundary func(path string)

// dagHostRolloutRefusals reads the rollout from start to its end and reports the relay dag-
// refusals it finds, with the offset the next check resumes from. The resume offset never advances
// past a relay call whose output has not been seen, so a call and its output split across two checks
// are still paired; a rollout shorter than the saved offset was replaced, so it is read from its
// start rather than skipped. A reading that cannot be taken is an error, never a partial answer.
func dagHostRolloutRefusals(path string, start int64) (dagHostRolloutReading, error) {
	f, err := os.Open(path)
	if err != nil {
		return dagHostRolloutReading{resume: start}, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return dagHostRolloutReading{resume: start}, err
	}
	if start > size {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return dagHostRolloutReading{resume: start}, err
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	// boundary is the size the rollout had when this reading began. The caller uses it to tell a
	// first-sight rollout's history from a refusal the parent appended while this reading ran.
	reading := dagHostRolloutReading{resume: start, boundary: size}
	if dagHostAfterBoundary != nil {
		dagHostAfterBoundary(path)
	}
	offset, resume := start, start
	calls := map[string]dagHostCall{}
	var refusals []dagHostRefusal
	for {
		lineStart := offset
		line, consumed, tooLong, readErr := dagHostReadLine(reader, dagHostLineLimit)
		if consumed > 0 {
			offset += consumed
			resume = lineStart
			if tooLong {
				// A line over the limit is a reading this check cannot take. It is not parsed, but the
				// reading continues after it, so the lines that follow are still read and the offset
				// still advances; the caller reports the unmeasured line.
				reading.tooLong = true
				continue
			}
			var entry dagHostRolloutEntry
			if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &entry) == nil && entry.Type == "response_item" {
				switch entry.Payload.Type {
				case "function_call", "custom_tool_call":
					if commands := dagHostRelaySubcommands(dagHostCallText(entry.Payload.Arguments, entry.Payload.Input)); len(commands) > 0 {
						calls[entry.Payload.CallID] = dagHostCall{commands: commands, callStart: lineStart}
					}
				case "function_call_output", "custom_tool_call_output":
					call, ok := calls[entry.Payload.CallID]
					if !ok {
						break
					}
					call.seen = true
					calls[entry.Payload.CallID] = call
					output := dagHostOutputText(entry.Payload.Output)
					if !dagHostRefusedAnswer.MatchString(output) && !dagHostRefusedEnvelope.MatchString(output) {
						break
					}
					// The body asks for an output carrying "ok": false or a refused reason, so either
					// marker reports; the reason names the refusal when the output carries one.
					reason := "the command was refused"
					if found := dagHostRefusedReason.FindStringSubmatch(output); found != nil {
						reason = found[1]
					}
					refusals = append(refusals, dagHostRefusal{callID: entry.Payload.CallID, commands: call.commands, reason: reason, callStart: call.callStart, outputStart: lineStart})
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return dagHostRolloutReading{resume: start}, fmt.Errorf("read the rollout: %w", readErr)
		}
	}
	// A relay call whose output has not arrived stays pending: the next check re-reads it from its
	// own line, so the call and its output are paired even when they are written in different runs.
	for _, call := range calls {
		if !call.seen && call.callStart < resume {
			resume = call.callStart
		}
	}
	reading.refusals = refusals
	reading.resume = resume
	return reading, nil
}

// dagHostReadLine reads one line bounded by limit bytes. The second result is the line's length in
// bytes, so a caller that cannot use an over-long line still advances past the whole of it rather
// than leaving its tail to be read as a line of its own. The third result is true when the line
// exceeds the limit, in which case the returned bytes are its first limit bytes and the rest is
// discarded: the caller reports an unmeasured reading rather than growing with it.
func dagHostReadLine(reader *bufio.Reader, limit int) ([]byte, int64, bool, error) {
	var line []byte
	var consumed int64
	over := false
	for {
		chunk, err := reader.ReadSlice('\n')
		consumed += int64(len(chunk))
		if !over {
			line = append(line, chunk...)
			if len(line) > limit {
				line = line[:limit]
				over = true
			}
		}
		if err == nil {
			return line, consumed, over, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, consumed, over, err
	}
}

// dagHostCall is the relay command a rollout's tool call carried, with the line it started on and
// whether its output has been seen.
type dagHostCall struct {
	commands  []string
	callStart int64
	seen      bool
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

// dagHostOffsets is the offset file's content: the resume offset of each rollout and the call ids
// of the refusals already reported for it, plus the document's other keys verbatim, so a rewrite
// does not drop a key this build does not name. A file written before this build holds no
// "reported" member and reads as an empty list.
type dagHostOffsets struct {
	Offsets  map[string]int64
	Reported map[string][]string
	raw      map[string]json.RawMessage
}

// dagHostLoadOffsets reads the offset file; an absent file is no offsets yet, and an unreadable one
// is an error rather than an empty set, so a corrupt file cannot silently re-report every refusal
// or clobber the offsets it holds. The document's other keys are kept verbatim. A missing
// "reported" member is an empty list; one this build cannot read is an error, so a file this build
// cannot fully understand never becomes a reason to re-report a refusal.
func dagHostLoadOffsets(stateDir string) (dagHostOffsets, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, dagHostStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return dagHostOffsets{Offsets: map[string]int64{}, Reported: map[string][]string{}, raw: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return dagHostOffsets{}, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return dagHostOffsets{}, fmt.Errorf("the offset file %s is not readable: %w", dagHostStateFile, err)
	}
	state := dagHostOffsets{Offsets: map[string]int64{}, Reported: map[string][]string{}, raw: raw}
	if held, ok := raw["offsets"]; ok {
		if err := json.Unmarshal(held, &state.Offsets); err != nil {
			return dagHostOffsets{}, fmt.Errorf("the offset file %s holds an unreadable offsets member: %w", dagHostStateFile, err)
		}
		if state.Offsets == nil {
			state.Offsets = map[string]int64{}
		}
	}
	if held, ok := raw["reported"]; ok {
		if err := json.Unmarshal(held, &state.Reported); err != nil {
			return dagHostOffsets{}, fmt.Errorf("the offset file %s holds an unreadable reported member: %w", dagHostStateFile, err)
		}
		if state.Reported == nil {
			state.Reported = map[string][]string{}
		}
	}
	return state, nil
}

// dagHostSaveOffsets writes the offset file atomically: a private temporary file in the same
// directory, synced, renamed over the target, then the directory synced, so a reader never sees a
// half-written file and a crash leaves either the old offsets or the new ones. Every key the file
// already held is written back unchanged, so a key this build does not name is not lost. A context
// cancelled while the file was being prepared is rechecked immediately before the rename, so a
// cancelled review commits no durable effect.
func dagHostSaveOffsets(ctx context.Context, stateDir string, state dagHostOffsets) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	offsets, err := json.Marshal(state.Offsets)
	if err != nil {
		return err
	}
	reported, err := json.Marshal(state.Reported)
	if err != nil {
		return err
	}
	document := map[string]json.RawMessage{}
	for key, value := range state.raw {
		document[key] = value
	}
	document["offsets"] = offsets
	document["reported"] = reported
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
	if err := ctx.Err(); err != nil {
		os.Remove(temporary)
		return err
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
// with no accepted receipt in its generation: the parent waits for an event that will not come. When
// the review is narrowed to plans, a child is read only when one of those plans released its
// relationship, which is the same narrowing the store readings take.
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
	released := map[string]bool{}
	for _, facts := range in.facts {
		for _, execution := range facts.executions {
			released[execution.relationshipID] = true
		}
	}
	cfg := scope.config()
	for _, child := range children {
		if len(scope.plans) > 0 && !released[child.relationshipID] {
			continue
		}
		reading, err := dagHostNewestTurnEnd(ctx, cfg, child.threadID)
		if err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "child_turn:" + child.threadID, State: dagReviewUnmeasured,
				Detail: dagHostReadDetail("thread/turns/list", err),
			})
			continue
		}
		if reading.detail != "" {
			// A turn that has ended but whose end this reading cannot measure is not a silent child
			// and not an anomaly: it is a reading this review could not take.
			in.review.Checks = append(in.review.Checks, Check{
				Name: "child_turn:" + child.threadID, State: dagReviewUnmeasured, Detail: reading.detail,
			})
			continue
		}
		if !reading.measured || in.now.Sub(reading.ended) <= scope.receipt {
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
				in.now.Sub(reading.ended).Round(time.Minute), child.generation),
		})
	}
	return nil
}

// dagHostTurnReading is one reading of a child's newest turn. measured is false when the turn has
// no end this reading can use, and detail is set only when the turn HAS ended and the end could not
// be measured, which is the one case the caller reports as an unmeasured check rather than passing
// over in silence.
type dagHostTurnReading struct {
	ended    time.Time
	measured bool
	detail   string
}

// dagHostNewestTurnEnd reads the child thread's newest turn and reports when it ended. An empty
// thread and a turn still in progress raise nothing. A terminal turn whose timestamps do not give an
// end is unmeasured with the state named, so a reading the review could not take is never mistaken
// for a silent child.
func dagHostNewestTurnEnd(ctx context.Context, cfg *Config, threadID string) (dagHostTurnReading, error) {
	raw, err := HostRead(ctx, cfg, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 1, "itemsView": "summary"})
	if err != nil {
		return dagHostTurnReading{}, err
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
		return dagHostTurnReading{}, fmt.Errorf("the thread/turns/list answer is not readable: %w", err)
	}
	if len(page.Data) == 0 {
		return dagHostTurnReading{}, nil
	}
	turn := page.Data[0]
	if turn.Status != "completed" && turn.Status != "failed" && turn.Status != "interrupted" {
		return dagHostTurnReading{}, nil
	}
	if end, ok := dagHostEpochSeconds(turn.CompletedAt); ok {
		return dagHostTurnReading{ended: time.Unix(int64(end), 0).UTC(), measured: true}, nil
	}
	started, ok := dagHostEpochSeconds(turn.StartedAt)
	if !ok {
		return dagHostTurnReading{detail: fmt.Sprintf("the newest turn is %s and carries no readable startedAt, so its end cannot be measured", turn.Status)}, nil
	}
	duration, ok := dagHostEpochSeconds(turn.DurationMs)
	if !ok {
		return dagHostTurnReading{detail: fmt.Sprintf("the newest turn is %s, carries no completedAt, and its durationMs is not readable, so its end cannot be measured", turn.Status)}, nil
	}
	return dagHostTurnReading{ended: time.Unix(int64(started+duration/1000), 0).UTC(), measured: true}, nil
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
