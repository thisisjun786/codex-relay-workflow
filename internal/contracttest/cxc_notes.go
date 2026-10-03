package contracttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// What a port issue stands behind for a fixture (contract/notes/cxc/README.md).
const (
	cxcPending   = "pending"
	cxcIdentical = "identical"
	cxcChanged   = "intentionally-changed"
)

// cxcNotesFile is one status file, contract/notes/cxc/<issue>.json.
type cxcNotesFile struct {
	Issue     string      `json:"issue"`
	Pending   []string    `json:"pending"`
	Identical []string    `json:"identical"`
	Changed   []cxcChange `json:"intentionally-changed"`
}

// cxcChange is a fixture replayed with named differences: Set replaces the expected text of a key,
// Remove drops the expected keys under a prefix (the keys a failing replay prints), and Given, a
// JSON object, overrides the given the build is run with (patchGiven).
type cxcChange struct {
	ID     string            `json:"id"`
	Reason string            `json:"reason"`
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
	Given  json.RawMessage   `json:"given"`
}

// cxcClaim is what the status files say about a fixture; the zero value is one no file registers.
type cxcClaim struct {
	State, Issue, Reason string
	Set                  map[string]string
	Remove               []string
	Given                json.RawMessage
}

// loadCXCNotes reads the status files in dir and resolves them against the fixtures: a claim beats
// a pending listing whichever file holds each (a port issue edits only its own file); two claims
// on one fixture, a claim without a reason and an id that is no fixture are errors.
func loadCXCNotes(dir string, fixtures []string) (map[string]cxcClaim, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	claims := map[string]cxcClaim{}
	add := func(id string, c cxcClaim) error {
		prior, seen := claims[id]
		switch {
		case !slices.Contains(fixtures, id):
			return fmt.Errorf("%s: %q is not a fixture of contract/fixtures/cxc", c.Issue, id)
		case !seen || prior.State == cxcPending:
			claims[id] = c
		case c.State != cxcPending:
			return fmt.Errorf("fixture %s is claimed by %s and %s", id, prior.Issue, c.Issue)
		}
		return nil
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file cxcNotesFile
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&file); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if _, err := decoder.Token(); err != io.EOF {
			return nil, fmt.Errorf("%s: data after the JSON value", path)
		}
		if stem := strings.TrimSuffix(filepath.Base(path), ".json"); file.Issue != stem {
			return nil, fmt.Errorf("%s: issue %q is not the file name %q", path, file.Issue, stem)
		}
		for state, ids := range map[string][]string{cxcPending: file.Pending, cxcIdentical: file.Identical} {
			for _, id := range ids {
				if err := add(id, cxcClaim{State: state, Issue: file.Issue}); err != nil {
					return nil, err
				}
			}
		}
		for _, change := range file.Changed {
			if strings.TrimSpace(change.Reason) == "" {
				return nil, fmt.Errorf("%s: %q is intentionally changed without a reason", file.Issue, change.ID)
			}
			claim := cxcClaim{State: cxcChanged, Issue: file.Issue, Reason: change.Reason, Set: change.Set, Remove: change.Remove, Given: change.Given}
			if err := add(change.ID, claim); err != nil {
				return nil, err
			}
		}
	}
	return claims, nil
}

// patchGiven applies a claim's given override to a scenario's given, which has been through the name
// substitution already: the override is in crw's names. The override is an object. It is decoded as
// a given first, strictly, so an unknown field is an error whatever its value; then it is merged
// into the given as raw JSON: a null field is removed, a field whose value is an object has its keys
// set or, when null, removed (git is such a field), and any other value (dirs) replaces the field. Entries are never merged
// further and stay raw, so the key order of a given.json entry the override does not name is kept.
func patchGiven(given cxccorpus.Given, override json.RawMessage) (cxccorpus.Given, error) {
	if len(override) == 0 {
		return given, nil
	}
	var patch map[string]json.RawMessage
	checked := json.NewDecoder(bytes.NewReader(override))
	checked.DisallowUnknownFields()
	if err := errors.Join(json.Unmarshal(override, &patch), checked.Decode(new(cxccorpus.Given))); err != nil {
		return given, fmt.Errorf("given override: %w", err)
	}
	if patch == nil {
		return given, errors.New("given override: not an object")
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(marshalPlain(given), &doc)
	for field, value := range patch {
		var keys, entries map[string]json.RawMessage
		switch {
		case string(value) == "null":
			delete(doc, field)
		case json.Unmarshal(value, &keys) != nil:
			doc[field] = value
		default:
			_ = json.Unmarshal(doc[field], &entries)
			if entries == nil {
				entries = map[string]json.RawMessage{}
			}
			for key, entry := range keys {
				if string(entry) == "null" {
					delete(entries, key)
				} else {
					entries[key] = entry
				}
			}
			doc[field] = marshalPlain(entries)
		}
	}
	var out cxccorpus.Given
	strict := json.NewDecoder(bytes.NewReader(marshalPlain(doc)))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&out); err != nil {
		return given, fmt.Errorf("given override: %w", err)
	}
	return out, nil
}

// marshalPlain is json.Marshal without the HTML escaping, which would rewrite the markup characters
// inside a given entry (the file the build reads would then differ from the oracle's).
func marshalPlain(v any) []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(v)
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}
