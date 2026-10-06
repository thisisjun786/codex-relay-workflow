// Loop CLI command library: goalplan-cli.ts at v0.2.40 (commit 3c1459ac), the read half.
//
// CRW-382 ported the structural parser (loop_args.go) and the renderers (loop_render.go). This file ports
// runGoalplanCli (:751-865) for the verbs that only read or create a plan - init, show, validate and ready -
// plus runReady (:461-525). The mutating verbs (steer, add-criterion, add-work-phase, add-task,
// complete-task, meet-criterion, ask, decide) belong to CRW-383 (C2); loopSeam is the one insertion point
// they take, so this issue never has to be undone to add them.
//
// The caller's shape is the oracle's cli.ts dispatch: parse, then run, and a library error is the oracle's
// uncaught throw (the harness row answers it as "crw cli failed: "). No package-level initializer runs here.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// LoopCliResult is GoalplanCliResult (:272-275): the text and the exit status, without writing process streams.
type LoopCliResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

// loopSeamText is the mutating verbs' answer: CRW-383 owns the write surface, and this build writes nothing,
// so a caller that ignores the exit status still cannot change a plan.
const loopSeamText = "loop %s: the write surface of the loop CLI is not in this build yet; the verb arrives with the loop write issue. Nothing was written."

// loopSeam is the mutating-verb seam CRW-383 replaces. It never touches the plan.
func loopSeam(args LoopCliArgs) LoopCliResult {
	return LoopCliResult{Output: fmt.Sprintf(loopSeamText, args.Verb), Code: 1}
}

// loopNowISO is new Date().toISOString() for the timestamps this file stamps.
func loopNowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// loopOpt is the value of an optional flag, or "" when it is absent.
func loopOpt(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// loopSessionID is the trimmed --session value, "" when absent.
func loopSessionID(args LoopCliArgs) string { return text.Trim(loopOpt(args.Session)) }

// RunLoopCli is runGoalplanCli (:751-865) for the verbs this issue owns. A non-nil error is the oracle's
// uncaught throw: a write that failed, or a lock status that could not be read.
func RunLoopCli(args LoopCliArgs) (LoopCliResult, error) {
	if args.Verb == LoopVerbHelp {
		return LoopCliResult{Output: RenderLoopHelp(), Code: 0}, nil
	}
	if args.Verb == LoopVerbInit {
		return loopInit(args)
	}
	// Checked BEFORE ResolveLoopSlug(): state paths sanitize the id, so a non-canonical one would resolve to
	// a DIFFERENT session's state file and this verb would print or judge a plan the caller never named. The
	// oracle guards the ready verb alone (:802-810), which leaves show --session a/b reading session a-b's
	// plan (docs/port-cxc/known-defects/CRW-646.md, port: fixed); the port applies the guard to every verb
	// that resolves a plan through --session.
	if id := loopSessionID(args); id != "" && !state.IsCanonicalSessionID(id) {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: session id is not canonical", args.Verb), Code: 1}, nil
	}
	switch args.Verb {
	case LoopVerbSteer, LoopVerbAsk, LoopVerbDecide, LoopVerbAddCriterion, LoopVerbAddWorkPhase,
		LoopVerbAddTask, LoopVerbCompleteTask, LoopVerbMeetCriterion:
		return loopSeam(args), nil
	}
	slug := ResolveLoopSlug(args)
	if slug == nil {
		return LoopCliResult{Output: fmt.Sprintf(
			"loop %s: --slug \"<text>\", --objective \"<text>\", or --session <id> (with a bound plan) is required",
			args.Verb), Code: 1}, nil
	}
	plan := goalplan.ReadGoalplan(args.Cwd, *slug)
	if plan == nil {
		// Issue #29: "no plan found" used to hide truncated writes and schema rejects.
		read := goalplan.ReadGoalplanDetailed(args.Cwd, *slug)
		return LoopCliResult{Output: DescribeLoopReadFailure(read, string(args.Verb), *slug), Code: 1}, nil
	}
	if args.Verb == LoopVerbShow {
		lock, err := goalplan.GoalplanWriteLockStatus(args.Cwd, plan.Slug, nil)
		if err != nil {
			return LoopCliResult{}, err
		}
		return LoopCliResult{Output: RenderLoopPlan(plan, &lock), Code: 0}, nil
	}
	if args.Verb == LoopVerbReady {
		return loopReady(args, plan), nil
	}
	return loopValidate(args, plan)
}

// loopInit is the init branch (:753-800): a real objective, no --surface, no plan of that slug yet, a
// resolvable source identity when the plan is bound to a session, then the plan, its created ledger row and
// the session's slug binding.
func loopInit(args LoopCliArgs) (LoopCliResult, error) {
	objective := text.Trim(loopOpt(args.Objective))
	if objective == "" {
		return LoopCliResult{Output: "loop init: --objective \"<text>\" is required", Code: 1}, nil
	}
	if args.SurfaceGiven {
		return LoopCliResult{Output: "loop init: --surface is not applied at init; bind the plan with --session and " +
			"register each surfaced criterion with crw pabcd loop add-criterion --session <id> --criterion <text> " +
			"--surface <logic|web|tui|desktop>\nNothing was written.", Code: 1}, nil
	}
	slug := interview.DeriveSlug(objective)
	// The oracle asks only whether a plan LOADED, so a truncated or structurally invalid plan file reads as
	// absent and WriteGoalplan's rename replaces its bytes (docs/port-cxc/known-defects/CRW-646.md,
	// port: fixed). Refuse whenever the plan file is there, so a damaged plan stays for repair.
	if read := goalplan.ReadGoalplanDetailed(args.Cwd, slug); read.Diagnostic == nil || loopPlanFileExists(args.Cwd, slug) {
		if read.Diagnostic == nil {
			return LoopCliResult{Output: fmt.Sprintf("loop init: a plan already exists at slug '%s' (use show/validate)", slug), Code: 1}, nil
		}
		return LoopCliResult{Output: fmt.Sprintf(
			"loop init: a plan file for slug '%s' already exists but could not be read (%s); refusing to overwrite it\nNothing was written.",
			slug, read.Diagnostic.Kind), Code: 1}, nil
	}
	// #133: a BOUND plan promises a closable cycle. Refuse here when the source identity cannot be resolved,
	// rather than letting P->A->B->C succeed and then stranding the session at C with no testReceiptPath.
	// Guarded on --session: an init without one writes the local artifact and binds nothing.
	sessionID := loopSessionID(args)
	if sessionID != "" {
		if verdict := session.CheckBound(args.Cwd, sessionID); !verdict.OK {
			return LoopCliResult{Output: "loop init: " + verdict.Reason + "\nNothing was written.", Code: 1}, nil
		}
		// ReadState answers a fresh IDLE state for a file it cannot decode, so binding a slug through it would
		// replace a damaged session state with a default and lose the original bytes
		// (docs/port-cxc/known-defects/CRW-646.md, port: fixed). Refuse before anything is written, and again
		// under the session lock at the binding itself.
		next, unreadable := state.ReadStateStrict(args.Cwd, sessionID)
		if unreadable {
			return LoopCliResult{Output: "loop init: session " + sessionID + " has unreadable state; refusing to overwrite it\nNothing was written.", Code: 1}, nil
		}
		// The strict reader also rebuilds records it cannot keep whole, so the write-back below would replace
		// them with the rebuilt ones (docs/port-cxc/known-defects/CRW-646.md, port: fixed). Every other writer
		// of the session file refuses such a rewrite by decision; this one follows the same rule.
		if raw, err := os.ReadFile(state.StatePath(args.Cwd, sessionID)); err == nil && !loopStateRewritable(raw, next) {
			return LoopCliResult{Output: "loop init: session " + sessionID + " holds records a rewrite would change; refusing to overwrite it\nNothing was written.", Code: 1}, nil
		}
	}
	criteria := make([]goalplan.NewGoalplanCriterion, 0, len(args.Criteria))
	for _, scenario := range args.Criteria {
		criteria = append(criteria, goalplan.NewGoalplanCriterion{Scenario: scenario})
	}
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: objective, Criteria: criteria, SchemaVersion: args.SchemaVersion})
	if err := goalplan.WriteGoalplan(args.Cwd, plan); err != nil {
		return LoopCliResult{}, err
	}
	if err := goalplan.AppendGoalplanLedger(args.Cwd, slug, goalplan.GoalplanLedgerEntry{
		Ts: loopNowISO(), Slug: slug, Event: goalplan.EventCreated,
		Detail: "init objective=\"" + objective + "\" criteria=" + fmt.Sprint(len(args.Criteria)),
	}); err != nil {
		return LoopCliResult{}, err
	}
	if sessionID != "" {
		if err := state.WithSessionLock(args.Cwd, sessionID, func() error {
			next, unreadable := state.ReadStateStrict(args.Cwd, sessionID)
			if unreadable {
				return errors.New("session state is unreadable; refusing to overwrite it")
			}
			if raw, err := os.ReadFile(state.StatePath(args.Cwd, sessionID)); err == nil && !loopStateRewritable(raw, next) {
				return errors.New("session state holds records a rewrite would change; refusing to overwrite it")
			}
			next.Slug = slug
			return state.WriteState(args.Cwd, next)
		}); err != nil {
			return LoopCliResult{}, err
		}
	}
	return LoopCliResult{Output: RenderLoopPlan(goalplan.ReadGoalplan(args.Cwd, slug), nil), Code: 0}, nil
}

// loopStateRewritable says whether writing next back over raw would keep every record the file stores. It is a copy of
// sessionHookStateRewritable and sessionHookInterviewKeepsStored of internal/pabcd/hook/session_hooks.go, whose package
// is not this issue's edit region, so the logic is copied rather than moved or exported (the parent records the
// consolidation as a follow-up). The reader normalises what it handles, so a write-back that only means to bind a slug
// can still drop or change records: ReconstructUnverified stops at MaxUnverifiedSubagents, cuts a receiptClaimed to
// MaxReceiptClaimLen and replaces a field of the wrong type (state.RewriteKeepsUnverified), ReconstructInterview caps
// every tracker array at interview.MaxTrackerArray, and a legacy D-close marker loses its distinction.
func loopStateRewritable(raw []byte, next state.State) bool {
	if next.DcloseRecovery != nil && next.DcloseRecovery.Legacy {
		return false
	}
	if !state.RewriteKeepsUnverified(raw, next.UnverifiedSubagents) {
		return false
	}
	return loopInterviewKeepsStored(raw, next.Interview)
}

// loopInterviewKeepsStored says whether the tracker the write would publish still holds every entry the file stores. A
// stored array longer than the rebuilt one is a record the write-back would lose. The rebuilt tracker cannot be compared
// instead: the reader normalises a dimension (an absent array becomes empty, an unknown level becomes low), so a value
// comparison would refuse every write. A tracker the file does not hold, or holds as null, holds no entry to lose.
func loopInterviewKeepsStored(raw []byte, kept *interview.Tracker) bool {
	stored, ok := loopStateJSONField(raw, "interview")
	if !ok {
		return false
	}
	object, _ := stored.(map[string]any)
	contradictions, _ := object["contradictions"].([]any)
	assumptions, _ := object["assumptions"].([]any)
	ontology, _ := object["ontologySchema"].([]any)
	if kept == nil {
		return len(contradictions) == 0 && len(assumptions) == 0 && len(ontology) == 0
	}
	if len(contradictions) > len(kept.Contradictions) || len(assumptions) > len(kept.Assumptions) || len(ontology) > len(kept.OntologySchema) {
		return false
	}
	for i, entity := range ontology {
		record, _ := entity.(map[string]any)
		relationships, _ := record["relationships"].([]any)
		if len(relationships) > len(kept.OntologySchema[i].Relationships) {
			return false
		}
	}
	return true
}

// loopStateJSONField is one top-level key of a state document, decoded as the reader decodes it: numbers stay json.Number.
// A document that is not one JSON object is refused; a key the document does not hold is the nil value.
func loopStateJSONField(doc []byte, key string) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, false
	}
	for dec.More() {
		name, err := dec.Token()
		if err != nil {
			return nil, false
		}
		if field, ok := name.(string); ok && field == key {
			var value any
			if dec.Decode(&value) != nil {
				return nil, false
			}
			return value, true
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return nil, false
		}
	}
	return nil, true
}

// loopPlanFileExists reports whether slug's plan file is there as a regular file. A path the slug resolver
// refuses - a linked state root, say - is not an existing plan file: the write path reports that refusal
// itself, exactly as the oracle's writeGoalplan does.
func loopPlanFileExists(cwd, slug string) bool {
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		return false
	}
	info, err := os.Lstat(filepath.Join(dir, goalplan.GoalplanFile))
	return err == nil && info.Mode().IsRegular()
}

// loopReadyPhaseRow, loopReadyTaskRow, loopReadyOpenDecisionRow and loopReadyAwaitingRow are runReady's JSON
// rows (:490-501). They are structs, not maps, because the recorded expectation compares the exact bytes and
// the oracle's key order is the literal's.
type loopReadyPhaseRow struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	DependsOn []string `json:"dependsOn"`
}

type loopReadyTaskRow struct {
	WorkPhaseID string `json:"workPhaseId"`
	ID          string `json:"id"`
	Title       string `json:"title"`
}

type loopReadyOpenDecisionRow struct {
	ID             string   `json:"id"`
	Question       string   `json:"question"`
	Recommendation *string  `json:"recommendation,omitempty"`
	Options        []string `json:"options,omitempty"`
	AskedAt        string   `json:"askedAt"`
}

type loopReadyAwaitingRow struct {
	WorkPhaseID string   `json:"workPhaseId"`
	DecisionIDs []string `json:"decisionIds"`
}

// loopReadyDoc is the object runReady's --json form prints (:486-502). The two decision arrays are pointers so
// that "the plan has no decisions key" (absent) stays apart from "it has an empty one" ([]).
type loopReadyDoc struct {
	Slug              string                      `json:"slug"`
	ReadyWorkPhases   []loopReadyPhaseRow         `json:"readyWorkPhases"`
	ReadyTasks        []loopReadyTaskRow          `json:"readyTasks"`
	OpenDecisions     *[]loopReadyOpenDecisionRow `json:"openDecisions,omitempty"`
	AwaitingDecisions *[]loopReadyAwaitingRow     `json:"awaitingDecisions,omitempty"`
}

// loopReady is runReady (:461-525). Integrity is checked FIRST: listing ready items out of a plan with a
// duplicate id or a dangling edge would hand back a confident answer computed from a graph the plan itself
// rejects.
func loopReady(args LoopCliArgs, plan *goalplan.Goalplan) LoopCliResult {
	reasons := append(goalplan.GoalplanDefinitionIntegrityReasons(plan), goalplan.GoalplanDependencyCompletionReasons(plan)...)
	if len(reasons) > 0 {
		lines := []string{"loop ready: " + plan.Slug + " has an invalid dependency graph"}
		for _, reason := range reasons {
			lines = append(lines, "  - "+reason)
		}
		return LoopCliResult{Output: strings.Join(lines, "\n"), Code: 1}
	}
	phases := goalplan.ReadyWorkPhases(plan)
	tasks := goalplan.ReadyTasks(plan)
	phaseRows := make([]loopReadyPhaseRow, 0, len(phases))
	for _, phase := range phases {
		dependsOn := phase.DependsOn
		if dependsOn == nil {
			dependsOn = []string{}
		}
		phaseRows = append(phaseRows, loopReadyPhaseRow{ID: phase.ID, Title: phase.Title, Status: string(phase.Status), DependsOn: dependsOn})
	}
	taskRows := make([]loopReadyTaskRow, 0, len(tasks))
	for _, entry := range tasks {
		taskRows = append(taskRows, loopReadyTaskRow{WorkPhaseID: entry.WorkPhaseID, ID: entry.Task.ID, Title: entry.Task.Title})
	}
	openDecisions := []loopReadyOpenDecisionRow{}
	awaitingDecisions := []loopReadyAwaitingRow{}
	hasDecisions := plan.Decisions != nil
	if hasDecisions {
		for i := range plan.Decisions {
			decision := &plan.Decisions[i]
			if decision.Status != goalplan.DecisionOpen {
				continue
			}
			row := loopReadyOpenDecisionRow{ID: decision.ID, Question: decision.Question, AskedAt: decision.AskedAt}
			if decision.Recommendation != "" {
				recommendation := decision.Recommendation
				row.Recommendation = &recommendation
			}
			if decision.Options != nil {
				row.Options = decision.Options
			}
			openDecisions = append(openDecisions, row)
		}
		for i := range plan.WorkPhases {
			phase := &plan.WorkPhases[i]
			if phase.Status != goalplan.WorkPhasePending && phase.Status != goalplan.WorkPhaseInProgress {
				continue
			}
			ids := []string{}
			for _, id := range goalplan.OpenDecisionIDsForPhase(plan, phase) {
				for j := range plan.Decisions {
					if plan.Decisions[j].ID == id && plan.Decisions[j].Status == goalplan.DecisionOpen {
						ids = append(ids, id)
						break
					}
				}
			}
			if len(ids) > 0 {
				awaitingDecisions = append(awaitingDecisions, loopReadyAwaitingRow{WorkPhaseID: phase.ID, DecisionIDs: ids})
			}
		}
	}
	if args.JSON {
		doc := loopReadyDoc{Slug: plan.Slug, ReadyWorkPhases: phaseRows, ReadyTasks: taskRows}
		if hasDecisions {
			doc.OpenDecisions, doc.AwaitingDecisions = &openDecisions, &awaitingDecisions
		}
		// statusJSON is this package's JSON.stringify: no HTML escaping and U+2028/U+2029 kept, which is what
		// the oracle prints for a title or a question holding those characters.
		encoded, err := statusJSON(doc)
		if err != nil {
			return LoopCliResult{Output: err.Error(), Code: 1}
		}
		return LoopCliResult{Output: encoded, Code: 0}
	}
	lines := []string{"[crw loop ready: " + plan.Slug + "]"}
	lines = append(lines, loopReadyPhaseLine(phases), loopReadyTaskLine(tasks))
	if hasDecisions {
		lines = append(lines, loopOpenDecisionLine(openDecisions), loopAwaitingDecisionLine(awaitingDecisions))
	}
	return LoopCliResult{Output: strings.Join(lines, "\n"), Code: 0}
}

func loopReadyPhaseLine(phases []*goalplan.GoalplanWorkPhase) string {
	if len(phases) == 0 {
		return "readyWorkPhases: none"
	}
	parts := make([]string, 0, len(phases))
	for _, phase := range phases {
		parts = append(parts, phase.ID+" ("+phase.Title+")")
	}
	return "readyWorkPhases: " + strings.Join(parts, "; ")
}

func loopReadyTaskLine(tasks []goalplan.ReadyGoalplanTask) string {
	if len(tasks) == 0 {
		return "readyTasks: none"
	}
	parts := make([]string, 0, len(tasks))
	for _, entry := range tasks {
		parts = append(parts, entry.WorkPhaseID+"/"+entry.Task.ID+" ("+entry.Task.Title+")")
	}
	return "readyTasks: " + strings.Join(parts, "; ")
}

func loopOpenDecisionLine(decisions []loopReadyOpenDecisionRow) string {
	if len(decisions) == 0 {
		return "openDecisions: none"
	}
	parts := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		parts = append(parts, decision.ID+" ("+decision.Question+")")
	}
	return "openDecisions: " + strings.Join(parts, "; ")
}

func loopAwaitingDecisionLine(entries []loopReadyAwaitingRow) string {
	if len(entries) == 0 {
		return "awaitingDecisions: none"
	}
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, entry.WorkPhaseID+": "+strings.Join(entry.DecisionIDs, ", "))
	}
	return "awaitingDecisions: " + strings.Join(parts, "; ")
}

// loopValidate is the validate branch (:841-864): a read-only E8 context, so a schemaVersion 2 plan is
// reported on rather than refused for a missing context. Nothing here mutates state.
func loopValidate(args LoopCliArgs, plan *goalplan.Goalplan) (LoopCliResult, error) {
	switch sessionID := loopSessionID(args); {
	case sessionID != "":
		if _, err := session.Resolve(args.Cwd, sessionID); err != nil {
			return LoopCliResult{Output: "loop validate: SOURCE-ROOT: " + err.Error(), Code: 1}, nil
		}
	case plan.FinalGate != nil && plan.FinalGate.SourceIdentity != nil && plan.FinalGate.SourceIdentity.SourceRoot != nil:
		return LoopCliResult{Output: "loop validate: SOURCE-ROOT: pass --session <id> to validate a bound source worktree.", Code: 1}, nil
	}
	ctx := &goalplan.GoalplanValidationCtx{
		Cwd:                   args.Cwd,
		CaptureSourceIdentity: func(cwd string) goalplan.SourceIdentity { return loopCaptureSource(args, cwd) },
		CompareSource: func(a, b goalplan.SourceIdentity) source.Comparison {
			return source.Compare(a.Identity(), b.Identity())
		},
		ReadReceipt: func(path string, kind gate.ReceiptKind) (goalplan.GoalplanReceiptEvidence, error) {
			receipt, err := gate.ParseSourceBoundReceipt(path, args.Cwd, kind)
			if err != nil {
				return goalplan.GoalplanReceiptEvidence{}, err
			}
			return goalplan.GoalplanReceiptEvidence{SourceIdentity: receipt.SourceIdentity, ArtifactManifest: receipt.ArtifactManifest}, nil
		},
	}
	verdict := goalplan.ValidateGoalplan(plan, ctx)
	if verdict.OK {
		return LoopCliResult{Output: "[crw loop validate: " + plan.Slug + "] OK " + loopEmDash + " complete + all met criteria carry evidence", Code: 0}, nil
	}
	lines := []string{"[crw loop validate: " + plan.Slug + "] FAIL"}
	for _, reason := range verdict.Reasons {
		lines = append(lines, "  - "+reason)
	}
	return LoopCliResult{Output: strings.Join(lines, "\n"), Code: 1}, nil
}

// loopEmDash is the em dash the oracle's OK line carries (U+2014), written as an escape so the source stays
// pure ASCII.
const loopEmDash = "\u2014"

// loopCaptureSource is captureSourceIdentity: the session's bound worktree when it has one, cwd otherwise.
// The oracle's resolver throws on a broken binding and that throw reaches the validator's catch arm, so this
// panics with the same error, which finalGateCaptureCurrent recovers.
func loopCaptureSource(args LoopCliArgs, cwd string) goalplan.SourceIdentity {
	if sessionID := loopSessionID(args); sessionID != "" {
		identity, err := session.Capture(cwd, sessionID, session.CaptureOptions{})
		if err != nil {
			panic(err)
		}
		return loopSourceIdentity(identity)
	}
	return loopSourceIdentity(source.Capture(cwd, source.Options{}))
}

// loopSourceIdentity is a source identity as the state and goalplan records hold it.
func loopSourceIdentity(identity source.Identity) state.SourceIdentity {
	out := state.SourceIdentity{Kind: identity.Kind, CommitSha: identity.CommitSha, Dirty: identity.Dirty,
		CapturedAt: identity.CapturedAt, SourceRoot: identity.SourceRoot}
	if identity.TreeHash != "" {
		hash := identity.TreeHash
		out.TreeHash = &hash
	}
	return out
}
