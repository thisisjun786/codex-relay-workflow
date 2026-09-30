package hook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// LookupReceipt uses one read-only snapshot for relationship, generation and head.
// Artifact I/O starts only after that snapshot is closed.
func LookupReceipt(ctx context.Context, path string, fallback func() (string, error), relationship, session, turn, generation, dispatch any) (Object, bool, error) {
	if !delivery.Named(relationship) || !delivery.Named(session) || !delivery.Named(turn) {
		return nil, true, nil
	}
	if path == "" && fallback != nil {
		var err error
		path, err = fallback()
		if err != nil {
			return nil, false, err
		}
	}
	if path == "" {
		return nil, false, nil
	}
	timeout := 250 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, max(0, time.Until(deadline)))
	}
	ro, err := store.OpenReadOnly(ctx, path, timeout)
	if errors.Is(err, store.ErrLiveState) {
		// The live-state guard (until todo 43) is a refusal to report, never a store that
		// merely reads as unreadable.
		return nil, false, err
	}
	if err != nil {
		return nil, false, nil
	}
	defer ro.Close()
	if _, err = ro.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return nil, false, nil
	}
	rollback := func() error { _, err := ro.ExecContext(ctx, "ROLLBACK"); return err }
	defer func() { _ = rollback() }()
	base := Object{{Key: "relationshipId", Value: relationship}, {Key: "sessionId", Value: session}, {Key: "turnId", Value: turn}, {Key: "atCurrentHead", Value: false}}
	answer := func(ev string, extra ...Field) (Object, bool, error) {
		o := set(base, "evidence", ev)
		return append(o, extra...), true, nil
	}
	var status, roots string
	var superseded sql.NullString
	var current int64
	err = ro.QueryRowContext(ctx, "SELECT status, superseded_by, execution_generation, artifact_roots FROM relationships WHERE relationship_id = ?", relationship).Scan(&status, &superseded, &current, &roots)
	if errors.Is(err, sql.ErrNoRows) {
		return answer("relationship_absent")
	}
	if err != nil {
		return nil, false, nil
	}
	if status != "active" || (superseded.Valid && superseded.String != "") {
		return answer("relationship_not_active")
	}
	if generation != nil {
		n, ok := evidence.IntOf(generation)
		if !ok || n != current {
			return answer("registration_generation_mismatch", Field{Key: "detail", Value: "the assignment registered generation " + evidence.Text(generation) + " and the relationship now stands on generation " + fmt.Sprint(current)})
		}
	}
	if dispatch != nil {
		var opened sql.NullString
		err = ro.QueryRowContext(ctx, "SELECT dispatch_request_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", relationship, current).Scan(&opened)
		if errors.Is(err, sql.ErrNoRows) {
			return answer("generation_absent", Field{Key: "detail", Value: "the relationship reports generation " + fmt.Sprint(current) + " and the store holds no record of which dispatch opened it"})
		}
		if err != nil {
			return nil, false, nil
		}
		if !delivery.SameIdentity(opened.String, dispatch) {
			return answer("generation_dispatch_mismatch", Field{Key: "detail", Value: "the relationship stands on generation " + fmt.Sprint(current) + ", which a different dispatch request opened"})
		}
	}
	head, err := delivery.HeadRevisionFrom(ctx, ro, text(relationship), current)
	if err != nil {
		return nil, false, nil
	}
	if slices.Contains([]string{delivery.Fork, delivery.Cycle, delivery.UnknownPredecessor, delivery.Disconnected}, text(get(head, "evidence"))) {
		return answer("head_" + text(get(head, "evidence")))
	}
	if !evidence.Truthy(get(head, "eventId")) {
		return answer("no_reviewable_revision")
	}
	var id, stage, producer, payload string
	var revision, thread, eventTurn, manifest sql.NullString
	err = ro.QueryRowContext(ctx, "SELECT event_id, stage, revision_hash, turn_thread_id, turn_id, producer, receipt, manifest_ref FROM events WHERE event_id = ?", get(head, "eventId")).Scan(&id, &stage, &revision, &thread, &eventTurn, &producer, &payload, &manifest)
	if rollbackErr := rollback(); rollbackErr != nil {
		return nil, false, nil
	}
	if err == sql.ErrNoRows {
		return answer("no_reviewable_revision")
	}
	if err != nil {
		return nil, false, nil
	}
	if producer != "child" || (stage != "staged" && stage != "final") {
		return answer("head_is_not_a_child_receipt")
	}
	if !delivery.SameIdentity(thread.String, session) || !delivery.SameIdentity(eventTurn.String, turn) {
		return answer("head_belongs_to_another_turn")
	}
	rv, re := Decode([]byte(roots))
	pv, pe := Decode([]byte(payload))
	if re != nil || pe != nil {
		e := re
		if e == nil {
			e = pe
		}
		return answer("stored_receipt_unreadable", Field{Key: "detail", Value: "JSONDecodeError: " + e.Error()}, Field{Key: "eventId", Value: id})
	}
	state, binding, detail := DeliverableState(ctx, pv, manifest.String, rv)
	if state == "unverifiable" {
		return append(set(base, "evidence", "deliverable_unverifiable"), Field{Key: "detail", Value: nullable(detail)}), false, nil
	}
	if state == "changed" {
		return answer("artifacts_changed_since_receipt", Field{Key: "detail", Value: nullable(detail)}, Field{Key: "eventId", Value: id}, Field{Key: "revisionHash", Value: nullable(revision.String)})
	}
	base = set(base, "atCurrentHead", true)
	return answer("at_head", Field{Key: "eventId", Value: id}, Field{Key: "revisionHash", Value: nullable(revision.String)}, Field{Key: "stage", Value: stage}, Field{Key: "deliverableBinding", Value: binding})
}
func accessFailure(err error) bool {
	var refused *store.RefusedError
	if errors.As(err, &refused) && refused.Reason == store.ReasonUnverifiablePathBinding {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno != syscall.ELOOP && errno != syscall.ENOTDIR && errno != syscall.ENOENT && errno != syscall.ESTALE
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// DeliverableState is guard.deliverable_state. The stored receipt's records and the frozen copy's
// are read as the values json.loads made of them (store.PythonManifestEntries), so a path, digest
// or byte count of another type is compared, printed and refused as the fence does, and an
// exception the fence raises is answered as its except clauses answer it (raisedState).
func DeliverableState(ctx context.Context, payload any, reference string, rootsValue any) (string, string, string) {
	o, ok := evidence.Object(payload)
	if !ok && evidence.Truthy(payload) {
		return "changed", "", "AttributeError: '" + evidence.TypeName(payload) + "' object has no attribute 'get'"
	}
	records, ok := evidence.List(get(o, "manifest"))
	if !ok || len(records) == 0 {
		return "changed", "", "the stored receipt carries no manifest to verify"
	}
	entries, err := store.PythonManifestEntries(records)
	if err != nil {
		return raisedState(err)
	}
	revision, err := store.PythonRevisionHash(entries)
	if err != nil {
		return raisedState(err)
	}
	claimed := get(o, "revisionHash")
	if !evidence.Truthy(claimed) {
		return "changed", "", "the stored receipt names no revision"
	}
	if claimed != revision {
		return "changed", "", "the stored manifest hashes to " + revision + " but the receipt claims " + evidence.Text(claimed)
	}
	rootsList, ok := evidence.List(rootsValue)
	if !ok {
		return "changed", "", "TypeError: artifact roots are not a list"
	}
	roots := []string{}
	for _, r := range rootsList {
		s, ok := r.(string)
		if !ok {
			return "changed", "", "TypeError: artifact root is not a string"
		}
		roots = append(roots, s)
	}
	problems, unreadable := verifyEntries(ctx, entries, roots)
	if len(problems) == 0 {
		return "current", "live", ""
	}
	if reference != "" {
		frozen, unreachable, raisedState, raised := verifyFrozen(ctx, reference, entries)
		if raisedState != "" {
			return raisedState, "", raised
		}
		if len(frozen) == 0 {
			return "current", "frozen", ""
		}
		if len(unreachable) > 0 {
			return "unverifiable", "", strings.Join(unreachable[:min(3, len(unreachable))], "; ")
		}
		problems = append(problems, frozen...)
	}
	if len(unreadable) > 0 {
		return "unverifiable", "", strings.Join(unreadable[:min(3, len(unreadable))], "; ")
	}
	return "changed", "", strings.Join(problems[:min(3, len(problems))], "; ")
}

// raisedState is guard.deliverable_state's except clauses: an OSError or a ScopeError could not
// look (unverifiable), and any other exception looked and found a record that is not the shape a
// receipt is (changed), each named f"{type(error).__name__}: {error}".
func raisedState(err error) (string, string, string) {
	var exception *store.ManifestException
	if errors.As(err, &exception) {
		if exception.OSError() {
			return "unverifiable", "", exception.PythonText()
		}
		return "changed", "", exception.PythonText()
	}
	return "unverifiable", "", "ScopeError: " + err.Error()
}

// verifyEntries is manifest.verify_against_disk_detailed over entries revision_hash accepted, so
// each path and digest is a str; a byte count is compared as the value it is.
func verifyEntries(ctx context.Context, entries []store.PythonEntry, roots []string) ([]string, []string) {
	problems, unreadable := []string{}, []string{}
	for _, e := range entries {
		path, _ := e.Path.(string)
		claimed, _ := e.SHA256.(string)
		digest, size, _, err := store.HashArtifactContext(ctx, path, roots, false)
		message := ""
		switch {
		case err != nil:
			message = path + ": " + err.Error()
			if accessFailure(err) {
				unreadable = append(unreadable, message)
			}
		case digest != claimed:
			message = path + ": bytes hash to " + digest + " but the manifest claims " + claimed
		case e.Bytes != nil && !store.PythonEqual(e.Bytes, size):
			message = fmt.Sprintf("%s: size %d but the manifest claims %s", path, size, store.PythonStr(e.Bytes))
		}
		if message != "" {
			problems = append(problems, message)
		}
	}
	return problems, unreadable
}

// verifyFrozen is manifest.verify_frozen_detailed as guard.deliverable_state reads it, under the
// hook's deadline and read bound: the problems, the subset that were failures to read, and what
// it raised instead of answering, as the state deliverable_state gives the exception and the
// exception's words (raisedState). A raised exception is the whole answer: the fence's exception
// leaves before any live problem or unreadable live file is weighed. The document is the path
// pathlib spells (store.FrozenDocument), and what follows its read is the store's own reading of
// a frozen copy (store.VerifyFrozenDocument), so the hook, the intake and the omission reader
// judge one frozen copy alike.
func verifyFrozen(ctx context.Context, reference string, entries []store.PythonEntry) ([]string, []string, string, string) {
	document := store.FrozenDocument(reference)
	if strings.ContainsRune(document, 0) {
		// os.stat refuses the name before any system call; the fence lets that ValueError out.
		return nil, nil, "changed", "ValueError: embedded null byte"
	}
	// _frozen_document_access: absent, or out of reach. Only the second is a failure to read.
	if info, err := os.Stat(document); err != nil || !info.Mode().IsRegular() {
		message := reference + ": no MANIFEST.json in the frozen copy"
		if err != nil && accessFailure(err) {
			return []string{message}, []string{reference + ": the frozen manifest could not be reached: " + store.PythonOSErrorText(err)}, "", ""
		}
		return []string{message}, nil, "", ""
	}
	raw, err := readRegular(ctx, document, maxInputBytes)
	if err != nil {
		// Reached and not read: the fence raises the OSError, a comparison that did not happen.
		return nil, nil, "unverifiable", store.PythonOSError(err)
	}
	_, problems, unreadable, err := store.VerifyFrozenDocument(ctx, reference, raw, entries)
	if err != nil {
		state, _, detail := raisedState(err)
		return nil, nil, state, detail
	}
	return problems, unreadable, "", ""
}
