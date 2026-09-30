package hook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
func manifestEntries(v any) ([]store.ManifestEntry, error) {
	list, ok := evidence.List(v)
	if !ok {
		return nil, fmt.Errorf("TypeError: '%s' object is not iterable", evidence.TypeName(v))
	}
	out := []store.ManifestEntry{}
	for _, raw := range list {
		o, ok := evidence.Object(raw)
		if !ok {
			return nil, fmt.Errorf("TypeError: manifest entry is not an object")
		}
		for _, k := range []string{"path", "sha256"} {
			if _, ok := evidence.Lookup(o, k); !ok {
				return nil, fmt.Errorf("KeyError: %s", evidence.StrRepr(k))
			}
		}
		path, pok := get(o, "path").(string)
		digest, dok := get(o, "sha256").(string)
		if !pok || !dok {
			return nil, fmt.Errorf("TypeError: manifest path and digest must be strings")
		}
		entry := store.ManifestEntry{Path: path, SHA256: digest}
		if n := get(o, "bytes"); n != nil {
			size, ok := evidence.IntOf(n)
			if !ok {
				return nil, fmt.Errorf("TypeError: manifest bytes must be an integer")
			}
			entry.Bytes = &size
		}
		out = append(out, entry)
	}
	return out, nil
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
func DeliverableState(ctx context.Context, payload any, reference string, rootsValue any) (string, string, string) {
	o, ok := evidence.Object(payload)
	if !ok && evidence.Truthy(payload) {
		return "changed", "", "AttributeError: '" + evidence.TypeName(payload) + "' object has no attribute 'get'"
	}
	records, ok := evidence.List(get(o, "manifest"))
	if !ok || len(records) == 0 {
		return "changed", "", "the stored receipt carries no manifest to verify"
	}
	entries, err := manifestEntries(records)
	if err != nil {
		return "changed", "", err.Error()
	}
	revision, err := store.ManifestRevision(entries)
	if err != nil {
		return "unverifiable", "", "ScopeError: " + err.Error()
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
func verifyEntries(ctx context.Context, entries []store.ManifestEntry, roots []string) ([]string, []string) {
	problems, unreadable := []string{}, []string{}
	for _, e := range entries {
		digest, size, _, err := store.HashArtifactContext(ctx, e.Path, roots, false)
		message := ""
		switch {
		case err != nil:
			message = e.Path + ": " + err.Error()
			if accessFailure(err) {
				unreadable = append(unreadable, message)
			}
		case digest != e.SHA256:
			message = e.Path + ": bytes hash to " + digest + " but the manifest claims " + e.SHA256
		case e.Bytes != nil && *e.Bytes != size:
			message = fmt.Sprintf("%s: size %d but the manifest claims %d", e.Path, size, *e.Bytes)
		}
		if message != "" {
			problems = append(problems, message)
		}
	}
	return problems, unreadable
}

// verifyFrozen is manifest.verify_frozen_detailed as guard.deliverable_state reads it, under the
// hook's deadline and read bound: the problems, the subset that were failures to read, and what
// it raised instead of answering, as the state deliverable_state gives the exception
// ("unverifiable" for an OSError or a ScopeError, "changed" for a document that is not a
// manifest) and the exception's words. A raised exception is the whole answer: the fence's
// exception leaves before any live problem or unreadable live file is weighed.
func verifyFrozen(ctx context.Context, reference string, entries []store.ManifestEntry) ([]string, []string, string, string) {
	document := filepath.Join(reference, "MANIFEST.json")
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
	if err := store.FrozenDocumentError(raw); err != nil {
		var exception *store.FrozenException
		if errors.As(err, &exception) {
			return nil, nil, "changed", exception.PythonText()
		}
		return nil, nil, "changed", "ValueError: " + err.Error()
	}
	v, err := Decode(raw)
	if err != nil {
		return nil, nil, "changed", "JSONDecodeError: " + err.Error()
	}
	o, _ := evidence.Object(v)
	records := get(o, "entries")
	if _, ok := evidence.List(records); !ok {
		// The shape check passed, so this is an empty dict or str, which iterates to no record.
		records = []any{}
	}
	frozen, err := manifestEntries(records)
	if err != nil {
		return nil, nil, "changed", err.Error()
	}
	problems, unreadable := []string{}, []string{}
	claimed, stored := map[[2]string]bool{}, map[[2]string]bool{}
	sizes := map[string]*int64{}
	for _, e := range entries {
		claimed[[2]string{e.Path, e.SHA256}] = true
	}
	for _, e := range frozen {
		stored[[2]string{e.Path, e.SHA256}] = true
		sizes[e.Path] = e.Bytes
	}
	same := len(claimed) == len(stored)
	for k := range claimed {
		same = same && stored[k]
	}
	if !same {
		problems = append(problems, reference+": the frozen manifest does not describe the same deliverables")
	}
	for _, e := range entries {
		if s := sizes[e.Path]; e.Bytes != nil && s != nil && *s != *e.Bytes {
			problems = append(problems, fmt.Sprintf("%s: caller claims %d bytes but the frozen copy records %d", e.Path, *e.Bytes, *s))
		}
	}
	for _, e := range frozen {
		if len(e.SHA256) != 64 || strings.Trim(e.SHA256, "0123456789abcdef") != "" {
			problems = append(problems, e.Path+": "+evidence.StrRepr(e.SHA256)+" is not a digest")
			continue
		}
		digest, size, err := store.ReadFrozenBlob(ctx, filepath.Join(reference, "files"), e.SHA256)
		message := ""
		switch {
		case err != nil:
			message = e.Path + ": frozen bytes unreadable for " + e.SHA256 + ": " + err.Error()
			// A deleted blob is a broken snapshot (the pinned walk's answer), not a failure to look.
			var scope *store.RefusedError
			if !errors.As(err, &scope) || accessFailure(err) {
				unreadable = append(unreadable, message)
			}
		case digest != e.SHA256:
			message = e.Path + ": frozen bytes do not match " + e.SHA256
		case e.Bytes != nil && size != *e.Bytes:
			message = fmt.Sprintf("%s: frozen bytes are %d, not the claimed %d", e.Path, size, *e.Bytes)
		}
		if message != "" {
			problems = append(problems, message)
		}
	}
	// revision_hash(frozen) closes the fence's function, and a path in the frozen copy it will not
	// normalize raises there, after every problem was found.
	if _, err := store.ManifestRevision(frozen); err != nil {
		return nil, nil, "unverifiable", "ScopeError: " + err.Error()
	}
	return problems, unreadable, "", ""
}
