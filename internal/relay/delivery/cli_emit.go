package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ObserveTurn is installed by the production adapter; nil keeps the existing
// host-unavailable path. The offline receipt path remains unchanged.
var ObserveTurn func(context.Context, string, string, string, string) (string, error)

// EmitConfirmTurn is installed by the production adapter beside ObserveTurn. It reads a turn
// through the App Server socket the store recorded, read-only, and answers whether an exhausted
// listing holds it: found reports the turn in the listing, confirmed reports the read reached a
// definite answer (the listing was exhausted, or the turn was found). A build that leaves it nil,
// a store that records no socket, and a read that could not be made leave the receipt staged as
// before, because a check that could not be made is not evidence of absence (CRW-675,
// docs/relay/invariants.md I-218).
var EmitConfirmTurn func(ctx context.Context, state, socket, thread, turn string) (found, confirmed bool, err error)

// emitCodexID is the Codex id form: 36 characters of lowercase hex in 8-4-4-4-12 groups.
var emitCodexID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// emitRefuseTurnIDForm refuses a turn id that does not have the thread's Codex id form, before
// anything is written. A thread that is not a Codex id (a fixture's "thread-1") is not checked, so
// nothing but a Codex-form thread gains the rule. The refusal is the existing unassigned_turn: no
// new reason, no output field and no event (CRW-675).
func emitRefuseTurnIDForm(thread, turn string) error {
	if !emitCodexID.MatchString(thread) || emitCodexID.MatchString(turn) {
		return nil
	}
	return &store.RefusedError{Reason: "unassigned_turn", Detail: "turn " + strconv.Quote(turn) + " is not the Codex id form of the thread " + strconv.Quote(thread) + ": a Codex id is 36 lowercase hex digits in 8-4-4-4-12 groups, so take the id from the command output instead of retyping it"}
}

// emitConfirmTurn reads the turn read-only through the host the store recorded, and refuses
// unassigned_turn only when that host answered with an exhausted listing that does not hold it.
// A store that records no socket, a hook a build leaves nil, and a host that could not be reached
// or read leave the receipt to stage as before (CRW-675).
func emitConfirmTurn(c *cliRun, thread, turn string) error {
	if EmitConfirmTurn == nil || !emitCodexID.MatchString(thread) {
		return nil
	}
	socket := store.StoreSocket(c.state + "/relay.sqlite3")
	if socket == "" {
		return nil
	}
	found, confirmed, err := EmitConfirmTurn(c.ctx, c.state, socket, thread, turn)
	if err != nil || !confirmed {
		return nil
	}
	if found {
		return nil
	}
	return &store.RefusedError{Reason: "unassigned_turn", Detail: "turn " + strconv.Quote(turn) + " does not exist on " + strconv.Quote(thread) + ": the host's listing was exhausted and does not hold it, so take the current turn id from the command output and emit again"}
}

// cmdEmit is cmd_emit: the child's receipt, accepted, and queued when final.
func cmdEmit(c *cliRun) (any, error) {
	// CRW-675: a turn id that is not the thread's Codex id form is refused before anything is
	// written. The check reads only the arguments, so it runs before the store is opened: a refused
	// malformed turn id leaves no socket_path recorded (CRW-680). Without --socket, a turn the
	// store's recorded host answers is absent from an exhausted listing is refused the same way;
	// that check needs the socket the store recorded, so it stays after the store is opened. Both
	// refusals are the existing unassigned_turn.
	thread, turn := c.s("--turn-thread"), c.s("--turn-id")
	if err := emitRefuseTurnIDForm(thread, turn); err != nil {
		return nil, err
	}
	d, _, err := c.services()
	if err != nil {
		return nil, err
	}
	rid := c.s("--relationship")
	relationship, err := RequireActive(c.ctx, d.Store, rid)
	if err != nil {
		return nil, err
	}
	if c.socket == "" {
		if err := emitConfirmTurn(c, thread, turn); err != nil {
			return nil, err
		}
	}
	generation, _ := c.opt("--generation").(int64)
	attempt, _ := c.opt("--attempt").(int64)
	outcome := c.s("--outcome")
	var manifest any
	digest := store.NoDeliverable
	var entries []store.ManifestEntry
	if paths := c.list("--artifact"); len(paths) > 0 {
		if entries, err = store.BuildManifest(c.ctx, paths, relationship.ArtifactRoots); err != nil {
			return nil, err
		}
		if digest, err = store.ManifestRevision(entries); err != nil {
			return nil, err
		}
		records := make([]any, len(entries))
		for i, e := range entries {
			o := Obj{{Key: "path", Value: e.Path}, {Key: "sha256", Value: e.SHA256}}
			if e.Bytes != nil {
				o = append(o, F{Key: "bytes", Value: *e.Bytes})
			}
			records[i] = o
		}
		manifest = records
	}
	var independentReview Obj
	if path := c.s("--independent-review"); path != "" {
		if independentReview, err = readIndependentReview(path); err != nil {
			return nil, err
		}
	}
	reference := c.s("--manifest-ref")
	if reference != "" && len(entries) > 0 {
		if err := store.FreezeManifest(entries, reference); err != nil {
			return nil, err
		}
	}
	// With no --socket there is no host to ask: a readiness claim may only stage.
	status, proof := c.s("--turn-status"), "claimed"
	if c.socket != "" {
		if ObserveTurn == nil {
			return nil, dispatch.Host("this build registers no host adapter, so it cannot observe the turn")
		}
		status, err = ObserveTurn(c.ctx, c.state, c.socket, c.s("--turn-thread"), c.s("--turn-id"))
		if err != nil {
			return nil, err
		}
		proof = "host_observed"
	}
	if c.socket == "" && outcome == "ready_for_review" && status != "inProgress" {
		status, proof = "inProgress", "unverified_staged"
	}
	if generation < 1 {
		return nil, dispatch.Host("generation must be a positive integer")
	}
	if outcome == "ready_for_review" && digest == store.NoDeliverable {
		return nil, dispatch.Host("a reviewable receipt cannot carry the no-deliverable sentinel")
	}
	attemptNumber := int(attempt)
	event, err := store.EventID(rid, int(generation), digest, outcome, c.s("--turn-id"), &attemptNumber)
	if err != nil {
		return nil, dispatch.Host("event identity: " + err.Error())
	}
	payload := Obj{{Key: "eventId", Value: event}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: json.Number(strconv.FormatInt(generation, 10))}, {Key: "attempt", Value: json.Number(strconv.FormatInt(attempt, 10))},
		{Key: "revisionHash", Value: digest}, {Key: "outcome", Value: outcome}, {Key: "producer", Value: "child"},
		{Key: "turnRef", Value: Obj{{Key: "threadId", Value: thread}, {Key: "turnId", Value: turn}, {Key: "turnStatus", Value: status}}},
		{Key: "manifest", Value: manifest}, {Key: "emittedAt", Value: c.clock.ISO()}}
	if reference != "" {
		payload = append(payload, F{Key: "manifestRef", Value: reference})
	}
	if independentReview != nil {
		payload = append(payload, F{Key: "independentReview", Value: independentReview})
	}
	options := store.AcceptOptions{}
	if anchor := c.s("--continues-anchor"); anchor != "" {
		actor := c.s("--continuation-actor")
		if actor == "" {
			actor = thread
		}
		reason := c.s("--continuation-reason")
		if reason == "" {
			reason = "continuation of this execution"
		}
		options.Continuation = []byte(dumps(Obj{{Key: "anchorTurnId", Value: anchor}, {Key: "actor", Value: actor}, {Key: "reason", Value: reason}}))
	}
	if c.opt("--supersedes-revision") != nil {
		// Whatever the child names is recorded as it stated it, a suppressed receipt of this
		// generation included (CRW-470): a staged receipt can be suppressed after the child names it,
		// so no check here keeps the naming out; the head reads it through the suppressed receipt
		// (registry.ReadThrough), and refuses nothing the child could not have known.
		s := c.s("--supersedes-revision")
		options.SupersedesRevision = &s
	}
	intake := store.ReceiptIntake{Store: d.Store, Now: c.clock.ISO, Minimum: store.BestEffortDetection}
	stored, err := intake.AcceptChildReceiptWith(c.ctx, []byte(dumps(payload)), store.TurnReference{ThreadID: thread, TurnID: turn, Status: status}, options)
	if err != nil {
		return nil, withTurnStatusHint(withContinuationHint(err), outcome, status, proof)
	}
	receipt := Obj{}
	for _, f := range loadsObj(stored.Record) {
		if len(f.Key) > 0 && f.Key[0] == '_' || (f.Key == "manifestRef" || f.Key == "criteria") && f.Value == nil {
			continue
		}
		receipt = append(receipt, f)
	}
	result := Obj{{Key: "receipt", Value: receipt}, {Key: "stage", Value: stored.Stage}, {Key: "duplicate", Value: stored.Duplicate}, {Key: "terminalProof", Value: proof}, {Key: "observedTurnStatus", Value: status}}
	if stored.Stage == "final" {
		if err := d.AnnotatePredecessors(c.ctx, event); err != nil {
			return nil, err
		}
		existing, err := d.Find(c.ctx, event)
		if err != nil {
			return nil, err
		}
		if c.opt("--no-enqueue") != true && existing == nil {
			row, err := d.Enqueue(c.ctx, event, "", "")
			if err != nil {
				return nil, err
			}
			result = append(result, F{Key: "delivery", Value: rowObj(row)})
		}
	}
	return result, nil
}

// independentReviewCap bounds the file --independent-review reads: the item is a short statement,
// not the review artifact it names.
const independentReviewCap = 1 << 20

// readIndependentReview reads the file that holds the independentReview item: one JSON object. What
// the object says about the artifact is for the parent to grade; emit carries it and reads nothing
// the item names.
func readIndependentReview(path string) (Obj, error) {
	usage := func(detail string) error {
		return &dispatch.UsageError{Detail: "--independent-review: " + detail, Code: contract.ExitUsage}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, usage("the file could not be read: " + err.Error())
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, independentReviewCap+1))
	if err != nil {
		return nil, usage("the file could not be read: " + err.Error())
	}
	if len(raw) > independentReviewCap {
		return nil, usage("the file is larger than " + strconv.Itoa(independentReviewCap) + " bytes, and the item is a short statement that names the artifact rather than holding it")
	}
	value, err := loads(string(raw))
	if err == nil && !json.Valid(raw) {
		err = errors.New("text follows the value") // loads stops at a closing bracket; the file is one value
	}
	if err != nil {
		return nil, usage("the file is not JSON: " + err.Error())
	}
	item, isObject := value.(Obj)
	if !isObject {
		return nil, usage("the file must hold one JSON object, not " + quote.Kind(value))
	}
	return item, nil
}

// withContinuationHint adds to the refusal of a turn the generation never admitted, when the
// receipt carried no claim, how this command states the claim that would admit it. The reason,
// the exit code and the shape of the refusal are the refusal's own; a refusal a claim would not
// cure (a thread that is not the registered child, a claim naming another anchor, a generation
// with no anchor) carries no marker and is returned as it came.
func withContinuationHint(err error) error {
	var need *store.ContinuationRequired
	var refused *store.RefusedError
	if !errors.As(err, &need) || !errors.As(err, &refused) {
		return err
	}
	return store.RefusedBecause(refused.Reason, refused.Detail+"; to continue this generation from this turn, re-run this emit with --continues-anchor "+quote.Shell(need.Anchor)+" --continuation-actor <your own task id> --continuation-reason <why this turn continues it>", err)
}

// withTurnStatusHint adds to the refusal of an execution-only failed or interrupted receipt that
// states a turn still in progress, which cannot carry either outcome, the status to pass instead
// (CRW-505). --turn-status is "inProgress" when it is left out, and a child that read the refusal
// as "receipt not emitted" ended its turn with nothing sent. The reason, the exit code, the rule
// and the refusal's cause are the refusal's own; any other refusal is returned as it came.
// Without --socket the status is the child's claim, so the instruction is the matching status;
// with it the relay read the status from the host and ignored the flag, so the way out is the
// same emit without --socket (the child packet gives that form). Dropping --socket also drops the
// store the socket selected when the line names no --state, so the hint keeps the store named.
func withTurnStatusHint(err error, outcome, status, proof string) error {
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != store.ReasonContradictoryObservation || status != "inProgress" || (outcome != "failed" && outcome != "interrupted") {
		return err
	}
	hint := "; to report " + strconv.Quote(outcome) + " from your own live turn, re-run this emit with --turn-status " + outcome + " and nothing else changed (--turn-status defaults to \"inProgress\" when it is left out)"
	if proof == "host_observed" {
		hint = "; with --socket the relay reads the turn status from the host and ignores --turn-status, and the host reports this turn \"inProgress\": to report " + strconv.Quote(outcome) + " from your own live turn, re-run this emit without --socket, with --state naming the store this emit used (the socket no longer selects it), and with --turn-status " + outcome
	}
	return store.RefusedBecause(refused.Reason, refused.Detail+hint, err)
}

// rowObj is dict(sqlite3.Row) of a deliveries row, in the table's column order.
func rowObj(row Row) Obj {
	var out Obj
	for _, column := range []string{"event_id", "relationship_id", "kind", "recipient_task_id", "recipient_thread_id", "state", "attempt_count", "next_eligible_at", "hold_reason", "lease_owner", "lease_until", "dispatch_evidence", "dispatch_turn_id", "provenance", "created_at", "updated_at"} {
		out = append(out, F{Key: column, Value: row.Opt(column)})
	}
	return out
}
