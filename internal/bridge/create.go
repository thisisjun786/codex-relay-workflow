package bridge

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// createAckBound is the acknowledgement bound every App Server call of the create path uses. Under
// load the host's answers to thread/start, thread/name/set and turn/start, and to the profile
// reads this path makes (config/read, plugin/installed, mcpServerStatus/list), have taken longer
// than the default 20 s: three of about eighty creations on 2026-10-06 hit it, and one answered
// after 25 s. A creation whose answer is lost leaves an outcome_unknown receipt although the host
// did the work, so the create path waits longer than every other caller of the client.
const createAckBound = 60 * time.Second

// refuseReadOnlyChild refuses a relay child (the child role) on every bridge path that creates one (create_thread, the
// relay's managed start through it, create_worktree_thread), before anything is reserved or sent, when it could not work
// or could not report through the relay daemon's control.sock (CRW-1296): a readOnly sandbox, or a workspace-write one
// without network access, where Codex refuses that connect (EPERM, codex-cli 0.154.0). A workspace-write child passes
// only with an expected policy granting networkAccess, which travels in the start config and is checked on creation.
func refuseReadOnlyChild(role, sandbox string, policy map[string]any) error {
	if role == "child" && !settings.RelayChildReaches(sandbox, policy) {
		return &Invalid{"a relay child reports through the relay daemon's socket, which a readOnly sandbox or a workspace-write sandbox without network access cannot reach; recreate it with a writable sandbox that reaches it (danger-full-access, or workspace-write with an expected_sandbox_policy granting networkAccess); settings are never silently changed"}
	}
	return nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type CreateThread struct {
	RequestID, CWD, Prompt, Title, Sandbox, Model, ProjectID, Effort, Exception, Role, MCPProfile string
	Roots                                                                                         []string
	Policy                                                                                        map[string]any
	// PromptRequired marks a call that must not create a thread with no first turn (the MCP create_thread; a relay
	// creation always carries its bootstrap prompt). With no Prompt it never begins an attempt (createWithoutPrompt).
	// With a Prompt, a conflict with a reservation made with its arguments without a prompt, before prompts were
	// required, retires that reservation (conflictBeforePrompt). It is not part of the fingerprint.
	PromptRequired bool
}

// ErrPromptRequired refuses a create_thread that must bring a prompt (PromptRequired), brings none, and finds no receipt
// on its request_id: nothing was reserved, so the call that brings a prompt may reuse the id.
var ErrPromptRequired = &Invalid{`prompt is required: a thread created with no first turn has no rollout, so thread/resume and thread/turns/list on it fail with "missing source rollout"`}

// RetiredBeforePrompt is the error a create_thread reservation made without a prompt, before prompts were required, is
// retired with (retireBeforePrompt), and so the start of the error of every receipt retired that way.
const RetiredBeforePrompt = "this create_thread reservation was made without a prompt, before a prompt was required, and nothing of it reached the host; it was retired instead of resumed, because resumed it would create a thread with no first turn, so no call on its request_id creates a thread. Create the thread with a prompt under a new request_id"

// ErrRetiredBeforePrompt answers a create_thread that must bring a prompt (PromptRequired) on a request_id whose
// reservation was made with the call's arguments without a prompt, before prompts were required: that reservation is
// retired (RetiredBeforePrompt), by this call or an earlier one, and never creates a thread.
var ErrRetiredBeforePrompt = &Invalid{RetiredBeforePrompt}

// errBeginsWithoutPrompt is createWithoutPrompt's refusal of the attempt it never begins; its callers never see it.
var errBeginsWithoutPrompt = errors.New("a create_thread that must bring a prompt and brings none begins no attempt")

// CreationWatch is the turn watch a creation opened on its thread, handed to the caller instead of being let go
// as the creation returns (CreateThreadWatched). The watch holds the thread's mutation gate and its
// subscription until Release, so the caller can wait for the standby turn's turn/completed on the connection
// that carried the creation.
type CreationWatch struct {
	watch   *appserver.TurnWatch
	receipt ledger.Receipt
	once    sync.Once
}

// TurnCompleted reports whether the watch has received turn/completed for turn.
func (w *CreationWatch) TurnCompleted(turn string) bool { return w.watch.Completed(turn) }

// Release lets the watch go, as the creation does when nobody keeps it. It is idempotent.
func (w *CreationWatch) Release() {
	w.once.Do(func() { finishSubscription(w.watch, w.receipt, true) })
}

type keepWatchKey struct{}

// CreateThreadWatched is CreateThread that keeps the creation's turn watch open: the returned CreationWatch is
// nil when the creation opened none (a scripted transport) or failed, and otherwise the caller releases it.
func (b *Bridge) CreateThreadWatched(ctx context.Context, in CreateThread) (ledger.Receipt, *CreationWatch, error) {
	keep := &CreationWatch{}
	receipt, err := b.CreateThread(context.WithValue(ctx, keepWatchKey{}, keep), in)
	if keep.watch == nil {
		return receipt, nil, err
	}
	return receipt, keep, err
}

func (b *Bridge) CreateThread(ctx context.Context, in CreateThread) (receipt ledger.Receipt, failure error) {
	var watch *appserver.TurnWatch
	var result ledger.Receipt
	defer func() {
		// Only an accepted creation hands its watch over: a creation that failed or is unknown goes on to the standby recovery, which
		// takes the thread's gate itself.
		if keep, ok := ctx.Value(keepWatchKey{}).(*CreationWatch); ok && watch != nil && failure == nil && result["status"] == "accepted" {
			keep.watch, keep.receipt = watch, result
			return
		}
		finishSubscription(watch, result, true)
	}()
	// One bound on this path's context covers every call it makes, before and inside the mutation.
	ctx = appserver.WithAckBound(ctx, createAckBound)
	if in.PromptRequired && in.Prompt == "" {
		return b.createWithoutPrompt(ctx, in)
	}
	if err := nonempty(in.CWD, "cwd", 100000); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(in.CWD) {
		return nil, &Invalid{"cwd must be an existing absolute directory on the App Server host"}
	}
	if in.Sandbox != "read-only" && in.Sandbox != "workspace-write" && in.Sandbox != "danger-full-access" {
		return nil, &Invalid{"Unsupported sandbox"}
	}
	for _, pair := range [][3]any{{in.Prompt, "prompt", 100000}, {in.Title, "title", 500}} {
		if value := pyjson.Text(pair[0]); value != "" {
			if err := nonempty(value, pair[1].(string), pair[2].(int)); err != nil {
				return nil, err
			}
		}
	}
	if in.Policy != nil {
		if err := validateSandboxPolicy(in.Policy); err != nil {
			return nil, err
		}
	}
	params := createThreadParams(in)
	var authorized execution.Authorized
	var contract settings.Contract
	var selection execution.MCPSelection
	var launch map[string]any
	validate := func() error {
		cwd, err := directory(in.CWD)
		if err != nil {
			return err
		}
		// CRW-1265's rule is asked about the cwd and the roots, and its verdict is a warning, never a refusal (CRW-1308): a
		// sandboxed thread may start in the account home, a folder under it or above a project root.
		sandboxroot.Warn("bridge/create_thread", sandboxroot.CheckAll(cwd, absolute(writableRoots(in.Roots, in.Policy)), effectiveSandbox(in.Sandbox, in.Policy)))
		authorized, err = b.Policy.Authorize(execution.Input{Model: execution.Optional(in.Model), Effort: execution.Optional(in.Effort), CWD: cwd, Exception: in.Exception, Role: in.Role})
		if err != nil {
			return err
		}
		if err := refuseReadOnlyChild(in.Role, in.Sandbox, in.Policy); err != nil {
			return err
		}
		contract = settings.Contract{CWD: cwd, Sandbox: in.Sandbox, Model: authorized.Model, ReasoningEffort: authorized.Effort, Roots: in.Roots, ExpectedPolicy: in.Policy, AutoCompactTokenLimit: authorized.AutoCompactTokenLimit}
		if err := contract.Validate(); err != nil {
			return err
		}
		if selection, err = b.selectMCP(in.Role, in.MCPProfile); err != nil {
			return err
		}
		launch = map[string]any{"cwd": cwd, "sandbox": in.Sandbox, "approvalPolicy": "never", "ephemeral": false}
		for k, v := range contract.StartParams() {
			launch[k] = v
		}
		if in.ProjectID != "" {
			launch["projectId"] = in.ProjectID
		}
		return nil
	}
	result, err := b.mutate(ctx, mutation{requestID: in.RequestID, method: "create_thread", params: params, validate: validate, action: func(ctx context.Context, receipt ledger.Receipt, effects *[]string) error {
		receipt["executionPolicy"] = authorized.Receipt
		if _, err := b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		if in.ProjectID != "" {
			if _, err := b.call(ctx, "project/read", map[string]any{"projectId": in.ProjectID}); err != nil {
				return err
			}
		}
		if selection.Name != "" {
			var info map[string]any
			var err error
			if contract.MCP, info, err = ResolveMCP(ctx, b.call, selection, contract.CWD); err != nil {
				return MCPRefusal("config/read", err)
			}
			receipt["mcpProfile"] = info
			launch["config"] = contract.Config()
		}
		created, err := b.dispatch(ctx, "thread/start", launch, effects)
		if err != nil {
			return err
		}
		threadID := id(created, "thread")
		receipt["threadId"] = threadID
		receipt["creation"] = created
		if watch, err = b.watchSubscription(ctx, threadID, true); err != nil {
			return err
		}
		if _, err = b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		observed := created
		if contract.MCP != nil {
			if observed, err = b.observeMCP(ctx, contract.MCP, threadID, created, receipt); err != nil {
				return err
			}
		}
		receipt["settings"] = contract.Receipt(observed, "creation")
		if _, err = b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		warnCreated(observed, in.Sandbox)
		if findings := contract.Findings(observed); len(findings) > 0 {
			first := findings[0]
			message := findingText(first.Code, first.Field, first.Returned, first.Expected) + "; initial prompt withheld. Inspect creation receipt. The thread remains retained."
			return &appserver.RPCError{Method: "thread/start", Message: message, Object: map[string]any{"code": first.Code, "message": message}}
		}
		// CRW-1313: the creator names the thread right after thread/start, by the thread title table, and a naming
		// failure is a recorded warning, never a refusal.
		name, warning := creationTitle(ctx, in.Role, in.Title, in.CWD)
		if warning != nil {
			recordTitleWarning(receipt, warning)
		}
		if name != "" {
			if err = b.nameThread(ctx, threadID, name, effects, receipt); err != nil {
				return err
			}
		}
		// The save right before turn/start, with no other save between them: it carries the naming's outcome, the name
		// (title) or the warning that the thread has none (titleWarning), and the reconciliation of an unknown managed
		// creation reads either one as a receipt that may have sent turn/start (savedBeforeFirstTurn in
		// internal/relay/managed/reconcile.go).
		if receipt["title"] != nil || receipt["titleWarning"] != nil {
			if _, err = b.Ledger.Save(ctx, receipt); err != nil {
				return err
			}
		}
		if in.Prompt != "" {
			turn, err := b.dispatch(ctx, "turn/start", map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": in.Prompt}}}, effects)
			if err != nil {
				return err
			}
			receipt["turnId"] = id(turn, "turn")
		}
		receipt["desktopProjectAssociation"] = "unverified; check Desktop listing"
		return nil
	}, legacy: createThreadLegacy(params, in.CWD)})
	if in.PromptRequired && errors.Is(err, ledger.ErrConflict) {
		return nil, b.conflictBeforePrompt(ctx, in, err)
	}
	return result, err
}

// createWithoutPrompt is a create_thread that must bring a prompt (PromptRequired) and brings none. It never begins an
// attempt; it may only replay a receipt that is not not_attempted. Under the mutation lock, right after the lookup,
// validate refuses the attempt, and Begin's own transaction refuses it again (refuseBegin), so a row another process
// changes in between begins nothing either. None of the request's checks run, because no attempt follows. The refusal
// is then answered by what the request_id holds (retireBeforePrompt): no receipt is ErrPromptRequired, with nothing
// reserved, and a not_attempted reservation, which is one made with exactly these arguments, so without a prompt, before
// prompts were required, is retired and answered ErrRetiredBeforePrompt. A replay of a receipt retired that way is
// answered ErrRetiredBeforePrompt too.
func (b *Bridge) createWithoutPrompt(ctx context.Context, in CreateThread) (ledger.Receipt, error) {
	params := createThreadParams(in)
	refuse := func() error { return errBeginsWithoutPrompt }
	result, err := b.mutate(ctx, mutation{requestID: in.RequestID, method: "create_thread", params: params, legacy: createThreadLegacy(params, in.CWD),
		validate: refuse, refuseBegin: func(ledger.Receipt) error { return refuse() }})
	switch {
	case errors.Is(err, errBeginsWithoutPrompt):
	case err == nil && retiredBeforePrompt(result):
		return nil, ErrRetiredBeforePrompt
	default:
		return result, err
	}
	retired, held, err := b.retireBeforePrompt(ctx, in)
	switch {
	case err != nil:
		return nil, err
	case retired:
		return nil, ErrRetiredBeforePrompt
	case held == nil:
		return nil, ErrPromptRequired
	}
	// Another attempt began on the reservation after the refusal: the call answers that receipt, as a replay.
	replay := copyMap(held)
	replay["replayed"] = true
	return replay, nil
}

// conflictBeforePrompt answers the conflict of a create_thread that must bring a prompt (PromptRequired) and brings one.
// When its request_id holds a reservation made with its arguments without a prompt, before prompts were required, the
// way on is a new request_id, and before that way is given the reservation is retired (retireBeforePrompt), so that no
// call on it, in this build or one that still takes a prompt-less create, creates a thread: ErrRetiredBeforePrompt. A
// failure to read the request_id is returned in place of the conflict, and every other row keeps the conflict.
func (b *Bridge) conflictBeforePrompt(ctx context.Context, in CreateThread, conflict error) error {
	retired, _, err := b.retireBeforePrompt(ctx, in)
	switch {
	case errors.Is(err, ledger.ErrConflict):
		return conflict
	case err != nil:
		return err
	case retired:
		return ErrRetiredBeforePrompt
	}
	return conflict
}

// retireBeforePrompt retires the not_attempted create_thread reservation on in.RequestID made with in's arguments
// without a prompt (in's own prompt is ignored), under the mutation lock and in one ledger transaction that writes only
// while the reservation is still exactly that (ledger.Retire): failed, an existing terminal status every build replays
// and never re-runs, with RetiredBeforePrompt as its error. A receipt with a host effect, a not_attempted reservation
// made with a prompt or other arguments, and a request_id no row holds are never written. It answers whether the
// reservation is retired, by this call or an earlier one, and the receipt the request_id holds (nil for none).
func (b *Bridge) retireBeforePrompt(ctx context.Context, in CreateThread) (bool, ledger.Receipt, error) {
	in.Prompt = ""
	params := createThreadParams(in)
	if b.waiting != nil {
		b.waiting(in.RequestID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	retired, held, err := b.Ledger.Retire(ctx, in.RequestID, "create_thread", params, createThreadLegacy(params, in.CWD), RetiredBeforePrompt)
	if err != nil {
		return false, nil, err
	}
	return retired || retiredBeforePrompt(held), held, nil
}

// retiredBeforePrompt reports whether receipt is a create_thread reservation retired by retireBeforePrompt: failed, with
// an error that starts with RetiredBeforePrompt. A replay of it carries the same two members.
func retiredBeforePrompt(receipt ledger.Receipt) bool {
	text, _ := receipt["error"].(string)
	return receipt["status"] == "failed" && strings.HasPrefix(text, RetiredBeforePrompt)
}

// createThreadParams is the params a create_thread request is fingerprinted by in the ledger, built from the request
// alone: the prompt is stored as null when there is none.
func createThreadParams(in CreateThread) map[string]any {
	params := map[string]any{"cwd": in.CWD, "sandbox": in.Sandbox, "approvalPolicy": "never", "ephemeral": false, "prompt": nullable(in.Prompt), "title": nullable(in.Title)}
	if in.Model != "" {
		params["model"] = in.Model
	}
	if in.ProjectID != "" {
		params["projectId"] = in.ProjectID
	}
	if in.Effort != "" {
		params["reasoning_effort"] = in.Effort
	}
	if in.Policy != nil {
		params["expected_sandbox_policy"] = in.Policy
	}
	if in.Roots != nil {
		roots := make([]any, len(in.Roots))
		for i, root := range in.Roots {
			roots[i] = root
		}
		params["runtime_workspace_roots"] = roots
	}
	if in.Exception != "" {
		params["policy_exception"] = in.Exception
	}
	if in.Role != "" {
		params["role"] = in.Role
	}
	if in.MCPProfile != "" {
		params["mcp_profile"] = in.MCPProfile
	}
	return params
}

// createThreadLegacy is the params of a version-1 (pre-upgrade) create_thread fingerprint: the same params with the cwd
// resolved through its symlinks, when it resolves.
func createThreadLegacy(params map[string]any, cwd string) func() map[string]any {
	return func() map[string]any {
		old := copyMap(params)
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
			old["cwd"] = resolved
		}
		return old
	}
}

// Reservation is what the ledger holds on a create_thread request_id, as CreateThreadReservation tells it.
type Reservation int

const (
	// ReservationNone: no row on the request_id.
	ReservationNone Reservation = iota
	// ReservationEffect: a row whose status is not not_attempted (the attempt began, it is a reservation made before
	// prompts were required and retired (RetiredBeforePrompt), or it is another operation's).
	ReservationEffect
	// ReservationPromptless: a not_attempted row stored with exactly this request's arguments and no prompt.
	ReservationPromptless
	// ReservationOther: a not_attempted row stored with other arguments than this request's without a prompt.
	ReservationOther
)

func (r Reservation) String() string {
	switch r {
	case ReservationNone:
		return "none"
	case ReservationEffect:
		return "effect"
	case ReservationPromptless:
		return "promptless"
	case ReservationOther:
		return "other"
	}
	return "unknown"
}

// CreateThreadReservation tells what the ledger holds on in.RequestID, compared with in's arguments without a prompt
// through the stored fingerprint (the same comparison, legacy fingerprint included, CreateThread's replay makes). in's
// own prompt is ignored. It only reads, without the mutation lock, so its answer may be out of date by the time a call
// acts on it: it can word a refusal, never allow anything, and CreateThread (PromptRequired) decides what a call does
// under the lock. A ledger read error is returned, never dropped.
func (b *Bridge) CreateThreadReservation(ctx context.Context, in CreateThread) (Reservation, error) {
	if b.Ledger == nil {
		return ReservationNone, nil
	}
	stored, err := b.Ledger.Get(ctx, in.RequestID)
	if errors.Is(err, ledger.ErrUnknown) {
		return ReservationNone, nil
	}
	if err != nil {
		return ReservationNone, err
	}
	if stored["status"] != "not_attempted" {
		return ReservationEffect, nil
	}
	in.Prompt = ""
	promptless := createThreadParams(in)
	receipt, err := b.Ledger.Lookup(ctx, in.RequestID, "create_thread", promptless, createThreadLegacy(promptless, in.CWD))
	switch {
	case errors.Is(err, ledger.ErrConflict):
		return ReservationOther, nil
	case err != nil:
		return ReservationNone, err
	case receipt == nil:
		return ReservationNone, nil
	case receipt["status"] != "not_attempted":
		// The row moved on between the two reads.
		return ReservationEffect, nil
	}
	return ReservationPromptless, nil
}
