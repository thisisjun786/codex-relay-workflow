package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// TargetKind routes findings to the hooks and mcp-targets checks.
type TargetKind string

const (
	TargetHook TargetKind = "hook"
	TargetMCP  TargetKind = "mcp"
)

type TargetIssue struct {
	Kind    TargetKind `json:"kind"`
	Message string     `json:"message"`
}

// TargetParseError carries the malformed document's kind and path. Filesystem
// errors are returned directly, as readJson of manifest-targets.ts:76-83 does.
type TargetParseError struct {
	Kind TargetKind
	Path string
	Err  error
}

func (e *TargetParseError) Error() string { return e.Err.Error() }
func (e *TargetParseError) Unwrap() error { return e.Err }

// pluginRootTarget is PLUGIN_ROOT_TARGET; JS whitespace is checked separately.
const pluginRootTarget = `\$\{PLUGIN_ROOT\}[\\/]([^"\t\n\v\f\r ]+)`

func targetReadJSON(kind TargetKind, path string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(source.DecodeUTF8(b), pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil {
		return nil, &TargetParseError{kind, path, err}
	}
	return v, nil
}
func targetCommands(command any) []string {
	s, ok := command.(string)
	if !ok {
		return nil
	}
	s = strings.Map(func(r rune) rune {
		if text.Trim(string(r)) == "" {
			return ' '
		}
		return r
	}, s)
	out := []string{}
	for _, m := range regexp.MustCompile(pluginRootTarget).FindAllStringSubmatch(s, -1) {
		rel := m[1]
		parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, "/"))
		}
	}
	return out
}

// targetEscapesRoot keeps the paired realpath fallback exact: either failure
// makes BOTH paths lexical. A missing leaf below a link is a missing target.
func targetEscapesRoot(root, target string) bool {
	r, e1 := filepath.EvalSymlinks(root)
	p, e2 := filepath.EvalSymlinks(target)
	if e1 != nil || e2 != nil {
		r, _ = filepath.Abs(root)
		p, _ = filepath.Abs(target)
	}
	r, _ = filepath.Abs(r)
	p, _ = filepath.Abs(p)
	r = filepath.Clean(r)
	p = filepath.Clean(p)
	return p != r && !strings.HasPrefix(p, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator))
}
func targetResolve(root, rel string) string {
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	p, _ := filepath.Abs(filepath.Join(root, strings.TrimPrefix(rel, "./")))
	return p
}
func targetCheck(issues *[]TargetIssue, kind TargetKind, root, rel, missing string) error {
	abs := targetResolve(root, rel)
	if filepath.IsAbs(rel) || targetEscapesRoot(root, abs) {
		*issues = append(*issues, TargetIssue{kind, "target escapes plugin root: " + rel})
		return nil
	}
	if _, err := os.Stat(abs); err != nil {
		*issues = append(*issues, TargetIssue{kind, missing})
		return nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		*issues = append(*issues, TargetIssue{kind, "target is empty: " + rel})
	}
	return nil
}

// targetProperty models the unvalidated manifest shapes: nullish receivers
// throw, while primitive values yield undefined properties.
func targetProperty(v any, key string) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("Cannot read properties of null (reading '%s')", key)
	}
	if o, ok := v.(pyjson.Object); ok {
		return o.Get(key), nil
	}
	return nil, nil
}
func targetEntries(v any) pyjson.Object {
	o, ok := v.(pyjson.Object)
	if !ok {
		if a, ok := v.([]any); ok {
			for i, x := range a {
				o = append(o, pyjson.Field{Key: strconv.Itoa(i), Value: x})
			}
		}
		return o
	}
	o = append(pyjson.Object{}, o...)
	index := func(k string) uint64 {
		n, e := strconv.ParseUint(k, 10, 32)
		if e == nil && n < 1<<32-1 && strconv.FormatUint(n, 10) == k {
			return n
		}
		return 1 << 32
	}
	slices.SortStableFunc(o, func(a, b pyjson.Field) int {
		ia, ib := index(a.Key), index(b.Key)
		if ia < ib {
			return -1
		}
		if ia > ib {
			return 1
		}
		return 0
	})
	return o
}
func targetIterable(v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	if a, ok := v.([]any); ok {
		return a, nil
	}
	if s, ok := v.(string); ok {
		a := []any{}
		for _, r := range s {
			a = append(a, string(r))
		}
		return a, nil
	}
	return nil, errors.New("value is not iterable")
}
func targetString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		n, _ := strconv.ParseFloat(string(x), 64)
		if n == 0 {
			return "0"
		}
		b, e := json.Marshal(n)
		if e != nil {
			return strconv.FormatFloat(n, 'g', -1, 64)
		}
		return string(b)
	case pyjson.Object:
		return "[object Object]"
	case []any:
		a := []string{}
		for _, e := range x {
			if e == nil {
				a = append(a, "")
			} else {
				a = append(a, targetString(e))
			}
		}
		return strings.Join(a, ",")
	}
	return fmt.Sprint(v)
}

// ValidateManifestTargets ports CXC v0.2.40 manifest-targets.ts. An absent
// manifest has no findings; malformed JSON stops validation with its kind.
func ValidateManifestTargets(pluginRoot string) ([]TargetIssue, error) {
	issues := []TargetIssue{}
	path := filepath.Join(pluginRoot, ".codex-plugin/plugin.json")
	if _, err := os.Stat(path); err != nil {
		return issues, nil
	}
	manifest, err := targetReadJSON(TargetHook, path)
	if err != nil {
		return nil, err
	}
	hooks, err := targetProperty(manifest, "hooks")
	if err != nil {
		return nil, err
	}
	entries, _ := hooks.([]any)
	for _, entry := range entries {
		rel, ok := entry.(string)
		if !ok {
			issues = append(issues, TargetIssue{TargetHook, "manifest hook file must be a string: " + targetString(entry)})
			continue
		}
		file := targetResolve(pluginRoot, rel)
		if filepath.IsAbs(rel) || targetEscapesRoot(pluginRoot, file) {
			issues = append(issues, TargetIssue{TargetHook, "manifest hook file escapes plugin root: " + rel})
			continue
		}
		if _, err := os.Stat(file); err != nil {
			issues = append(issues, TargetIssue{TargetHook, "manifest hook file missing: " + rel})
			continue
		}
		v, err := targetReadJSON(TargetHook, file)
		if err != nil {
			return nil, err
		}
		v, err = targetProperty(v, "hooks")
		if err != nil {
			return nil, err
		}
		if err = targetHookGroups(&issues, pluginRoot, v); err != nil {
			return nil, err
		}
	}
	mcp, err := targetProperty(manifest, "mcpServers")
	if err != nil {
		return nil, err
	}
	rel, ok := mcp.(string)
	if !ok {
		return issues, nil
	}
	file := targetResolve(pluginRoot, rel)
	if filepath.IsAbs(rel) || targetEscapesRoot(pluginRoot, file) {
		return append(issues, TargetIssue{TargetMCP, "manifest mcpServers file escapes plugin root: " + rel}), nil
	}
	if _, err := os.Stat(file); err != nil {
		return append(issues, TargetIssue{TargetMCP, "manifest mcpServers file missing: " + rel}), nil
	}
	v, err := targetReadJSON(TargetMCP, file)
	if err != nil {
		return nil, err
	}
	v, err = targetProperty(v, "mcpServers")
	if err != nil {
		return nil, err
	}
	for _, srv := range targetEntries(v) {
		args, e := targetProperty(srv.Value, "args")
		if e != nil {
			return nil, e
		}
		a, e := targetIterable(args)
		if e != nil {
			return nil, e
		}
		for _, arg := range a {
			if rel, ok := arg.(string); ok && strings.HasSuffix(rel, ".js") {
				if e := targetCheck(&issues, TargetMCP, pluginRoot, rel, "mcp server "+srv.Key+" references missing dist: "+rel); e != nil {
					return nil, e
				}
			}
		}
	}
	return issues, nil
}
func targetHookGroups(issues *[]TargetIssue, root string, v any) error {
	for _, event := range targetEntries(v) {
		groups, err := targetIterable(event.Value)
		if err != nil {
			return err
		}
		for _, group := range groups {
			h, err := targetProperty(group, "hooks")
			if err != nil {
				return err
			}
			handlers, err := targetIterable(h)
			if err != nil {
				return err
			}
			for _, handler := range handlers {
				for _, key := range []string{"command", "commandWindows"} {
					command, err := targetProperty(handler, key)
					if err != nil {
						return err
					}
					for _, rel := range targetCommands(command) {
						if err := targetCheck(issues, TargetHook, root, rel, "hook references missing dist: "+rel); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}
