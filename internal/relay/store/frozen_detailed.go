package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func VerifyFrozenDetailed(reference string, entries []ManifestEntry) (string, []string, []string, error) {
	document := filepath.Join(reference, "MANIFEST.json")
	info, err := os.Stat(document)
	if err != nil {
		problem := []string{reference + ": no MANIFEST.json in the frozen copy"}
		if errors.Is(err, unix.EACCES) {
			return "", nil, nil, &frozenAccessError{err}
		}
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ESTALE) {
			return "", problem, []string{}, nil
		}
		return "", nil, nil, &frozenAccessError{err}
	}
	if !info.Mode().IsRegular() {
		return "", []string{reference + ": no MANIFEST.json in the frozen copy"}, []string{}, nil
	}
	raw, err := os.ReadFile(document)
	if err != nil {
		return "", nil, nil, &frozenAccessError{err}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil, nil, err
	}
	if fields == nil {
		return "", nil, nil, errors.New("frozen manifest is not an object")
	}
	if _, present := fields["entries"]; !present {
		return "", nil, nil, errors.New("frozen manifest has no entries")
	}
	if string(fields["entries"]) == "null" {
		return "", nil, nil, errors.New("frozen manifest entries is null")
	}
	var documentValue struct {
		Entries []map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &documentValue); err != nil {
		return "", nil, nil, err
	}
	type frozenEntry struct {
		ManifestEntry
		bytes *frozenByteCount
	}
	payload := struct{ Entries []frozenEntry }{}
	for _, record := range documentValue.Entries {
		path, ok := record["path"]
		if !ok {
			return "", nil, nil, &pythonJSONKeyError{"path"}
		}
		digest, ok := record["sha256"]
		if !ok {
			return "", nil, nil, &pythonJSONKeyError{"sha256"}
		}
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

type pythonJSONKeyError struct{ key string }

func (e *pythonJSONKeyError) Error() string { return PythonRepr(e.key) }

// Preserve the underlying errno while exposing the Python boundary's text.
type frozenAccessError struct{ cause error }

func (e *frozenAccessError) Error() string { return PythonOSErrorText(e.cause) }
func (e *frozenAccessError) Unwrap() error { return e.cause }

// Path.resolve(strict=False) keeps an inaccessible suffix for the pinned walk.
func resolveFrozenPath(path string) (string, error) {
	resolved, err := ResolvePath(path)
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOTDIR) {
		return filepath.Abs(path)
	}
	return resolved, err
}
