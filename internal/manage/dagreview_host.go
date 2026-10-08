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

	"golang.org/x/sys/unix"
	"mvdan.cc/sh/v3/syntax"

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

// dagHostStateLockFile is the lock beside the offset file. A review holds it across the whole read
// and the write of the offsets, so two reviews of the same state directory cannot both read the
// reported list before either writes it, report the same refusal, and each save a list that misses
// what the other reported. It is a separate file so the offset file's own atomic replacement never
// swaps the locked inode out from under a holder.
const dagHostStateLockFile = "dag-review-state.lock"

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
	offsets := dagHostOffsets{Offsets: map[string]int64{}, Reported: map[string][]string{}, Unparsed: map[string][]string{}, raw: map[string]json.RawMessage{}}
	offsetsRead, refusalsMeasured := false, false
	if scope.noState {
		refusalsMeasured = true
	} else if scope.stateDir != "" {
		// The lock is held from before the offsets are read until after they are written, so a second
		// review of the same state directory waits here and reads the list the first one saved rather
		// than the one it read for itself. A lock that cannot be taken is a reading this check could
		// not take: it reports unmeasured instead of reporting refusals it cannot keep track of.
		release, err := dagHostStateLock(ctx, scope.stateDir)
		if err != nil {
			in.review.Checks = append(in.review.Checks, Check{
				Name: "dag_review_state", State: dagReviewUnmeasured, Detail: err.Error(),
			})
		} else {
			defer release()
			state, err := dagHostLoadOffsets(scope.stateDir)
			if err != nil {
				in.review.Checks = append(in.review.Checks, Check{
					Name: "dag_review_state", State: dagReviewUnmeasured, Detail: err.Error(),
				})
			} else {
				offsets, offsetsRead, refusalsMeasured = state, true, true
			}
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
	unparsedReported := map[string]bool{}
	for _, callID := range offsets.Unparsed[path] {
		unparsedReported[callID] = true
	}
	for _, line := range dagHostNewUnparsed(reading, unparsedReported) {
		in.review.Checks = append(in.review.Checks, Check{
			Name: "parent_command:" + parent.id, State: dagReviewUnmeasured,
			Detail: fmt.Sprintf("command_unparsed: the command line of call %s is not valid shell, so its relay calls were not read", line.callID),
		})
	}
	if offsetsRead {
		offsets.Offsets[path] = reading.resume
		offsets.Reported[path] = dagHostResumeRefusalIDs(reading)
		offsets.Unparsed[path] = dagHostResumeUnparsedIDs(reading)
	}
}

// dagHostNewRefusals is the refusals of one reading this check has not reported yet. A rollout seen
// for the first time reports only the refusals whose OUTPUT line was complete before the reading
// began (the file size at that moment), so its history is not replayed while a refusal the parent
// appended during the scan still is. An output line that was still being written when the reading
// began is not history: it is reported, because the refusal it carries arrived with this reading.
// The call's own position does not matter, so the answer of a call that already existed before the
// reading is reported when it arrives. A known rollout reports every refusal the reading found that
// is not in the reported list. The ids the next check re-reads stay in that list, so a refusal is
// reported once. One reading reports a call id once whatever the rollout holds for it: a repeated
// answer is the duplicate tool outputs reading's finding, and a second refusal anomaly for the same
// command would be a second report of one refusal.
func dagHostNewRefusals(reading dagHostRolloutReading, reported map[string]bool, firstSight bool) []dagHostRefusal {
	out := []dagHostRefusal{}
	for _, refusal := range reading.refusals {
		if reported[refusal.callID] {
			continue
		}
		// The id is recorded whatever the refusal's verdict: a refusal this reading treats as history
		// is one this reading has seen, so a duplicate of its answer appended during the same scan is
		// not a new refusal. The duplicate-outputs reading is the one that reports a repeated answer.
		reported[refusal.callID] = true
		if firstSight && refusal.outputEnd <= reading.boundary {
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

// dagHostResumeUnparsedIDs is the call ids of the refused command lines a check resuming at
// reading.resume reads again. They are kept in their own list, so a refusal and an unparsed line
// with the same call id never share one reported entry.
func dagHostResumeUnparsedIDs(reading dagHostRolloutReading) []string {
	ids := []string{}
	for _, line := range reading.unparsed {
		if line.callStart < reading.resume {
			continue
		}
		ids = append(ids, line.callID)
	}
	sort.Strings(ids)
	return ids
}

// dagHostNewUnparsed is the command lines of one reading that this check has not reported yet. A
// refused command line is unmeasured wherever it stands, so a first-sight rollout reports its history
// too, unlike a refusal. The lines are kept in their own reported list, so a
// line the next check reads again is not reported twice.
func dagHostNewUnparsed(reading dagHostRolloutReading, reported map[string]bool) []dagHostUnparsed {
	out := []dagHostUnparsed{}
	for _, line := range reading.unparsed {
		if reported[line.callID] {
			continue
		}
		reported[line.callID] = true
		out = append(out, line)
	}
	return out
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
	outputEnd   int64
}

// dagHostRolloutReading is one reading of a rollout: the refusals it shows, the offset the next
// check resumes from, the size the rollout had when the reading began, and whether a line was too
// long to read. boundary is what separates a first-sight rollout's history from a refusal the parent
// appended while this reading ran.
type dagHostRolloutReading struct {
	refusals []dagHostRefusal
	unparsed []dagHostUnparsed
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
	dagHostDagSubcommand = regexp.MustCompile("^dag-[a-z-]+$")
	// dagHostSubstitutionWord stands for a word part the reading does not expand (a command
	// substitution, a parameter expansion, arithmetic). A word holding one is never a program, a
	// subcommand or a dag- word.
	dagHostSubstitutionWord = "<substitution>"
	dagHostRefusedAnswer    = regexp.MustCompile(`"ok"\s*:\s*false`)
	dagHostRefusedEnvelope  = regexp.MustCompile(`"error"\s*:\s*"refused"`)
	dagHostRefusedReason    = regexp.MustCompile(`"reason"\s*:\s*"([a-z_]+)"`)
)

// dagHostUnparsed is a shell command line of a tool call that the parser refused. The reading does not
// guess whether it held a relay call; it leaves the line unmeasured. callStart is the
// offset of the call's line.
type dagHostUnparsed struct {
	callID    string
	callStart int64
}

// dagHostWord is one word of a simple command as the shell reads it. text is the word with its quotes
// removed; static is false when a part of it is expanded at run time, and then text holds the
// placeholder for that part; variable is true when the whole word is one bare parameter expansion,
// such as $RELAY, ${RELAY} or "$RELAY".
type dagHostWord struct {
	text     string
	static   bool
	variable bool
}

// dagHostRelaySubcommands reads the dag- subcommands of the relay invocations in a command line. The
// command line is parsed with mvdan.cc/sh/v3/syntax, and every simple command in the tree is judged,
// so a call in a substitution (including one in an expanded here-document), a pipeline, a list, a
// compound command or a function body is found, while a quoted string, a comment, a quoted
// here-document body or a document read is not. A simple command is a relay invocation when its
// program word is codex-session-relay (by basename), crw followed by relay, or a bare parameter
// expansion such as $RELAY. The subcommand is the first word the relay's root parser does not consume,
// and the call is a dag call only when that word matches ^dag-[a-z-]+$. A command line the parser
// refuses returns its error, and the reading never guesses at it.
func dagHostRelaySubcommands(command string) ([]string, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, err
	}
	var subcommands []string
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			if subcommand, found := dagHostRelaySubcommand(call.Args); found {
				subcommands = append(subcommands, subcommand)
			}
		}
		return true
	})
	return subcommands, nil
}

// dagHostRelaySubcommand is the dag- subcommand of one simple command's words, when the command is a
// relay invocation of one.
func dagHostRelaySubcommand(args []*syntax.Word) (string, bool) {
	words := make([]dagHostWord, 0, len(args))
	for _, arg := range args {
		words = append(words, dagHostWordOf(arg))
	}
	// exec runs the command that follows it in place of the shell, so it names the same program.
	if len(words) > 0 && words[0].static && words[0].text == "exec" {
		words = words[1:]
	}
	if len(words) == 0 {
		return "", false
	}
	var rest []dagHostWord
	switch {
	case words[0].variable:
		rest = words[1:]
	case words[0].static && path.Base(words[0].text) == "codex-session-relay":
		rest = words[1:]
	case words[0].static && path.Base(words[0].text) == "crw" && len(words) > 1 && words[1].static && words[1].text == "relay":
		rest = words[2:]
	default:
		return "", false
	}
	// The relay's root parser knows which options take a value, so a dag- word given as an option's
	// value is not read as the subcommand, and a line the parser refuses is not a dag call.
	texts := make([]string, len(rest))
	for i, word := range rest {
		texts[i] = word.text
	}
	parsed := argparse.Parse("", texts)
	if parsed.Message != "" || len(parsed.Remaining) == 0 {
		return "", false
	}
	if dagHostDagSubcommand.MatchString(parsed.Remaining[0]) {
		return parsed.Remaining[0], true
	}
	return "", false
}

// dagHostWordOf reads one word of a simple command.
func dagHostWordOf(word *syntax.Word) dagHostWord {
	var out strings.Builder
	static := true
	for _, part := range word.Parts {
		dagHostPartText(&out, &static, part, false)
	}
	return dagHostWord{text: out.String(), static: static, variable: dagHostBareExpansion(word)}
}

// dagHostPartText writes the text one part of a word reads as. quoted reports whether the part sits
// inside double quotes, where a backslash escapes fewer characters.
func dagHostPartText(out *strings.Builder, static *bool, part syntax.WordPart, quoted bool) {
	switch p := part.(type) {
	case *syntax.Lit:
		out.WriteString(dagHostUnescape(p.Value, quoted))
	case *syntax.SglQuoted:
		if p.Dollar {
			out.WriteString(dagHostANSIText(p.Value))
			return
		}
		out.WriteString(p.Value)
	case *syntax.DblQuoted:
		if p.Dollar {
			*static = false
			out.WriteString(dagHostSubstitutionWord)
			return
		}
		for _, inner := range p.Parts {
			dagHostPartText(out, static, inner, true)
		}
	default:
		*static = false
		out.WriteString(dagHostSubstitutionWord)
	}
}

// dagHostUnescape removes the escapes of a literal part. An unquoted backslash quotes the character
// after it; inside double quotes a backslash quotes only $, backtick, double quote, backslash and a
// newline, and keeps itself before anything else. A backslash that ends a line continues it, so both
// are removed.
func dagHostUnescape(raw string, quoted bool) string {
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '\\' || i+1 >= len(raw) {
			out.WriteByte(c)
			continue
		}
		next := raw[i+1]
		if quoted && !strings.ContainsRune("$`\"\\\n", rune(next)) {
			out.WriteByte(c)
			continue
		}
		i++
		if next != '\n' {
			out.WriteByte(next)
		}
	}
	return out.String()
}

// dagHostANSIText is the text of an ANSI-C quoted string ($'...'): the escapes bash decodes are
// decoded, and an escape bash keeps as it is stays as it is. A relay call's program word may be
// written this way, so the reading names it like a plain word.
func dagHostANSIText(raw string) string {
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			out.WriteByte(raw[i])
			continue
		}
		i++
		switch c := raw[i]; c {
		case 'a':
			out.WriteByte('\a')
		case 'b':
			out.WriteByte('\b')
		case 'e', 'E':
			out.WriteByte(0x1b)
		case 'f':
			out.WriteByte('\f')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'v':
			out.WriteByte('\v')
		case '\\', '\'', '"', '?':
			out.WriteByte(c)
		case 'c':
			if i+1 < len(raw) {
				i++
				out.WriteByte(raw[i] & 0x1f)
			} else {
				out.WriteString("\\c")
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			value := 0
			for digits := 0; digits < 3 && i < len(raw) && raw[i] >= '0' && raw[i] <= '7'; digits++ {
				value = value*8 + int(raw[i]-'0')
				i++
			}
			out.WriteByte(byte(value))
			i--
		case 'x', 'u', 'U':
			width := 2
			if c == 'u' {
				width = 4
			} else if c == 'U' {
				width = 8
			}
			value, digits := 0, 0
			for digits < width && i+1+digits < len(raw) {
				digit, ok := dagHostHexValue(raw[i+1+digits])
				if !ok {
					break
				}
				value = value*16 + digit
				digits++
			}
			if digits == 0 {
				out.WriteByte('\\')
				out.WriteByte(c)
				continue
			}
			i += digits
			if c == 'x' {
				out.WriteByte(byte(value))
			} else {
				out.WriteRune(rune(value))
			}
		default:
			out.WriteByte('\\')
			out.WriteByte(c)
		}
	}
	return out.String()
}

// dagHostHexValue is the value of one hexadecimal digit.
func dagHostHexValue(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// dagHostBareExpansion reports whether a word is one parameter expansion with no operator, bare or
// inside double quotes: $RELAY, ${RELAY} or "$RELAY". Only a plain name is read as a program.
func dagHostBareExpansion(word *syntax.Word) bool {
	parts := word.Parts
	if len(parts) == 1 {
		if quoted, ok := parts[0].(*syntax.DblQuoted); ok && !quoted.Dollar {
			parts = quoted.Parts
		}
	}
	if len(parts) != 1 {
		return false
	}
	p, ok := parts[0].(*syntax.ParamExp)
	if !ok {
		return false
	}
	return p.Exp == nil && p.Index == nil && p.Slice == nil && p.Repl == nil && p.Names == 0 &&
		!p.Excl && !p.Length && !p.Width && !p.IsSet && p.Flags == nil
}

// dagHostCallText is a tool call's command line, and whether a line the parser refuses is reported as
// unmeasured. A function_call's arguments is a JSON object whose cmd member carries the command line;
// a JSON object with no cmd is another tool's arguments and is not a command line at all. Arguments
// that are not a JSON object are read as the command line itself, as a rollout that stores the command
// directly does. A custom_tool_call's input is the code of whatever tool it calls (an exec cell, a
// patch), so it is judged for relay calls but its refusal is not reported.
func dagHostCallText(kind, arguments, input string) (string, bool) {
	if kind == "custom_tool_call" {
		return input, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &object) != nil {
		return arguments, true
	}
	var parsed struct {
		Cmd string `json:"cmd"`
	}
	if json.Unmarshal([]byte(arguments), &parsed) != nil || parsed.Cmd == "" {
		return "", false
	}
	return parsed.Cmd, true
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
			// The resume offset advances past a line only when the line ended with a newline: a
			// trailing line without one is still being written, so the next check reads it again from
			// its own start. A reading with nothing left pending resumes at the end of the file.
			if readErr == nil {
				resume = offset
			} else {
				resume = lineStart
			}
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
					command, reportable := dagHostCallText(entry.Payload.Type, entry.Payload.Arguments, entry.Payload.Input)
					commands, err := dagHostRelaySubcommands(command)
					if err != nil {
						if reportable {
							reading.unparsed = append(reading.unparsed, dagHostUnparsed{callID: entry.Payload.CallID, callStart: lineStart})
						}
						break
					}
					if len(commands) > 0 {
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
					refusals = append(refusals, dagHostRefusal{callID: entry.Payload.CallID, commands: call.commands, reason: reason, callStart: call.callStart, outputStart: lineStart, outputEnd: offset})
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
	Unparsed map[string][]string
	raw      map[string]json.RawMessage
}

// dagHostStateLock takes the state directory's review lock, so one review's read of the offsets, its
// scan and its write of the offsets are one span no second review can interleave with: without it
// two reviews could both read a reported list that lacks a refusal, both report it, and each save a
// list missing what the other reported. A lock already held is a reading this check cannot take, so
// the caller leaves the refusal reading unmeasured rather than reporting a refusal it cannot track.
// The kernel releases the lock when the process ends, so a crash cannot leave it held.
func dagHostStateLock(ctx context.Context, stateDir string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, dagHostStateLockFile)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("the review state lock is a symlink; refusing to lock through it")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("another review holds the review state lock, so the refusal reading was not taken")
		}
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

// dagHostLoadOffsets reads the offset file; an absent file is no offsets yet, and an unreadable one
// is an error rather than an empty set, so a corrupt file cannot silently re-report every refusal
// or clobber the offsets it holds. The document's other keys are kept verbatim. A missing
// "reported" member is an empty list; one this build cannot read is an error, so a file this build
// cannot fully understand never becomes a reason to re-report a refusal.
func dagHostLoadOffsets(stateDir string) (dagHostOffsets, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, dagHostStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return dagHostOffsets{Offsets: map[string]int64{}, Reported: map[string][]string{}, Unparsed: map[string][]string{}, raw: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return dagHostOffsets{}, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return dagHostOffsets{}, fmt.Errorf("the offset file %s is not readable: %w", dagHostStateFile, err)
	}
	state := dagHostOffsets{Offsets: map[string]int64{}, Reported: map[string][]string{}, Unparsed: map[string][]string{}, raw: raw}
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
	if held, ok := raw["reported_unparsed"]; ok {
		if err := json.Unmarshal(held, &state.Unparsed); err != nil {
			return dagHostOffsets{}, fmt.Errorf("the offset file %s holds an unreadable reported_unparsed member: %w", dagHostStateFile, err)
		}
		if state.Unparsed == nil {
			state.Unparsed = map[string][]string{}
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
	unparsed, err := json.Marshal(state.Unparsed)
	if err != nil {
		return err
	}
	document := map[string]json.RawMessage{}
	for key, value := range state.raw {
		document[key] = value
	}
	document["offsets"] = offsets
	document["reported"] = reported
	document["reported_unparsed"] = unparsed
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
