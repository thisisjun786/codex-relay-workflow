package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// The one reading of a stored receipt that the Stop hook (hook.LookupReceipt) and the omission
// reader (delivery.ObserveOmission, delivery.DeriveOmission) share, through delivery.LookupStoredReceipt
// and LookupStoredReceiptAt. Both read a head event's receipt and its relationship's
// artifact roots as the values json.loads makes of them, and judge them by
// guard.deliverable_state's rule, so a path, digest or byte count of another type is compared,
// printed and refused as the guard refuses it. Only text that is not JSON is unreadable.

// DeliverableState's answers.
const (
	// DeliverableCurrent: the artifacts still hash to the revision the receipt claims, live or
	// through the frozen copy (the binding says which).
	DeliverableCurrent = "current"
	// DeliverableChanged: the comparison happened and the deliverable is not the one described,
	// or the stored record is not the shape a receipt is.
	DeliverableChanged = "changed"
	// DeliverableUnverifiable: the comparison could not happen (an unreachable file or frozen copy,
	// a path the revision hash refuses, a cancelled context). Never folded into changed.
	DeliverableUnverifiable = "unverifiable"
)

// storedValues is how a stored record is read: Python's values (pyjson.Loads), objects in order,
// an integer an int64 (a json.Number past it), NaN and the infinities floats and a lone surrogate
// escape kept, with json.loads' own refusals. The bytes are strict UTF-8 first (DecodeRecord), so
// no lone surrogate arrives unescaped.
var storedValues = pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.Int64Numbers}

// DecodeRecord is json.loads over bytes: strict UTF-8 (its UnicodeDecodeError), then json.loads'
// language and refusals (its JSONDecodeError text), without panic paths. hook.Decode is this.
func DecodeRecord(raw []byte) (any, error) {
	if _, err := pyjson.DecodeUTF8(raw); err != nil {
		return nil, err
	}
	return pyjson.Loads(string(raw), storedValues)
}

// DecodeStoredReceipt reads a relationship's artifact_roots text and a head event's receipt text,
// in that order. When either is not JSON, unreadable is the detail a reader keeps with its
// stored_receipt_unreadable answer ("JSONDecodeError: " and the first refusal's words); otherwise
// it is empty and both values are the decoded documents.
func DecodeStoredReceipt(rootsText, payloadText string) (roots, payload any, unreadable string) {
	roots, rootsErr := DecodeRecord([]byte(rootsText))
	payload, payloadErr := DecodeRecord([]byte(payloadText))
	if rootsErr != nil || payloadErr != nil {
		err := rootsErr
		if err == nil {
			err = payloadErr
		}
		return nil, nil, "JSONDecodeError: " + err.Error()
	}
	return roots, payload, ""
}

// DeliverableState is guard.deliverable_state. The stored receipt's records and the frozen copy's
// are read as the values json.loads made of them (FrozenManifestEntries), so a path, digest or byte
// count of another type is compared, printed and refused as the fence does, and an exception the
// fence raises is answered as its except clauses answer it (raisedState). The error is the
// exception those clauses do not name (a *ManifestException whose RuntimeError holds), which leaves
// deliverable_state and lookup_receipt, so the Stop hook faults on it and the omission reader leaves
// the omission unmeasured as evidence_unreadable.
//
// ctx bounds the artifact hashing and the frozen copy's read: a comparison it cuts off did not
// happen, so it is unverifiable, never changed.
func DeliverableState(ctx context.Context, payload any, reference string, rootsValue any) (string, string, string, error) {
	o, ok := payload.(contract.OrderedObject)
	if !ok && pyvalue.Truthy(payload) {
		return DeliverableChanged, "", "AttributeError: '" + pyvalue.TypeName(payload) + "' object has no attribute 'get'", nil
	}
	manifest, _ := pythonGet(o, "manifest")
	records, ok := manifest.([]any)
	if !ok || len(records) == 0 {
		return DeliverableChanged, "", "the stored receipt carries no manifest to verify", nil
	}
	entries, err := FrozenManifestEntries(records)
	if err != nil {
		return raisedState(err)
	}
	revision, err := FrozenRevisionHash(entries)
	if err != nil {
		return raisedState(err)
	}
	claimed, _ := pythonGet(o, "revisionHash")
	if !pyvalue.Truthy(claimed) {
		return DeliverableChanged, "", "the stored receipt names no revision", nil
	}
	if claimed != revision {
		return DeliverableChanged, "", "the stored manifest hashes to " + revision + " but the receipt claims " + pyvalue.Str(claimed), nil
	}
	rootsList, ok := rootsValue.([]any)
	if !ok {
		return DeliverableChanged, "", "TypeError: artifact roots are not a list", nil
	}
	roots := []string{}
	for _, r := range rootsList {
		s, ok := r.(string)
		if !ok {
			return DeliverableChanged, "", "TypeError: artifact root is not a string", nil
		}
		roots = append(roots, s)
	}
	problems, unreadable := verifyEntries(ctx, entries, roots)
	if len(problems) == 0 {
		return DeliverableCurrent, "live", "", nil
	}
	if reference != "" {
		frozen, unreached, err := verifyFrozen(ctx, reference, entries)
		if err != nil {
			return raisedState(err)
		}
		if len(frozen) == 0 {
			return DeliverableCurrent, "frozen", "", nil
		}
		if len(unreached) > 0 {
			return DeliverableUnverifiable, "", joinFirst(unreached), nil
		}
		problems = append(problems, frozen...)
	}
	if len(unreadable) > 0 {
		return DeliverableUnverifiable, "", joinFirst(unreadable), nil
	}
	return DeliverableChanged, "", joinFirst(problems), nil
}

func joinFirst(messages []string) string { return strings.Join(messages[:min(3, len(messages))], "; ") }

// raisedState is guard.deliverable_state's except clauses: an OSError or a ScopeError could not
// look (unverifiable), and a ValueError, KeyError, TypeError or AttributeError looked and found a
// record that is not the shape a receipt is (changed), each named f"{type(error).__name__}:
// {error}". A RecursionError is none of those, so it is returned, to leave deliverable_state.
func raisedState(err error) (string, string, string, error) {
	var answered *frozenAnswer
	if errors.As(err, &answered) {
		return answered.state, "", answered.detail, nil
	}
	var exception *ManifestException
	if errors.As(err, &exception) {
		switch {
		case exception.RuntimeError():
			return "", "", "", exception
		case exception.OSError():
			return DeliverableUnverifiable, "", exception.StoredText(), nil
		}
		return DeliverableChanged, "", exception.StoredText(), nil
	}
	return DeliverableUnverifiable, "", "ScopeError: " + err.Error(), nil
}

// frozenAnswer is an exception the read of a frozen MANIFEST.json raises before VerifyFrozenDocument
// reads the document, with the state guard.deliverable_state's except clauses give it.
type frozenAnswer struct{ state, detail string }

func (e *frozenAnswer) Error() string { return e.detail }

// unreachable is manifest's access-failure rule as the Stop hook applies it under its deadline: an
// answer about the path (absent, not a directory, a loop, stale) is not a failure to look, a path
// whose binding could not be verified or any other errno is, and so is a context that ended.
// VerifyAgainstDiskDetailed and VerifyFrozenDetailed, which the omission reader used to call, keep
// their own accessFailure, which has no context to end.
func unreachable(err error) bool {
	var refused *RefusedError
	if errors.As(err, &refused) && refused.Reason == ReasonUnverifiablePathBinding {
		return true
	}
	var errno unix.Errno
	if errors.As(err, &errno) {
		return errno != unix.ELOOP && errno != unix.ENOTDIR && errno != unix.ENOENT && errno != unix.ESTALE
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// verifyEntries is manifest.verify_against_disk_detailed over entries FrozenRevisionHash accepted,
// so each path and digest is a str; a byte count is compared as the value it is. It is the
// context-aware twin of VerifyAgainstDiskDetailed, which compares typed entries.
func verifyEntries(ctx context.Context, entries []FrozenEntry, roots []string) ([]string, []string) {
	problems, unreadable := []string{}, []string{}
	for _, e := range entries {
		path, _ := e.Path.(string)
		claimed, _ := e.SHA256.(string)
		digest, size, _, err := HashArtifactContext(ctx, path, roots, false)
		message := ""
		switch {
		case err != nil:
			message = path + ": " + err.Error()
			if unreachable(err) {
				unreadable = append(unreadable, message)
			}
		case digest != claimed:
			message = path + ": bytes hash to " + digest + " but the manifest claims " + claimed
		case e.Bytes != nil && !pyvalue.Equal(e.Bytes, size):
			message = fmt.Sprintf("%s: size %d but the manifest claims %s", path, size, pyvalue.Str(e.Bytes))
		}
		if message != "" {
			problems = append(problems, message)
		}
	}
	return problems, unreadable
}

// verifyFrozen is manifest.verify_frozen_detailed as guard.deliverable_state reads it, under the
// caller's deadline: the problems, the subset that were failures to read, or what it raised instead
// of answering, which raisedState answers as deliverable_state does. The document is read whole, as
// the fence reads it, so no byte bound applies to it. A raised exception is the whole answer: the
// fence's exception leaves before any live problem or unreadable live file is weighed. The document
// is the path pathlib spells (FrozenDocument), and what follows its read is the store's own reading
// of a frozen copy (VerifyFrozenDocument), so the hook, the intake and the omission reader judge
// one frozen copy alike. It is the context-aware twin of VerifyFrozenDetailed.
func verifyFrozen(ctx context.Context, reference string, entries []FrozenEntry) ([]string, []string, error) {
	document := FrozenDocument(reference)
	if strings.ContainsRune(document, 0) {
		// os.stat refuses the name before any system call; the fence lets that ValueError out.
		return nil, nil, &frozenAnswer{DeliverableChanged, "ValueError: embedded null byte"}
	}
	// _frozen_document_access: absent, or out of reach. Only the second is a failure to read.
	if info, err := os.Stat(document); err != nil || !info.Mode().IsRegular() {
		message := reference + ": no MANIFEST.json in the frozen copy"
		if err != nil && unreachable(err) {
			return []string{message}, []string{reference + ": the frozen manifest could not be reached: " + StoredOSErrorText(err)}, nil
		}
		return []string{message}, nil, nil
	}
	raw, err := readWhole(ctx, document)
	if err != nil {
		// Reached and not read: the fence raises the OSError, a comparison that did not happen.
		return nil, nil, &frozenAnswer{DeliverableUnverifiable, StoredOSError(err)}
	}
	_, problems, unreadable, err := VerifyFrozenDocument(ctx, reference, raw, entries)
	if err != nil {
		return nil, nil, err
	}
	return problems, unreadable, nil
}

// readWhole reads a regular file within ctx with no byte bound: the context is checked before the
// open and after the read, a path that is not a regular file is refused before and after the open,
// and a FIFO is never waited on (O_NONBLOCK). It is hook.readRegular with its limit unbounded; the
// hook keeps that function for the evidence files it bounds.
func readWhole(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return raw, ctx.Err()
}
