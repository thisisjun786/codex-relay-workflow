package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// VerifyFrozenDetailed is manifest.verify_frozen_detailed: the revision the frozen copy's own
// manifest hashes to, every problem found, and the subset of them that were failures to read.
// A frozen MANIFEST.json that is absent, is not a regular file or cannot be reached is a
// problem; which of those was an access failure is asked of stat as the fence asks it
// (_frozen_document_access). A document that was reached and could not be read, or was read and
// is not a manifest, is the exception the fence raises instead of an answer (FrozenException).
func VerifyFrozenDetailed(reference string, entries []ManifestEntry) (string, []string, []string, error) {
	document := filepath.Join(reference, "MANIFEST.json")
	if strings.ContainsRune(document, 0) {
		// os.stat refuses the name before any system call, and the probe lets that out.
		return "", nil, nil, &FrozenException{Class: "ValueError", text: "embedded null byte"}
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
	if err := FrozenDocumentError(raw); err != nil {
		return "", nil, nil, err
	}
	var documentValue struct {
		Entries json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &documentValue); err != nil {
		return "", nil, nil, err
	}
	// The shape walk above leaves a list of records, or an empty dict or str that iterates to none.
	var records []map[string]json.RawMessage
	if list := bytes.TrimSpace(documentValue.Entries); len(list) > 0 && list[0] == '[' {
		if err := json.Unmarshal(list, &records); err != nil {
			return "", nil, nil, err
		}
	}
	type frozenEntry struct {
		ManifestEntry
		bytes *frozenByteCount
	}
	payload := struct{ Entries []frozenEntry }{}
	for _, record := range records {
		path, digest := record["path"], record["sha256"]
		var entry ManifestEntry
		if err := json.Unmarshal(path, &entry.Path); err != nil {
			return "", nil, nil, err
		}
		if err := json.Unmarshal(digest, &entry.SHA256); err != nil {
			return "", nil, nil, err
		}
		stored := frozenEntry{ManifestEntry: entry}
		if rawBytes, ok := record["bytes"]; ok && string(rawBytes) != "null" {
			count, err := parseFrozenByteCount(rawBytes)
			if err != nil {
				return "", nil, nil, err
			}
			stored.bytes = &count
		}
		payload.Entries = append(payload.Entries, stored)
	}
	problems, unreadable := []string{}, []string{}
	if entries != nil {
		claimed, stored := map[[2]string]bool{}, map[[2]string]bool{}
		sizes := map[string]*frozenByteCount{}
		for _, e := range entries {
			claimed[[2]string{e.Path, e.SHA256}] = true
		}
		for _, e := range payload.Entries {
			stored[[2]string{e.Path, e.SHA256}] = true
			sizes[e.Path] = e.bytes
		}
		same := len(claimed) == len(stored)
		for key := range claimed {
			same = same && stored[key]
		}
		if !same {
			problems = append(problems, reference+": the frozen manifest does not describe the same deliverables")
		}
		for _, e := range entries {
			if size := sizes[e.Path]; e.Bytes != nil && size != nil && !size.equalInt64(*e.Bytes) {
				problems = append(problems, fmt.Sprintf("%s: caller claims %d bytes but the frozen copy records %s", e.Path, *e.Bytes, size.pythonString()))
			}
		}
	}
	for _, entry := range payload.Entries {
		if !lowerDigest.MatchString(entry.SHA256) {
			problems = append(problems, entry.Path+": "+PythonRepr(entry.SHA256)+" is not a digest")
			continue
		}
		// Resolve non-strictly like Path.resolve(): a missing leaf must still reach the
		// pinned walk, which reports path_changed rather than a generic os.PathError.
		files, err := resolveFrozenPath(filepath.Join(reference, "files"))
		var digest string
		var size int64
		if err == nil {
			var blob string
			blob, err = resolveFrozenPath(filepath.Join(reference, "files", entry.SHA256))
			if err == nil {
				digest, size, _, err = HashArtifact(blob, []string{files}, false)
			}
		}
		if err != nil {
			message := entry.Path + ": frozen bytes unreadable for " + entry.SHA256 + ": " + err.Error()
			problems = append(problems, message)
			var scope *RefusedError
			if !errors.As(err, &scope) || accessFailure(err) {
				unreadable = append(unreadable, message)
			}
			continue
		}
		if digest != entry.SHA256 {
			problems = append(problems, entry.Path+": frozen bytes do not match "+entry.SHA256)
		} else if entry.bytes != nil && !entry.bytes.equalInt64(size) {
			problems = append(problems, fmt.Sprintf("%s: frozen bytes are %d, not the claimed %s", entry.Path, size, entry.bytes.pythonString()))
		}
	}
	manifestEntries := make([]ManifestEntry, len(payload.Entries))
	for i, entry := range payload.Entries {
		manifestEntries[i] = entry.ManifestEntry
	}
	revision, err := ManifestRevision(manifestEntries)
	return revision, problems, unreadable, err
}

type frozenByteCount struct {
	number *big.Rat
	text   string
	bool   *bool
}

func parseFrozenByteCount(raw json.RawMessage) (frozenByteCount, error) {
	if string(raw) == "true" || string(raw) == "false" {
		value := string(raw) == "true"
		return frozenByteCount{bool: &value}, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return frozenByteCount{}, err
	}
	exact, ok := new(big.Rat).SetString(string(number))
	if !ok {
		return frozenByteCount{}, fmt.Errorf("invalid JSON number %q", number)
	}
	return frozenByteCount{number: exact, text: string(number)}, nil
}

func (c frozenByteCount) equalInt64(size int64) bool {
	if c.bool != nil {
		return (*c.bool && size == 1) || (!*c.bool && size == 0)
	}
	return c.number.Cmp(new(big.Rat).SetInt64(size)) == 0
}

func (c frozenByteCount) pythonString() string {
	if c.bool != nil {
		if *c.bool {
			return "True"
		}
		return "False"
	}
	if !strings.ContainsAny(c.text, ".eE") {
		return c.text
	}
	value, _ := json.Number(c.text).Float64()
	return pythonFloat(value)
}

// FrozenException is an exception manifest.verify_frozen_detailed raises instead of answering: the
// frozen MANIFEST.json was reached and could not be read (an OSError), or was read and is not a
// manifest (UnicodeDecodeError, JSONDecodeError, KeyError, TypeError). Error is str(exception)
// and Class its type, which the fence's host envelope and guard.deliverable_state name.
type FrozenException struct {
	Class string
	text  string
	cause error
}

func (e *FrozenException) Error() string { return e.text }
func (e *FrozenException) Unwrap() error { return e.cause }

// OSError reports an OSError: a manifest that is there and that nobody could read, which
// guard.deliverable_state answers as a comparison that did not happen rather than a change.
func (e *FrozenException) OSError() bool {
	var errno unix.Errno
	return errors.As(e.cause, &errno)
}

// PythonText is the fence's f"{type(error).__name__}: {error}".
func (e *FrozenException) PythonText() string { return e.Class + ": " + e.text }

func frozenOSException(err error) *FrozenException {
	return &FrozenException{Class: pythonOSErrorClass(err), text: PythonOSErrorText(err), cause: err}
}

// interpretedErrno is manifest._INTERPRETED_ERRNOS: an answer about the path rather than a
// failure to reach it.
func interpretedErrno(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ESTALE)
}

// FrozenDocumentError is what verify_frozen_detailed raises for MANIFEST.json's bytes before it
// looks at any blob: a UnicodeDecodeError, a JSONDecodeError, or the KeyError or TypeError of
// reading payload["entries"] and each record's path and digest. Nil when the bytes are a manifest.
func FrozenDocumentError(raw []byte) error {
	text, err := DecodeUTF8(raw)
	if err != nil {
		return &FrozenException{Class: "UnicodeDecodeError", text: err.Error(), cause: err}
	}
	if message := PythonJSONError(text); message != "" {
		class := "JSONDecodeError"
		if strings.HasPrefix(message, "Exceeds the limit (") {
			// int() refuses an over-long integer literal with a plain ValueError.
			class = "ValueError"
		}
		return &FrozenException{Class: class, text: message}
	}
	return frozenRecordShape(raw)
}

// frozenRecordShape walks payload["entries"] and each record's ["path"] and ["sha256"] as
// verify_frozen_detailed does, and returns the exception the first step that fails raises.
func frozenRecordShape(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	entries, err := pythonSubscript(document, "entries")
	if err != nil {
		return err
	}
	var records []any
	switch value := entries.(type) {
	case []any:
		records = value
	case map[string]any:
		// Iterating a dict yields its keys, each a str the record subscript refuses.
		if len(value) > 0 {
			records = []any{""}
		}
	case string:
		if value != "" {
			records = []any{""}
		}
	default:
		return &FrozenException{Class: "TypeError", text: "'" + pythonTypeName(entries) + "' object is not iterable"}
	}
	for _, record := range records {
		for _, key := range []string{"path", "sha256"} {
			if _, err := pythonSubscript(record, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// pythonSubscript is value[key] for a str key over a decoded JSON value.
func pythonSubscript(value any, key string) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		if item, ok := v[key]; ok {
			return item, nil
		}
		return nil, &FrozenException{Class: "KeyError", text: PythonRepr(key)}
	case []any:
		return nil, &FrozenException{Class: "TypeError", text: "list indices must be integers or slices, not str"}
	case string:
		return nil, &FrozenException{Class: "TypeError", text: "string indices must be integers, not 'str'"}
	}
	return nil, &FrozenException{Class: "TypeError", text: "'" + pythonTypeName(value) + "' object is not subscriptable"}
}

// pythonTypeName is type(value).__name__ for a JSON scalar as json.loads builds it.
func pythonTypeName(value any) string {
	switch v := value.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			return "float"
		}
		return "int"
	}
	return fmt.Sprintf("%T", value)
}

// Path.resolve(strict=False) keeps an inaccessible suffix for the pinned walk.
func resolveFrozenPath(path string) (string, error) {
	resolved, err := ResolvePath(path)
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOTDIR) {
		return filepath.Abs(path)
	}
	return resolved, err
}
