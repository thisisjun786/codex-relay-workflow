package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ObserveTurn is installed by the production adapter; nil keeps the existing
// host-unavailable path. The offline receipt path remains unchanged.
var ObserveTurn func(context.Context, string, string, string, string) (string, error)

// cmdEmit is cmd_emit: the child's receipt, accepted, and queued when final.
func cmdEmit(c *cliRun) (any, error) {
	d, _, err := c.services()
	if err != nil {
		return nil, err
	}
	rid := c.s("--relationship")
	relationship, err := RequireActive(c.ctx, d.Store, rid)
	if err != nil {
		return nil, err
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
	thread, turn := c.s("--turn-thread"), c.s("--turn-id")
	payload := Obj{{Key: "eventId", Value: event}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: json.Number(strconv.FormatInt(generation, 10))}, {Key: "attempt", Value: json.Number(strconv.FormatInt(attempt, 10))},
		{Key: "revisionHash", Value: digest}, {Key: "outcome", Value: outcome}, {Key: "producer", Value: "child"},
		{Key: "turnRef", Value: Obj{{Key: "threadId", Value: thread}, {Key: "turnId", Value: turn}, {Key: "turnStatus", Value: status}}},
		{Key: "manifest", Value: manifest}, {Key: "emittedAt", Value: c.clock.ISO()}}
	if reference != "" {
		payload = append(payload, F{Key: "manifestRef", Value: reference})
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
		s := c.s("--supersedes-revision")
		options.SupersedesRevision = &s
	}
	intake := store.ReceiptIntake{Store: d.Store, Now: c.clock.ISO, Minimum: store.BestEffortDetection}
	stored, err := intake.AcceptChildReceiptWith(c.ctx, []byte(dumps(payload)), store.TurnReference{ThreadID: thread, TurnID: turn, Status: status}, options)
	if err != nil {
		return nil, withContinuationHint(err)
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
	return store.RefusedBecause(refused.Reason, refused.Detail+"; to continue this generation from this turn, re-run this emit with --continues-anchor "+pyvalue.StrRepr(need.Anchor)+" --continuation-actor <your own task id> --continuation-reason <why this turn continues it>", err)
}

// rowObj is dict(sqlite3.Row) of a deliveries row, in the table's column order.
func rowObj(row Row) Obj {
	var out Obj
	for _, column := range []string{"event_id", "relationship_id", "kind", "recipient_task_id", "recipient_thread_id", "state", "attempt_count", "next_eligible_at", "hold_reason", "lease_owner", "lease_until", "dispatch_evidence", "dispatch_turn_id", "provenance", "created_at", "updated_at"} {
		out = append(out, F{Key: column, Value: row.Opt(column)})
	}
	return out
}
