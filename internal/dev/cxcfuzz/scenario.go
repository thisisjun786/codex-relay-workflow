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
		raw, err := json.Marshal(plain(item))
		if err != nil {
			return nil, fmt.Errorf("fs[%d]: %w", i, err)
		}
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("fs[%d]: %w", i, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
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

// plain converts a decoded value into what encoding/json writes back.
func plain(value any) any {
	switch v := value.(type) {
	case pyjson.Object:
		out := make(map[string]any, len(v))
		for _, item := range v {
			out[item.Key] = plain(item.Value)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, plain(item))
		}
		return out
	default:
		return value
	}
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
	return joined, nil
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
