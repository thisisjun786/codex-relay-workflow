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

// targetShapeError retains JavaScript's TypeError message spelling.
type targetShapeError string

func (e targetShapeError) Error() string { return string(e) }

// pluginRootTarget is PLUGIN_ROOT_TARGET; JS whitespace is checked separately.
const pluginRootTarget = `\$\{PLUGIN_ROOT\}[\\/]([^"\t\n\v\f\r ]+)`

// targetNodeText is the file-system form of a manifest string: a manifest string as Node's path encoding reads
// it. pyjson.Loads keeps a lone surrogate escape as three WTF-8 bytes (ED A0..BF 80..BF), which Go reads as
// three invalid bytes and rewrites as three U+FFFD; V8 writes the one lone surrogate as one U+FFFD (EF BF BD)
// when a string becomes a file name, so the existence check must look for that name. A valid pair is a
// four-byte character and stays. Only opening and stat use this form: the string a finding reports, and the
// string the containment test compares when its realpath calls fail, keep the lone surrogate, as the oracle's
// JavaScript string does (CRW-652).
func targetNodeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		if n == 3 && pyjson.IsSurrogate(r) {
			b.WriteString("\uFFFD")
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

func targetReadJSON(kind TargetKind, path string) (any, error) {
	b, err := os.ReadFile(targetNodeText(path))
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
	s = manifestTargetsCommandText(s)
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

// manifestTargetsCommandText is the JavaScript-whitespace mapping the oracle's PLUGIN_ROOT_TARGET leaves to
// its [^"\s] class: each JavaScript whitespace character becomes a space, so the pattern can be a byte class
// here. It walks the string by code point instead of strings.Map, which reads each of a lone surrogate's three
// WTF-8 bytes as U+FFFD and writes three of them, losing the one character the oracle's JavaScript string
// holds; a byte that is not UTF-8 keeps strings.Map's U+FFFD. The match boundaries are the same either way,
// because a lone surrogate and U+FFFD are both outside the class.
func manifestTargetsCommandText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		switch {
		case n == 3 && pyjson.IsSurrogate(r):
			b.WriteString(s[i : i+n])
		case n == 1 && pyjson.IsSurrogate(r):
			b.WriteString("\uFFFD")
		case text.Trim(string(r)) == "":
			b.WriteByte(' ')
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// manifestTargetsRealpath is fs.realpathSync(path) as the oracle's escapesRoot calls it. Go's
// filepath.EvalSymlinks answers the name the directory entry carries, while Node's JavaScript
// implementation resolves the symlinks and keeps the spelling every other component was given; the two
// differ exactly when the caller spelled a component with a lone surrogate, which Node encodes to U+FFFD
// for the system call and returns as the character it was given. The containment judgement compares those
// two answers, so the spelling has to survive (CRW-652).
func manifestTargetsRealpath(path string) (string, error) {
	abs, err := manifestTargetsAbsolute(path)
	if err != nil {
		return "", err
	}
	return manifestTargetsFollow(abs, 0)
}

// manifestTargetsAbsolute makes a path absolute without resolving '..'. filepath.Abs would Clean the
// result, and a '..' in the argument -- after a symlink or not -- would be dropped before the walk
// reached its own component, so the walk would answer a different file than the kernel does (CRW-937).
// The walk resolves every component, '..' included, in kernel order, so the argument has to reach it
// spelled as the caller gave it. What the oracle's path.resolve does drop for a path with no '..' --
// '.' components, doubled separators and a trailing separator -- is dropped here too, so such a path is
// judged exactly as before.
func manifestTargetsAbsolute(path string) (string, error) {
	if filepath.IsAbs(path) {
		return manifestTargetsCollapseDots(path), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if path == "" {
		return wd, nil
	}
	return manifestTargetsCollapseDots(wd + string(filepath.Separator) + path), nil
}

// manifestTargetsCollapseDots drops the components the oracle's path.resolve drops for a path that
// holds no '..': '.' components, doubled separators and a trailing separator. A '..' component is kept,
// because the walk resolves it in kernel order. This is not filepath.Clean: Clean would also resolve
// '..' lexically, which is the defect CRW-937 fixes.
func manifestTargetsCollapseDots(path string) string {
	sep := string(filepath.Separator)
	volume := filepath.VolumeName(path)
	parts := strings.Split(path[len(volume):], sep)
	out := make([]string, 0, len(parts))
	for i, part := range parts {
		if part == "." {
			continue
		}
		if part == "" && i != 0 {
			continue
		}
		out = append(out, part)
	}
	joined := volume + strings.Join(out, sep)
	if joined == "" {
		return sep
	}
	return joined
}

// manifestTargetsFollow is one walk of manifestTargetsRealpath: the components are walked in kernel
// order, and a symlink component is replaced by its target's components, put in front of the components
// still left, so the directory reached is always fully resolved and its lexical parent is its physical
// parent.
//
// A symlink component is stat'ed before its target is read, exactly as Node's realpathSync calls
// binding.stat(base) before binding.readlink (CRW-840): the stat follows the link, so a link whose target
// cannot be reached answers that error instead of a resolved path, and targetEscapesRoot then takes its
// paired lexical fallback.
//
// The link target is not joined or cleaned before its components are walked: it is concatenated as it
// stands, so the next walk splits it and steps through its components ahead of the components still
// left. That is the order the kernel resolves in, so a '..' in the target climbs from the directory the
// walk has physically reached -- not from a lexical prefix. This deliberately diverges from the oracle,
// whose realpathSync resolves the target with pathModule.resolve(previous, linkTarget) and therefore
// drops that '..' lexically (CRW-937): this walk answers the plugin-root containment check, and the
// oracle's answer would judge a target that reaches outside the root as one inside it. The prefix already
// walked is compacted with filepath.Clean, which cannot change the answer because a component reaches it
// only after Lstat found it is not a symlink, so its lexical parent is its physical parent; without that
// the accumulated string would grow without bound and an over-long path would answer an error instead.
// A component that cannot be read, and a link chain past the depth a realpath follows, answer an error,
// which sends both paths to the caller's lexical fallback as the oracle's throw does.
func manifestTargetsFollow(path string, depth int) (string, error) {
	if depth > 40 {
		return "", errors.New("ELOOP: too many levels of symbolic links")
	}
	sep := string(filepath.Separator)
	volume := filepath.VolumeName(path)
	parts := strings.Split(strings.TrimPrefix(path[len(volume):], sep), sep)
	dir := volume
	for i, part := range parts {
		if part == "" {
			continue
		}
		next := dir + sep + part
		info, err := os.Lstat(targetNodeText(next))
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			dir = filepath.Clean(next)
			continue
		}
		// Node's realpathSync stats the link before reading it, so a target it cannot reach is the
		// error the oracle's escapesRoot catches. Without this stat the walk resolved a target whose
		// path passed through a missing component (CRW-840).
		if _, err := os.Stat(targetNodeText(next)); err != nil {
			return "", err
		}
		target, err := os.Readlink(targetNodeText(next))
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = dir + sep + target
		}
		if rest := strings.Join(parts[i+1:], sep); rest != "" {
			target = target + sep + rest
		}
		return manifestTargetsFollow(target, depth+1)
	}
	if dir == "" {
		return sep, nil
	}
	return dir, nil
}

// targetEscapesRoot keeps the paired realpath fallback exact: either failure
// makes BOTH paths lexical. A missing leaf below a link is a missing target.
func targetEscapesRoot(root, target string) bool {
	r, e1 := manifestTargetsRealpath(root)
	p, e2 := manifestTargetsRealpath(target)
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
	// One leading "./" is removed, exactly as the oracle's rel.replace(/^\.\//, "") does: what
	// follows decides whether the result is absolute.
	rel = strings.TrimPrefix(rel, "./")
	if filepath.IsAbs(rel) {
		return manifestTargetsCollapseDots(rel)
	}
	// Concatenated, not joined: filepath.Join would Clean the result and drop a '..' the caller
	// spelled after a symlink, so the containment check would resolve a different file than the
	// kernel does (CRW-937). manifestTargetsRealpath walks every component in order, so only the
	// components the oracle's path.resolve drops for a '..'-free path are dropped here.
	sep := string(filepath.Separator)
	return manifestTargetsCollapseDots(strings.TrimRight(root, sep) + sep + rel)
}
func targetCheck(issues *[]TargetIssue, kind TargetKind, root, rel, missing string) error {
	abs := targetResolve(root, rel)
	if filepath.IsAbs(rel) || targetEscapesRoot(root, abs) {
		*issues = append(*issues, TargetIssue{kind, "target escapes plugin root: " + rel})
		return nil
	}
	if _, err := os.Stat(targetNodeText(abs)); err != nil {
		*issues = append(*issues, TargetIssue{kind, missing})
		return nil
	}
	info, err := os.Stat(targetNodeText(abs))
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
		return nil, targetShapeError(fmt.Sprintf("Cannot read properties of null (reading '%s')", key))
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
	// The manifest is read through the SAME resolution as every target it declares, so one root
	// governs the whole validation (CRW-937): joining it here would clean a root spelled through a
	// link and a '..' back to its lexical directory and mix two roots in one judgement.
	path := targetResolve(pluginRoot, ".codex-plugin/plugin.json")
	if _, err := os.Stat(targetNodeText(path)); err != nil {
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
		if _, err := os.Stat(targetNodeText(file)); err != nil {
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
	if _, err := os.Stat(targetNodeText(file)); err != nil {
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
