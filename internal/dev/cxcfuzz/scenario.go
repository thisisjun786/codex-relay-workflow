//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Entry is one "fs" entry of an input: a path under the case root and what stands there.
type Entry struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Mode    int    `json:"mode"`
	Content string `json:"content"`
}

// Scenarios materialises an input's "fs" array under root and returns how many entries it built.
// An input without an "fs" array builds nothing. A path outside root, or a symlink whose target
// resolves outside it, is refused before anything is written, so a refused case leaves no tree.
func Scenarios(root string, input any) (int, error) {
	entries, err := fsEntries(input)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}
	for _, entry := range entries {
		if _, err := confine(root, entry.Path); err != nil {
			return 0, err
		}
		if entry.Kind == "symlink" {
			if _, err := linkTarget(root, entry); err != nil {
				return 0, err
			}
		}
	}
	for _, entry := range entries {
		if err := build(root, entry); err != nil {
			return 0, err
		}
	}
	return len(entries), nil
}

// fsEntries reads the "fs" array of an input.
func fsEntries(input any) ([]Entry, error) {
	value, found := field(input, "fs")
	if !found {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("fs must be an array")
	}
	entries := make([]Entry, 0, len(items))
	for i, item := range items {
		entry, err := readEntry(item)
		if err != nil {
			return nil, fmt.Errorf("fs[%d]: %w", i, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// readEntry reads one entry's fields from the decoded value. A text field is taken as the string it
// already is rather than round-tripped through encoding/json, which replaces the three WTF-8 bytes
// a lone surrogate is held in with U+FFFD: the file would not hold what the input describes.
func readEntry(item any) (Entry, error) {
	var entry Entry
	for _, f := range []struct {
		key  string
		into *string
	}{
		{"path", &entry.Path},
		{"kind", &entry.Kind},
		{"target", &entry.Target},
		{"content", &entry.Content},
	} {
		value, found := field(item, f.key)
		if !found || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return entry, fmt.Errorf("%s must be a string, not %T", f.key, value)
		}
		*f.into = text
	}
	value, found := field(item, "mode")
	if !found || value == nil {
		return entry, nil
	}
	mode, err := integer(value)
	if err != nil {
		return entry, fmt.Errorf("mode: %w", err)
	}
	entry.Mode = mode
	return entry, nil
}

// integer reads a decoded JSON number as an int: pyjson answers a Python integer as a json.Number,
// and a generator may hand one in as a Go int.
func integer(value any) (int, error) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s is not an integer", v)
		}
		return int(n), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("%T is not a number", value)
	}
}

// field reads one key of an object value.
func field(input any, key string) (any, bool) {
	switch v := input.(type) {
	case pyjson.Object:
		return v.Lookup(key)
	case map[string]any:
		value, found := v[key]
		return value, found
	}
	return nil, false
}

// confine resolves a relative path under root, refusing an absolute one or one that leaves root.
func confine(root, path string) (string, error) {
	if path == "" {
		return "", errors.New("the fs path is empty")
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("the fs path %q is absolute", path)
	}
	joined := filepath.Join(root, path)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the fs path %q leaves the case root", path)
	}
	if err := withinRoot(root, joined); err != nil {
		return "", fmt.Errorf("the fs path %q %w", path, err)
	}
	return joined, nil
}

// withinRoot refuses a joined path whose existing part resolves outside root, so a symlink standing
// under the root cannot carry a later write out of it. Only the part that exists is resolved: a
// component that does not exist yet cannot be a symlink, so its nearest existing ancestor decides,
// and a dangling symlink is already confined by linkTarget's check of its own target.
func withinRoot(root, joined string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	for existing := joined; ; {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
				return errors.New("resolves outside the case root")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(existing)
		if parent == existing || len(parent) < len(resolvedRoot) {
			return nil
		}
		existing = parent
	}
}

// linkTarget is a symlink entry's target resolved under root, refused when it leaves root.
func linkTarget(root string, entry Entry) (string, error) {
	target := entry.Target
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(entry.Path), target)
	}
	resolved, err := confine(root, target)
	if err != nil {
		return "", fmt.Errorf("the symlink %s target %q leaves the case root", entry.Path, entry.Target)
	}
	return resolved, nil
}

// build writes one entry. Content is written as content and never executed.
func build(root string, entry Entry) error {
	path, err := confine(root, entry.Path)
	if err != nil {
		return err
	}
	switch entry.Kind {
	case "dir":
		return os.MkdirAll(path, modeOf(entry.Mode, 0o755))
	case "file":
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(entry.Content), modeOf(entry.Mode, 0o644))
	case "symlink":
		if _, err := linkTarget(root, entry); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.Symlink(entry.Target, path)
	default:
		return fmt.Errorf("unknown fs kind %q", entry.Kind)
	}
}

func modeOf(mode int, fallback os.FileMode) os.FileMode {
	if mode == 0 {
		return fallback
	}
	return os.FileMode(mode)
}

// ReplaceRoot rewrites one case's root in an output as ${ROOT}, so the same relative behaviour in
// two different roots compares equal.
func ReplaceRoot(out, root string) string {
	if root == "" {
		return out
	}
	return strings.ReplaceAll(out, root, "${ROOT}")
}

// stripRoot applies ReplaceRoot to every string inside a value, before the two answers compare.
func stripRoot(value any, root string) any {
	switch v := value.(type) {
	case string:
		return ReplaceRoot(v, root)
	case pyjson.Object:
		out := make(pyjson.Object, 0, len(v))
		for _, item := range v {
			out = append(out, pyjson.Field{Key: item.Key, Value: stripRoot(item.Value, root)})
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, stripRoot(item, root))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = stripRoot(item, root)
		}
		return out
	default:
		return value
	}
}
