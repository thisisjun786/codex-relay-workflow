package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// VerifyFrozenDetailed is manifest.verify_frozen_detailed: the revision the frozen copy's own
// manifest hashes to, every problem found, and the subset of them that were failures to read.
// A frozen MANIFEST.json that is absent, is not a regular file or cannot be reached is a
// problem; which of those was an access failure is asked of stat as the fence asks it
// (_frozen_document_access). A document that was reached and could not be read, or was read and
// is not a manifest, is the exception the fence raises instead of an answer (ManifestException,
// or the ScopeError revision_hash raises as a RefusedError).
func VerifyFrozenDetailed(reference string, entries []ManifestEntry) (string, []string, []string, error) {
	document := FrozenDocument(reference)
	if strings.ContainsRune(document, 0) {
		// os.stat refuses the name before any system call, and the probe lets that out.
		return "", nil, nil, &ManifestException{Class: "ValueError", text: "embedded null byte"}
	}
	info, err := os.Stat(document)
	if err != nil || !info.Mode().IsRegular() {
		problems, unreadable := []string{reference + ": no MANIFEST.json in the frozen copy"}, []string{}
		if err != nil && !interpretedErrno(err) {
			unreadable = append(unreadable, reference+": the frozen manifest could not be reached: "+PythonOSErrorText(err))
		}
		return "", problems, unreadable, nil
	}
	raw, err := os.ReadFile(document)
	if err != nil {
		return "", nil, nil, frozenOSException(err)
	}
	return VerifyFrozenDocument(context.Background(), reference, raw, PythonEntries(entries))
}

// FrozenDocument is str(Path(reference) / "MANIFEST.json").
func FrozenDocument(reference string) string { return frozenPath(reference, "MANIFEST.json") }

// frozenPath is str(Path(reference).joinpath(*names)). pathlib drops '.' parts, repeated and
// trailing slashes, and keeps exactly two leading slashes and every '..', which the kernel then
// resolves after the symlink before it. Cleaning the '..' away here would name another directory
// whenever the component before it is a symlink or does not exist.
func frozenPath(reference string, names ...string) string {
	root := ""
	switch {
	case strings.HasPrefix(reference, "//") && !strings.HasPrefix(reference, "///"):
		root = "//"
	case strings.HasPrefix(reference, "/"):
		root = "/"
	}
	parts := []string{}
	for _, part := range strings.Split(reference, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return root + strings.Join(append(parts, names...), "/")
}

// VerifyFrozenDocument is verify_frozen_detailed from json.loads(document.read_text()) on, over
// the MANIFEST.json bytes a caller has read: each frozen record's path, digest and bytes are the
// values json.loads made of them, and each step raises what the fence raises there. The frozen
// set is hashed (a TypeError for a list or dict path or digest), a digest is matched (a TypeError
// for a true value that is not a str, a problem for a false one), each blob is read within ctx,
// and revision_hash closes, raising after every problem was found. entries are the caller's,
// with a str path and digest each; nil is the fence's None and skips the comparison with them.
func VerifyFrozenDocument(ctx context.Context, reference string, raw []byte, entries []PythonEntry) (string, []string, []string, error) {
	frozen, err := frozenRecords(raw)
	if err != nil {
		return "", nil, nil, err
	}
	problems, unreadable := []string{}, []string{}
	if entries != nil {
		for _, entry := range frozen {
			for _, field := range []any{entry.Path, entry.SHA256} {
				if !pythonHashable(field) {
					return "", nil, nil, &ManifestException{Class: "TypeError", text: "unhashable type: '" + pythonTypeName(field) + "'"}
				}
			}
		}
		claimed, stored := map[[2]string]bool{}, map[[2]string]bool{}
		for _, entry := range entries {
			path, _ := entry.Path.(string)
			digest, _ := entry.SHA256.(string)
			claimed[[2]string{path, digest}] = true
		}
		// Only a str path and digest can equal a caller's, so any other frozen pair makes the
		// two sets differ, and only a str path can be looked up by one.
		same := true
		sizes := map[string]any{}
		for _, entry := range frozen {
			path, pathIsText := entry.Path.(string)
			digest, digestIsText := entry.SHA256.(string)
			if pathIsText && digestIsText {
				stored[[2]string{path, digest}] = true
			} else {
				same = false
			}
			if pathIsText {
				sizes[path] = entry.Bytes
			}
		}
		same = same && len(claimed) == len(stored)
		for key := range claimed {
			same = same && stored[key]
		}
		if !same {
			problems = append(problems, reference+": the frozen manifest does not describe the same deliverables")
		}
		for _, entry := range entries {
			path, _ := entry.Path.(string)
			if size := sizes[path]; entry.Bytes != nil && size != nil && !PythonEqual(size, entry.Bytes) {
				problems = append(problems, fmt.Sprintf("%s: caller claims %s bytes but the frozen copy records %s", path, PythonStr(entry.Bytes), PythonStr(size)))
			}
		}
	}
	for _, entry := range frozen {
		// DIGEST_RE.match(entry.sha256 or ""): a false value is matched as "", a true one that is
		// not a str is refused by re itself.
		var candidate any = ""
		if pythonTruthy(entry.SHA256) {
			candidate = entry.SHA256
		}
		digest, ok := candidate.(string)
		if !ok {
			return "", nil, nil, &ManifestException{Class: "TypeError", text: "expected string or bytes-like object, got '" + pythonTypeName(candidate) + "'"}
		}
		if !lowerDigest.MatchString(digest) {
			problems = append(problems, PythonStr(entry.Path)+": "+pythonReprValue(entry.SHA256)+" is not a digest")
			continue
		}
		hashed, size, err := ReadFrozenBlob(ctx, reference, digest)
		if err != nil {
			message := PythonStr(entry.Path) + ": frozen bytes unreadable for " + digest + ": " + err.Error()
			problems = append(problems, message)
			// A deleted blob is the pinned walk's answer about a broken snapshot (a ScopeError),
			// not a failure to look; anything else is.
			var scope *RefusedError
			if !errors.As(err, &scope) || accessFailure(err) {
				unreadable = append(unreadable, message)
			}
			continue
		}
		if hashed != digest {
			problems = append(problems, PythonStr(entry.Path)+": frozen bytes do not match "+digest)
		} else if entry.Bytes != nil && !PythonEqual(entry.Bytes, size) {
			problems = append(problems, fmt.Sprintf("%s: frozen bytes are %d, not the claimed %s", PythonStr(entry.Path), size, PythonStr(entry.Bytes)))
		}
	}
	revision, err := PythonRevisionHash(frozen)
	if err != nil {
		return "", nil, nil, err
	}
	return revision, problems, unreadable, nil
}

// frozenJSONDepth is how many nested containers json.loads's C scanner reads where the fence reads
// a frozen MANIFEST.json: the recursion budget left at that call when the installed console script
// runs the relay (decision 44), the boundary every other Go reader of a fence JSON document keeps.
const frozenJSONDepth = 9998

// frozenRecords is [Entry.from_record(record) for record in json.loads(text)["entries"]] over the
// MANIFEST.json bytes, text being what Path.read_text() makes of them: a UnicodeDecodeError, then
// universal newlines, then a JSONDecodeError (or the ValueError of an integer too long to read,
// or the RecursionError of a document nested deeper than frozenJSONDepth), then the KeyError or
// TypeError of reading payload["entries"] and each record's path and digest.
func frozenRecords(raw []byte) ([]PythonEntry, error) {
	text, err := DecodeUTF8(raw)
	if err != nil {
		// Its positions count the bytes before any newline is translated, as the fence's do.
		return nil, &ManifestException{Class: "UnicodeDecodeError", text: err.Error(), cause: err}
	}
	// read_text() opens the file in text mode, so a CRLF or a lone CR is one line feed by the time
	// json.loads counts lines, columns and characters to say where a document stops being JSON.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if message, recursion := PythonJSONErrorWithLimit(text, frozenJSONDepth); message != "" {
		class := "JSONDecodeError"
		switch {
		case recursion:
			class = "RecursionError"
		case strings.HasPrefix(message, "Exceeds the limit ("):
			// int() refuses an over-long integer literal with a plain ValueError.
			class = "ValueError"
		}
		return nil, &ManifestException{Class: class, text: message}
	}
	document, err := decodePythonJSON(text)
	if err != nil {
		return nil, fmt.Errorf("frozen manifest json.loads accepted: %w", err)
	}
	entries, err := pythonSubscript(document, "entries")
	if err != nil {
		return nil, err
	}
	var records []any
	switch value := entries.(type) {
	case []any:
		records = value
	case contract.OrderedObject:
		// Iterating a dict yields its keys, each a str the record subscript refuses.
		if len(value) > 0 {
			records = []any{""}
		}
	case string:
		if value != "" {
			records = []any{""}
		}
	default:
		return nil, &ManifestException{Class: "TypeError", text: "'" + pythonTypeName(entries) + "' object is not iterable"}
	}
	return PythonManifestEntries(records)
}

// ManifestException is an exception the fence raises while reading a manifest instead of
// answering: verify_frozen_detailed's frozen MANIFEST.json, reached and not read (an OSError),
// read and not a manifest (UnicodeDecodeError, JSONDecodeError, KeyError, TypeError), or nested
// deeper than json.loads descends (RecursionError), and the records and revision it and
// guard.deliverable_state read (TypeError, AttributeError, UnicodeEncodeError). Error is
// str(exception) and Class its type, which the fence's host envelope and guard.deliverable_state
// name.
type ManifestException struct {
	Class string
	text  string
	cause error
}

func (e *ManifestException) Error() string { return e.text }
func (e *ManifestException) Unwrap() error { return e.cause }

// OSError reports an OSError: a manifest that is there and that nobody could read, which
// guard.deliverable_state answers as a comparison that did not happen rather than a change.
func (e *ManifestException) OSError() bool {
	var errno unix.Errno
	return errors.As(e.cause, &errno)
}

// RuntimeError reports a RecursionError, the one exception here that is not an OSError, a
// ValueError, a KeyError, a TypeError or an AttributeError. guard.deliverable_state's except
// clauses name only those, so this one leaves it: the guard faults, and the omission reader, which
// catches RuntimeError, answers it as evidence it could not read.
func (e *ManifestException) RuntimeError() bool { return e.Class == "RecursionError" }

// PythonText is the fence's f"{type(error).__name__}: {error}".
func (e *ManifestException) PythonText() string { return e.Class + ": " + e.text }

func frozenOSException(err error) *ManifestException {
	return &ManifestException{Class: pythonOSErrorClass(err), text: PythonOSErrorText(err), cause: err}
}

// interpretedErrno is manifest._INTERPRETED_ERRNOS: an answer about the path rather than a
// failure to reach it.
func interpretedErrno(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ESTALE)
}

// resolveFrozenPath is Path.resolve() (os.path.realpath, strict=False) of a pathlib spelling: a
// symlink is followed before the '..' after it, and a component that cannot be examined is kept
// as spelled, so a missing or inaccessible suffix reaches the pinned walk, which names it.
func resolveFrozenPath(path string) (string, error) { return resolvePathDepth(path, 0, false) }
