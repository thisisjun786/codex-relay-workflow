// Package managed ports managed.py's managed-start admission.
package managed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const Schema = "managed-start/1"
const maxRequestBytes = 256000

// ParseRequest snapshots the request and rejects unknown fields before persistent effects.
func ParseRequest(raw []byte) (map[string]any, error) {
	if len(raw) > maxRequestBytes {
		return nil, fmt.Errorf("managed request exceeds the byte limit")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	r, err := object(v, []string{"schema", "requestId", "issueKey", "parent", "child", "artifactRoots", "allowedRecipients", "criteria", "criteriaSource", "baselineRevision", "scopeRef", "prompt"}, []string{"projectKey"}, "request")
	if err != nil {
		return nil, err
	}
	if r["schema"] != Schema {
		return nil, fmt.Errorf("unknown managed request schema")
	}
	for _, key := range []string{"requestId", "issueKey", "criteriaSource", "baselineRevision", "scopeRef", "prompt"} {
		limit := 4096
		if key == "requestId" {
			limit = 128
		}
		if key == "prompt" {
			limit = 90000
		}
		if err = text(r[key], key, limit); err != nil {
			return nil, err
		}
	}
	if p, ok := r["projectKey"]; ok {
		if err = text(p, "projectKey", 4096); err != nil {
			return nil, err
		}
	}
	parent, err := object(r["parent"], []string{"taskId", "hostId", "settings"}, nil, "parent")
	if err != nil {
		return nil, err
	}
	child, err := object(r["child"], []string{"hostId", "title", "settings"}, nil, "child")
	if err != nil {
		return nil, err
	}
	for _, f := range []struct {
		value any
		at    string
	}{{parent["taskId"], "parent.taskId"}, {parent["hostId"], "parent.hostId"}, {child["hostId"], "child.hostId"}, {child["title"], "child.title"}} {
		if err = text(f.value, f.at, 500); err != nil {
			return nil, err
		}
	}
	if err := validateSettings(parent["settings"], "parent.settings"); err != nil {
		return nil, err
	}
	if err := validateSettings(child["settings"], "child.settings"); err != nil {
		return nil, err
	}
	if parent["hostId"] != child["hostId"] {
		return nil, fmt.Errorf("managed start supports one local host only")
	}
	for _, key := range []string{"artifactRoots", "allowedRecipients"} {
		items, ok := r[key].([]any)
		if !ok || len(items) == 0 || len(items) > 256 {
			return nil, fmt.Errorf("%s must be a nonempty list of at most 256 entries", key)
		}
		seen := map[string]bool{}
		for _, item := range items {
			if err = text(item, key, 4096); err != nil {
				return nil, err
			}
			s := item.(string)
			if seen[s] {
				return nil, fmt.Errorf("%s contains duplicates", key)
			}
			seen[s] = true
			if key == "artifactRoots" {
				if !filepath.IsAbs(s) {
					return nil, fmt.Errorf("artifactRoots must be absolute")
				}
				info, err := os.Stat(s)
				if err != nil || !info.IsDir() {
					return nil, fmt.Errorf("artifactRoots must name an existing directory")
				}
			}
		}
		if key == "allowedRecipients" && !seen[parent["taskId"].(string)] {
			return nil, fmt.Errorf("the parent must be an allowed recipient")
		}
	}
	entries, ok := r["criteria"].([]any)
	if !ok || len(entries) == 0 || len(entries) > 256 {
		return nil, fmt.Errorf("criteria must be a nonempty list of at most 256 entries")
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		e, err := object(entry, []string{"id", "title", "required"}, nil, "criterion")
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"id", "title"} {
			if err = text(e[key], "criterion."+key, 4096); err != nil {
				return nil, err
			}
		}
		if _, ok := e["required"].(bool); !ok {
			return nil, fmt.Errorf("criterion.required must be a boolean")
		}
		id := e["id"].(string)
		if seen[id] {
			return nil, fmt.Errorf("duplicate criterion %q", id)
		}
		seen[id] = true
		e["title"] = strings.TrimSpace(e["title"].(string))
	}
	return r, nil
}
func object(v any, required, optional []string, at string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", at)
	}
	allowed := map[string]bool{}
	for _, key := range append(append([]string{}, required...), optional...) {
		allowed[key] = true
	}
	var missing, extra []string
	for _, key := range required {
		if _, ok := m[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range m {
		if !allowed[key] {
			extra = append(extra, key)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		sort.Strings(missing)
		sort.Strings(extra)
		return nil, fmt.Errorf("%s: missing %s, unknown %s", at, pythonTextList(missing), pythonTextList(extra))
	}
	return m, nil
}
func pythonTextList(values []string) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = "'" + strings.ReplaceAll(value, "'", "\\'") + "'"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
func text(v any, at string, limit int) error {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > limit || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%s must be nonblank text of at most %d characters without NUL", at, limit)
	}
	return nil
}
func OperationIDs(requestID string) (string, string) {
	sum := sha256.Sum256([]byte(requestID))
	digest := hex.EncodeToString(sum[:])
	return "managed-create-" + digest, "managed-business-" + digest
}
