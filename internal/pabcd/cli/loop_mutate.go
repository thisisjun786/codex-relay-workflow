// Loop CLI mutating verbs: goalplan-cli.ts at v0.2.40 (commit 3c1459ac), the write half.
//
// CRW-383 ports runSteer (:296-352), runAddOp (:354-420), runDecision (:525-560) and runLifecycle
// (:562-699), the verbs steer, add-criterion, add-work-phase, add-task, complete-task, meet-criterion, ask
// and decide. Every verb is a thin driver over the goalplan package: the steering transaction, the
// lifecycle transitions and the write lock are the ones B14 and B15 ported, and this file adds only the
// argument checks, the idempotency key of the add verbs and the answer texts of the oracle.
//
// Like the read verbs, a mutating verb is dispatched after the structural parse, and a library error is the
// oracle's uncaught throw. The checks that come before any state is touched keep the oracle's order and
// text; the oracle's own session checks run here, not the generic guard of the read verbs, because their
// texts differ per verb.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// loopIsMutatingVerb reports the verbs this file owns.
func loopIsMutatingVerb(verb LoopVerb) bool {
	switch verb {
	case LoopVerbSteer, LoopVerbAddCriterion, LoopVerbAddWorkPhase, LoopVerbAddTask,
		LoopVerbCompleteTask, LoopVerbMeetCriterion, LoopVerbAsk, LoopVerbDecide:
		return true
	}
	return false
}

// loopRunMutating dispatches one mutating verb (:810-826).
func loopRunMutating(ctx context.Context, args LoopCliArgs) (LoopCliResult, error) {
	switch args.Verb {
	case LoopVerbSteer:
		return loopSteer(ctx, args, nil, nil)
	case LoopVerbAsk, LoopVerbDecide:
		return loopDecision(args)
	case LoopVerbAddCriterion, LoopVerbAddWorkPhase:
		return loopAddOp(args)
	}
	return loopLifecycle(args)
}

// loopBoundSlug is the session's bound slug, "" when the session binds none (readState(...).slug).
func loopBoundSlug(args LoopCliArgs, session string) string {
	return state.ReadState(args.Cwd, session).Slug
}

// loopNotBound is the refusal for a session that binds no goalplan. steer and ask/decide and the rest use the
// same sentence; the dash is the oracle's (steer alone spells it with an em dash).
func loopNotBound(verb LoopVerb, session, dash string) LoopCliResult {
	return LoopCliResult{Output: fmt.Sprintf("loop %s: session '%s' has no bound goalplan %s run `crw pabcd loop init --session %s` first",
		verb, session, dash, session), Code: 1}
}

// loopNodeReadMessage is Error.message of readFileSync: the errno spelling, with no path for a failed read of an
// opened directory.
func loopNodeReadMessage(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "read" {
		return strings.TrimSuffix(nodeErrorMessage(err), " '"+pathErr.Path+"'")
	}
	return nodeErrorMessage(err)
}

// loopParseJSON is JSON.parse: one document and nothing after it. Numbers stay json.Number so a value no
// float64 holds does not cost the document. The error text is Go's, not V8's (docs/port-cxc/known-defects/CRW-383.md).
func loopParseJSON(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected non-whitespace character after JSON at position %d", decoder.InputOffset())
	}
	return value, nil
}

// loopLossyJSON names what the decoder would replace by U+FFFD in a JSON text that parsed, "" when it replaces
// nothing. Two idempotency keys, or two scenarios, that differ only in such a character would decode equal and the
// second batch would be dropped as a duplicate, so the goalplan reader's refusal of the same loss applies here as
// well. The oracle's JSON.parse has the loss; the verb is stricter (docs/port-cxc/known-defects/CRW-383.md).
func loopLossyJSON(raw string) string {
	if !utf8.ValidString(raw) {
		for i := 0; i < len(raw); {
			r, n := utf8.DecodeRuneInString(raw[i:])
			if r == utf8.RuneError && n == 1 {
				return fmt.Sprintf("a byte that is not UTF-8 at byte %d that would lose stored text", i)
			}
			i += n
		}
	}
	if at := goalplan.UnpairedJSONSurrogate(raw); at >= 0 {
		return fmt.Sprintf("an unpaired JSON surrogate at byte %d that would lose stored text", at)
	}
	return ""
}

// loopReadBatch reads the batch file. A context that can end reads it in a goroutine, so a path that blocks
// (a pipe whose writer has not finished, as --batch-json /proc/self/fd/0 on an open stdin does) does not hold
// the verb past the first SIGINT, which the oracle's process dies at (CRW-1074). The read that outlives an
// ended context is abandoned with the process; nothing it returns is used.
func loopReadBatch(ctx context.Context, path string) ([]byte, error) {
	if ctx.Done() == nil {
		return os.ReadFile(path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type read struct {
		bytes []byte
		err   error
	}
	done := make(chan read, 1)
	go func() {
		bytes, err := os.ReadFile(path)
		done <- read{bytes, err}
	}()
	select {
	case r := <-done:
		// A line that arrived just before the signal is not applied either.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.bytes, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// loopSteer is runSteer (:296-352): the plan is the one the session is bound to, the batch is inline JSON or a
// file, and the steering transaction answers.
//
// The first SIGINT ends it (CRW-1074, the CRW-871 rule): the batch read, the goalplan lock wait and the check
// made with the lock held and immediately before the transaction's first write all observe ctx, and a steer
// they end returns the context's own error with nothing written. A steer whose transaction has begun to write
// finishes and answers as before. lock is the test seam of the goalplan lock (its Now runs right after the lock
// is taken); nil means the real lock. beforeWrite is the test seam of the plan write (it runs as that write
// begins); nil means none.
func loopSteer(ctx context.Context, args LoopCliArgs, lock *goalplan.GoalplanWriteLockOptions, beforeWrite func()) (LoopCliResult, error) {
	begun := false
	result, err := loopSteerRun(ctx, args, lock, func() {
		begun = true
		if beforeWrite != nil {
			beforeWrite()
		}
	})
	// Every answer reached before the transaction's first write began (a refusal of the arguments or the batch,
	// an unbound session, a duplicate, a locked, unusable or rejected plan, and an error of the lock or the plan
	// path) is the answer of a process the signal would already have ended: an ended context takes precedence and
	// nothing is printed. Once the write has begun, the answer stands, error or not.
	if !begun {
		if cerr := ctx.Err(); cerr != nil {
			return LoopCliResult{}, cerr
		}
	}
	return result, err
}

// loopSteerRun is loopSteer's body; began runs as the steering transaction's plan write begins.
func loopSteerRun(ctx context.Context, args LoopCliArgs, lock *goalplan.GoalplanWriteLockOptions, began func()) (LoopCliResult, error) {
	session := loopSessionID(args)
	if session == "" {
		return LoopCliResult{Output: "loop steer: --session <id> is required", Code: 1}, nil
	}
	if !state.IsCanonicalSessionID(session) {
		return LoopCliResult{Output: fmt.Sprintf("loop steer: --session \"%s\" is not a canonical session id — it would resolve to a different state file and steer another goal", session), Code: 1}, nil
	}
	raw := text.Trim(loopOpt(args.BatchJSON))
	if raw == "" {
		return LoopCliResult{Output: "loop steer: --batch-json <path-or-json> is required", Code: 1}, nil
	}
	batchText := raw
	if !strings.HasPrefix(raw, "{") {
		path := raw
		if !filepath.IsAbs(path) {
			path = filepath.Join(args.Cwd, path)
		}
		bytes, err := loopReadBatch(ctx, path)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return LoopCliResult{}, err
		}
		if err != nil {
			return LoopCliResult{Output: fmt.Sprintf("loop steer: could not read the batch at %s (%s)", raw, loopNodeReadMessage(err)), Code: 1}, nil
		}
		batchText = string(bytes)
	}
	batch, err := loopParseJSON(batchText)
	if err != nil {
		return LoopCliResult{Output: fmt.Sprintf("loop steer: batch is not valid JSON (%s)", err.Error()), Code: 1}, nil
	}
	if lossy := loopLossyJSON(batchText); lossy != "" {
		return LoopCliResult{Output: "loop steer: batch holds " + lossy, Code: 1}, nil
	}
	slug := loopBoundSlug(args, session)
	if slug == "" {
		return loopNotBound(LoopVerbSteer, session, "—"), nil
	}
	options := &goalplan.SteeringBatchOptions{BeforeWrite: began}
	if lock != nil || ctx.Done() != nil {
		var lockOptions goalplan.GoalplanWriteLockOptions
		if lock != nil {
			lockOptions = *lock
		}
		if ctx.Done() != nil {
			lockOptions.Context = ctx
		}
		options.Lock = &lockOptions
	}
	result, err := goalplan.ApplySteeringBatch(args.Cwd, slug, batch, options)
	if err != nil {
		return LoopCliResult{}, err
	}
	switch result.Kind {
	case goalplan.SteerResultApplied:
		output := fmt.Sprintf("loop steer: applied %s (%s)", result.Entry.IdempotencyKey, result.Entry.Summary)
		if result.Warning != "" {
			output += "\n  warning: " + result.Warning
		}
		return LoopCliResult{Output: output, Code: 0}, nil
	case goalplan.SteerResultDuplicate:
		output := fmt.Sprintf("loop steer: %s was already applied at %s — nothing to do",
			result.Entry.IdempotencyKey, result.Entry.AppliedAt)
		if result.Warning != "" {
			// CRW-1111: the retry recorded what it could of the rows the first attempt left out, and this
			// row still could not be written.
			output += "\n  warning: " + result.Warning
		}
		return LoopCliResult{Output: output, Code: 0}, nil
	}
	return LoopCliResult{Output: "loop steer: " + result.Reason, Code: 1}, nil
}

// loopAddOp is runAddOp (:354-420): add-criterion and add-work-phase are sugar over the steering batch, which
// owns the lock, the idempotency key and the ledger row, so a second write path never exists.
func loopAddOp(args LoopCliArgs) (LoopCliResult, error) {
	session := loopSessionID(args)
	if session == "" {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: --session <id> is required", args.Verb), Code: 1}, nil
	}
	if !state.IsCanonicalSessionID(session) {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: --session \"%s\" is not a canonical session id - it would resolve to a different state file and steer another goal", args.Verb, session), Code: 1}, nil
	}
	slug := loopBoundSlug(args, session)
	if slug == "" {
		return loopNotBound(args.Verb, session, "-"), nil
	}
	var op map[string]any
	var summary string
	if args.Verb == LoopVerbAddCriterion {
		scenario := ""
		if len(args.Criteria) > 0 {
			scenario = text.Trim(args.Criteria[0])
		}
		if scenario == "" {
			return LoopCliResult{Output: "loop add-criterion: --criterion \"<scenario>\" is required", Code: 1}, nil
		}
		if args.SurfaceGiven && args.Surface == nil {
			return LoopCliResult{Output: "loop add-criterion: --surface needs a value (logic|web|tui|desktop)", Code: 1}, nil
		}
		surface := "logic"
		if args.Surface != nil {
			surface = *args.Surface
		}
		switch surface {
		case "logic", "web", "tui", "desktop":
		default:
			return LoopCliResult{Output: fmt.Sprintf("loop add-criterion: --surface must be logic|web|tui|desktop (got '%s')", loopOpt(args.Surface)), Code: 1}, nil
		}
		if args.Presented != nil && (surface != "desktop" || *args.Presented != "native") {
			return LoopCliResult{Output: "loop add-criterion: --presented native requires --surface desktop", Code: 1}, nil
		}
		op = map[string]any{"kind": "add-criterion", "scenario": scenario, "surface": surface}
		if args.Presented != nil && *args.Presented == "native" {
			op["presented"] = "native"
		}
		summary = scenario
	} else {
		id := text.Trim(loopOpt(args.ID))
		title := text.Trim(loopOpt(args.Title))
		if id == "" || title == "" {
			return LoopCliResult{Output: "loop add-work-phase: --id <id> and --title <text> are both required", Code: 1}, nil
		}
		op = map[string]any{"kind": "add-work-phase", "id": id, "title": title}
		if args.DependsOn != nil && len(*args.DependsOn) > 0 {
			dependsOn := make([]any, 0, len(*args.DependsOn))
			for _, dependency := range *args.DependsOn {
				dependsOn = append(dependsOn, dependency)
			}
			op["dependsOn"] = dependsOn
		}
		// dependsOn stays OUT of the summary so a phase registered without prerequisites keeps the exact
		// idempotency key it had before the upgrade: re-running an old command is a recorded duplicate.
		summary = id + ": " + title
	}
	// The key is content-derived, so re-running the same command is a recorded duplicate and not a second
	// criterion with the same text.
	digest := sha256.Sum256([]byte(summary))
	key := string(args.Verb) + "-" + hex.EncodeToString(digest[:])[:12]
	result, err := goalplan.ApplySteeringBatch(args.Cwd, slug, map[string]any{
		"idempotencyKey": key,
		"rationale":      "crw pabcd loop " + string(args.Verb),
		"evidence":       summary,
		"ops":            []any{op},
	}, nil)
	if err != nil {
		return LoopCliResult{}, err
	}
	switch result.Kind {
	case goalplan.SteerResultApplied:
		// The oracle drops the transaction's warning here, so a ledger row or a plan directory sync that failed
		// after the commit is invisible to the caller (docs/port-cxc/known-defects/CRW-383.md, port: fixed).
		output := RenderLoopPlan(result.Plan, nil)
		if result.Warning != "" {
			output += "\nwarning: " + result.Warning
		}
		return LoopCliResult{Output: output, Code: 0}, nil
	case goalplan.SteerResultDuplicate:
		// The key names the scenario (or the id and title) alone, so a recorded key only says that this
		// criterion or phase was registered; a retry that asks for other options is not that retry.
		if reason := loopAddOpConflict(goalplan.ReadGoalplan(args.Cwd, slug), op); reason != "" {
			return LoopCliResult{Output: fmt.Sprintf("loop %s: %s", args.Verb, reason), Code: 1}, nil
		}
		output := fmt.Sprintf("loop %s: already applied at %s - nothing to do", args.Verb, result.Entry.AppliedAt)
		if result.Warning != "" {
			output += "\nwarning: " + result.Warning
		}
		return LoopCliResult{Output: output, Code: 0}, nil
	case goalplan.SteerResultRejected:
		// CRW-1111: a recorded batch under the same key that differs from this one is refused by the
		// transaction; the add verb's own conflict text names the option that differs when it can.
		if result.Entry != nil {
			if reason := loopAddOpConflict(goalplan.ReadGoalplan(args.Cwd, slug), op); reason != "" {
				return LoopCliResult{Output: fmt.Sprintf("loop %s: %s", args.Verb, reason), Code: 1}, nil
			}
		}
	}
	return LoopCliResult{Output: fmt.Sprintf("loop %s: %s", args.Verb, result.Reason), Code: 1}, nil
}

// loopAddOpConflict is the refusal for a retry of an add verb whose recorded key matches but whose plan holds the
// criterion or phase with other options than the retry asks for, "" for an exact retry (or when the plan no longer
// shows the entry and the retry names no prerequisites, which the key then speaks for).
func loopAddOpConflict(plan *goalplan.Goalplan, op map[string]any) string {
	if plan == nil {
		return ""
	}
	if op["kind"] == "add-criterion" {
		scenario, _ := op["scenario"].(string)
		surface, _ := op["surface"].(string)
		presented, _ := op["presented"].(string)
		found := false
		for _, criterion := range plan.Criteria {
			if criterion.Scenario != scenario {
				continue
			}
			found = true
			have := string(criterion.Surface)
			if have == "" {
				have = "logic"
			}
			if have != surface || string(criterion.Presented) != presented {
				quoted, _ := json.Marshal(scenario)
				return "a criterion with scenario " + string(quoted) + " is already registered with another surface or presentation"
			}
		}
		// A key without its criterion (the state an old command left behind) shows no surface or presentation to compare
		// with: only the default retry (logic, no presentation) is the legacy retry the key stays for.
		if !found && (surface != "logic" || presented != "") {
			quoted, _ := json.Marshal(scenario)
			return "a criterion with scenario " + string(quoted) + " has a recorded key but is not in the plan, so its --surface and --presented cannot be checked against what was registered"
		}
		return ""
	}
	id, _ := op["id"].(string)
	var want []string
	if deps, ok := op["dependsOn"].([]any); ok {
		for _, dep := range deps {
			want = append(want, fmt.Sprint(dep))
		}
	}
	for _, phase := range plan.WorkPhases {
		if phase.ID != id {
			continue
		}
		have := append([]string(nil), phase.DependsOn...)
		slices.Sort(have)
		slices.Sort(want)
		if !slices.Equal(have, want) {
			return "work phase '" + id + "' is already registered with other prerequisites"
		}
		return ""
	}
	// A key without its phase (the state an old command left behind) shows no prerequisites to compare with: only the
	// retry that names none is the legacy retry the key stays for.
	if len(want) > 0 {
		return "work phase '" + id + "' has a recorded key but is not in the plan, so its --depends-on cannot be checked against what was registered"
	}
	return ""
}

// loopDecisionCommit is the three answers of the decision step inside the write lock.
type loopDecisionCommit struct {
	kind     string // "rejected", "changed" or "unchanged"
	reason   string
	warnings []string
}

// loopDecision is runDecision (:525-560): ask records a host-submitted question, decide its answer, under the
// goalplan write lock. Neither writes a ledger row.
func loopDecision(args LoopCliArgs) (LoopCliResult, error) {
	session := loopSessionID(args)
	if session == "" {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: --session <id> is required", args.Verb), Code: 1}, nil
	}
	if !state.IsCanonicalSessionID(session) {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: session id is not canonical", args.Verb), Code: 1}, nil
	}
	slug := loopBoundSlug(args, session)
	if slug == "" {
		return loopNotBound(args.Verb, session, "-"), nil
	}
	id := text.Trim(loopOpt(args.ID))
	if args.Verb == LoopVerbAsk && (id == "" || text.Trim(loopOpt(args.Question)) == "") {
		return LoopCliResult{Output: "loop ask: --id and non-empty --question are required", Code: 1}, nil
	}
	if args.Verb == LoopVerbDecide && (id == "" || text.Trim(loopOpt(args.Answer)) == "") {
		return LoopCliResult{Output: "loop decide: --id and non-empty --answer are required", Code: 1}, nil
	}
	locked, err := goalplan.WithGoalplanWriteLock(args.Cwd, slug, func(plan *goalplan.Goalplan) (loopDecisionCommit, error) {
		var result goalplan.GoalplanLifecycleResult
		if args.Verb == LoopVerbAsk {
			workPhaseIDs := []string{}
			if args.WorkPhaseIDs != nil {
				workPhaseIDs = *args.WorkPhaseIDs
			}
			result = goalplan.AskGoalplanDecision(plan, goalplan.AskGoalplanDecisionInput{
				ID: id, Question: loopOpt(args.Question), Recommendation: args.Recommendation,
				Options: args.Options, WorkPhaseIDs: workPhaseIDs, AskedAt: loopNowISO(),
			})
		} else {
			result = goalplan.DecideGoalplanDecision(plan, id, loopOpt(args.Answer), loopNowISO())
		}
		switch result.Kind {
		case goalplan.GoalplanLifecycleRejected:
			return loopDecisionCommit{kind: "rejected", reason: result.Reason}, nil
		case goalplan.GoalplanLifecycleUnchanged:
			return loopDecisionCommit{kind: "unchanged", reason: result.Reason}, nil
		}
		commit := loopDecisionCommit{kind: "changed"}
		if err := loopInitWriteGoalplan(args.Cwd, result.Plan); err != nil {
			// A plan that published at its final path and then failed the directory sync is a written plan.
			if !state.Published(err) {
				return loopDecisionCommit{}, err
			}
			commit.warnings = append(commit.warnings, cliPublishedGoalplanWarning(slug, err))
		}
		return commit, nil
	}, nil)
	if err != nil {
		return LoopCliResult{}, err
	}
	if locked.Kind == "locked" || locked.Kind == "unreadable" {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: %s", args.Verb, locked.Reason), Code: 1}, nil
	}
	switch locked.Value.kind {
	case "rejected":
		return LoopCliResult{Output: fmt.Sprintf("loop %s: %s", args.Verb, locked.Value.reason), Code: 1}, nil
	case "unchanged":
		return LoopCliResult{Output: fmt.Sprintf("loop %s: %s; nothing to do", args.Verb, locked.Value.reason), Code: 0}, nil
	}
	return loopInitAppendWarnings(LoopCliResult{Output: fmt.Sprintf("loop %s: %s %s applied", args.Verb, slug, id), Code: 0}, locked.Value.warnings), nil
}

// loopAppendLedger is the ledger append of the lifecycle verbs; a test replaces it to observe where it runs.
var loopAppendLedger = goalplan.AppendGoalplanLedger

// loopLifecycleCommit is the answers of the lifecycle step inside the write lock.
type loopLifecycleCommit struct {
	kind     string // "refused", "committed" or "unchanged"
	reason   string
	warnings []string
}

// loopLifecycle is runLifecycle (:562-699): add-task, complete-task and meet-criterion share one locked
// read-modify-write. goalplan.json is the commit point: a failed ledger append answers success with a warning,
// because the plan on disk already moved.
func loopLifecycle(args LoopCliArgs) (LoopCliResult, error) {
	session := loopSessionID(args)
	if session == "" {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: --session <id> is required", args.Verb), Code: 1}, nil
	}
	if !state.IsCanonicalSessionID(session) {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: --session \"%s\" is not a canonical session id - it would resolve to a different state file and steer another goal", args.Verb, session), Code: 1}, nil
	}
	slug := loopBoundSlug(args, session)
	if slug == "" {
		return loopNotBound(args.Verb, session, "-"), nil
	}
	id := text.Trim(loopOpt(args.ID))
	var ledgerEvent goalplan.GoalplanLedgerEvent
	ledgerDetail := ""
	var transition func(*goalplan.Goalplan) goalplan.GoalplanLifecycleResult
	switch args.Verb {
	case LoopVerbAddTask:
		workPhaseID := text.Trim(loopOpt(args.WorkPhaseID))
		title := text.Trim(loopOpt(args.Title))
		// One sentence naming every required argument: reporting them one rejection at a time cost the caller a
		// round trip per missing flag (issue #31).
		if workPhaseID == "" || id == "" || title == "" {
			return LoopCliResult{Output: "loop add-task: --work-phase, --id, and non-empty --title are required", Code: 1}, nil
		}
		var dependsOn []string
		if args.DependsOn != nil {
			dependsOn = *args.DependsOn
		}
		if len(dependsOn) > 0 {
			ledgerEvent = goalplan.EventDependencyRegistered
			ledgerDetail = fmt.Sprintf("task %s/%s depends on %s", workPhaseID, id, strings.Join(dependsOn, ", "))
		}
		transition = func(plan *goalplan.Goalplan) goalplan.GoalplanLifecycleResult {
			return goalplan.AddGoalplanTask(plan, workPhaseID, goalplan.AddGoalplanTaskInput{ID: id, Title: title, DependsOn: dependsOn})
		}
	case LoopVerbCompleteTask:
		workPhaseID := text.Trim(loopOpt(args.WorkPhaseID))
		outcome := text.Trim(loopOpt(args.Outcome))
		if workPhaseID == "" || id == "" || outcome == "" {
			return LoopCliResult{Output: "loop complete-task: --work-phase, --id, and non-empty --outcome are required", Code: 1}, nil
		}
		ledgerEvent = goalplan.EventTaskDone
		ledgerDetail = outcome
		transition = func(plan *goalplan.Goalplan) goalplan.GoalplanLifecycleResult {
			return goalplan.CompleteGoalplanTask(plan, workPhaseID, id, outcome)
		}
	default:
		evidence := text.Trim(loopOpt(args.Evidence))
		if id == "" || evidence == "" {
			return LoopCliResult{Output: "loop meet-criterion: --id and non-empty --evidence are required", Code: 1}, nil
		}
		ledgerEvent = goalplan.EventCriterionMet
		ledgerDetail = evidence
		transition = func(plan *goalplan.Goalplan) goalplan.GoalplanLifecycleResult {
			return goalplan.MeetGoalplanCriterion(plan, id, evidence)
		}
	}
	locked, err := goalplan.WithGoalplanWriteLock(args.Cwd, slug, func(plan *goalplan.Goalplan) (loopLifecycleCommit, error) {
		result := transition(plan)
		switch result.Kind {
		case goalplan.GoalplanLifecycleRejected:
			return loopLifecycleCommit{kind: "refused", reason: result.Reason}, nil
		case goalplan.GoalplanLifecycleUnchanged:
			// The idempotent resubmission: the plan already carries this transition. It exits 0 like a fresh
			// apply but appends no second ledger row, or the ledger would claim the task finished twice.
			return loopLifecycleCommit{kind: "unchanged", reason: result.Reason}, nil
		}
		commit := loopLifecycleCommit{kind: "committed"}
		if err := loopInitWriteGoalplan(args.Cwd, result.Plan); err != nil {
			if !state.Published(err) {
				return loopLifecycleCommit{}, err
			}
			commit.warnings = append(commit.warnings, cliPublishedGoalplanWarning(slug, err))
		}
		// The row is written before the lock is released: the plan and the ledger then order two writers the same
		// way, and the row's timestamp is taken while the transition is still the latest one (as the steering
		// transaction writes its own rows).
		if ledgerEvent != "" {
			if err := loopAppendLedger(args.Cwd, slug, goalplan.GoalplanLedgerEntry{
				Ts: loopNowISO(), Slug: slug, Event: ledgerEvent, Detail: ledgerDetail,
			}); err != nil {
				commit.warnings = append(commit.warnings, "warning: goalplan state was committed, but ledger append failed: "+err.Error())
			}
		}
		return commit, nil
	}, nil)
	if err != nil {
		return LoopCliResult{}, err
	}
	if locked.Kind == "locked" || locked.Kind == "unreadable" {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: %s", args.Verb, locked.Reason), Code: 1}, nil
	}
	switch locked.Value.kind {
	case "refused":
		return LoopCliResult{Output: fmt.Sprintf("loop %s: %s", args.Verb, locked.Value.reason), Code: 1}, nil
	case "unchanged":
		// The pure reason IS the message: wrapping it would give one state two wordings by surface.
		return LoopCliResult{Output: fmt.Sprintf("loop %s: %s; nothing to do", args.Verb, locked.Value.reason), Code: 0}, nil
	}
	return loopInitAppendWarnings(LoopCliResult{Output: fmt.Sprintf("loop %s: %s %s applied", args.Verb, slug, id), Code: 0}, locked.Value.warnings), nil
}
