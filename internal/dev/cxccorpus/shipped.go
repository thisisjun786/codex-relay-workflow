//go:build dev

package cxccorpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The plugin's hook declarations (CRW-392): one file per K1 leg, generated from
// hook-declarations.json, plus CRW's own GitHub post guard, beside the relay's completion Stop,
// which is written by hand and is not generated.
const (
	// ShippedHooksDir is where the plugin's hook files live, relative to the repository root.
	ShippedHooksDir = "plugins/crw/wiring/hooks"
	// ShippedManifest is the plugin manifest whose hooks list names them.
	ShippedManifest = "plugins/crw/.codex-plugin/plugin.json"
	// CompletionStop is the relay's completion Stop, which runs whatever the hook switch says.
	CompletionStop = "stop-recording-completion.json"
	// RuntimePointer is the crw a declaration starts: the installer's pointer under the home
	// directory, as the completion Stop names it.
	RuntimePointer = `"$HOME/.local/share/crw-runtime/current/bin/crw"`
	// statusRule is the substitution rule a K1 statusMessage goes through, and only that one.
	statusRule = "R24"
)

// ShippedHook is one generated declaration: a K1 leg, or the GitHub post guard.
type ShippedHook struct {
	Leg, Event, Matcher, Status string
	Timeout                     int
}

// GitHubPostGuard is CRW's own PreToolUse guard over the shell tools (CRW-783), declared after the
// K1 legs. Its matcher is the guard's own set of shell tools (memoryGateShellTool).
var GitHubPostGuard = ShippedHook{
	Leg: "pre-tool-use-guarding-github-post", Event: "PreToolUse", Matcher: "^(Bash|shell|exec_command|local_shell)$",
	Timeout: 10, Status: "(crw) Guarding GitHub posts",
}

// File is the declaration's file name: the leg's name, so the leg split out of one oracle file
// (post-compact-injecting-bg-terminal-affordance.<event>) has a file of its own.
func (h ShippedHook) File() string { return h.Leg + ".json" }

// Command is the declared command: the runtime pointer's crw hook <event> --leg <leg>, with no
// `; exit 0`, so the harness's own failure status reaches the host.
func (h ShippedHook) Command() string {
	return RuntimePointer + " hook " + Kebab(h.Event) + " --leg " + h.Leg
}

// Document is the declaration file's bytes, in the oracle's hook-file layout (the matcher after the
// group's hooks, two-space indent, a final newline), with no HTML escaping.
func (h ShippedHook) Document() ([]byte, error) {
	type command struct {
		Type          string `json:"type"`
		Command       string `json:"command"`
		Timeout       int    `json:"timeout"`
		StatusMessage string `json:"statusMessage"`
	}
	type group struct {
		Hooks   []command `json:"hooks"`
		Matcher string    `json:"matcher,omitempty"`
	}
	doc := struct {
		Hooks map[string][]group `json:"hooks"`
	}{map[string][]group{h.Event: {{Hooks: []command{{"command", h.Command(), h.Timeout, h.Status}}, Matcher: h.Matcher}}}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ShippedHooks is every generated declaration in manifest order: the K1 legs in theirs, then the
// GitHub post guard. A K1 leg keeps its event, matcher and timeout byte for byte; its statusMessage
// goes through R24 of the substitution table.
func ShippedHooks(root string) ([]ShippedHook, error) {
	decls, _, err := LoadDeclarations(root)
	if err != nil {
		return nil, err
	}
	sub, err := LoadSubstitution(root)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(sub.File.Rules, func(r SubstitutionRule) bool { return r.ID == statusRule })
	if i < 0 {
		return nil, fmt.Errorf("%s has no rule %s", Substitution, statusRule)
	}
	rule := sub.File.Rules[i]
	re, err := regexp.Compile(rule.Regex)
	if err != nil {
		return nil, err
	}
	var out []ShippedHook
	for _, d := range decls.Legs {
		out = append(out, ShippedHook{Leg: d.Leg, Event: d.Event, Matcher: d.Matcher, Timeout: d.Timeout, Status: re.ReplaceAllString(d.Status, rule.Replace)})
	}
	if slices.ContainsFunc(out, func(h ShippedHook) bool { return h.Leg == GitHubPostGuard.Leg }) {
		return nil, fmt.Errorf("%s declares %s, CRW's own leg", Declarations, GitHubPostGuard.Leg)
	}
	return append(out, GitHubPostGuard), nil
}

// ShippedHookList is the manifest's hooks list: the completion Stop, then every generated file.
func ShippedHookList(hooks []ShippedHook) []string {
	list := []string{"./wiring/hooks/" + CompletionStop}
	for _, h := range hooks {
		list = append(list, "./wiring/hooks/"+h.File())
	}
	return list
}

// WriteShippedHooks is `crw-dev cxc hooks`: it writes every generated file and removes any other
// file from the hooks directory but the completion Stop. The manifest's hooks list is not written;
// CheckShippedHooks says when it differs. It publishes only inside root: it refuses, before it writes
// or removes anything, a hooks directory (or a parent of it below root) or a declaration path that is
// a link or not a regular file, and each file is replaced by a rename, never written through a path.
func WriteShippedHooks(root string) error {
	hooks, err := ShippedHooks(root)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, ShippedHooksDir)
	if err := refuseLinkedDir(root, ShippedHooksDir); err != nil {
		return err
	}
	keep := map[string]bool{CompletionStop: true}
	docs := make([][]byte, len(hooks))
	for i, h := range hooks {
		if docs[i], err = h.Document(); err != nil {
			return err
		}
		if info, err := os.Lstat(filepath.Join(dir, h.File())); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("%s/%s is not a regular file (%s): refusing to write through it", ShippedHooksDir, h.File(), info.Mode().Type())
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		keep[h.File()] = true
	}
	for i, h := range hooks {
		if err := replaceFile(dir, h.File(), docs[i]); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// refuseLinkedDir fails unless rel, below root, is a directory reached through no link: every
// component from root down is looked at without following it.
func refuseLinkedDir(root, rel string) error {
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a plain directory (%s): refusing to publish through it", strings.TrimPrefix(cur, root+string(filepath.Separator)), info.Mode().Type())
		}
	}
	return nil
}

// replaceFile writes data as dir/name through a temporary file in dir and a rename, which replaces
// the directory entry itself and never follows a link at name.
func replaceFile(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".hook-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// CheckShippedHooks is the K1-against-shipped equality (Lint): every generated file is in the hooks
// directory with exactly the bytes ShippedHooks gives, the directory holds nothing else but the
// completion Stop, and the manifest's hooks list is ShippedHookList in that order.
func CheckShippedHooks(root string) (problems []string) {
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	hooks, err := ShippedHooks(root)
	if err != nil {
		return []string{err.Error()}
	}
	dir := filepath.Join(root, ShippedHooksDir)
	want := map[string]bool{CompletionStop: true}
	for _, h := range hooks {
		want[h.File()] = true
		doc, err := h.Document()
		if err != nil {
			add("%s: %v", h.File(), err)
			continue
		}
		got, err := os.ReadFile(filepath.Join(dir, h.File()))
		if errors.Is(err, os.ErrNotExist) {
			add("%s/%s is missing: run `crw-dev cxc hooks`", ShippedHooksDir, h.File())
		} else if err != nil {
			add("%s/%s: %v", ShippedHooksDir, h.File(), err)
		} else if !bytes.Equal(got, doc) {
			add("%s/%s differs from %s: run `crw-dev cxc hooks`", ShippedHooksDir, h.File(), Declarations)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		add("%s: %v", ShippedHooksDir, err)
	}
	for _, e := range entries {
		if !want[e.Name()] {
			add("%s/%s is not generated from %s: run `crw-dev cxc hooks`", ShippedHooksDir, e.Name(), Declarations)
		}
	}
	var manifest struct {
		Hooks []string `json:"hooks"`
	}
	raw, err := os.ReadFile(filepath.Join(root, ShippedManifest))
	if err == nil {
		err = json.Unmarshal(raw, &manifest)
	}
	if err != nil {
		add("%s: %v", ShippedManifest, err)
	} else if list := ShippedHookList(hooks); !slices.Equal(manifest.Hooks, list) {
		add("%s hooks must be, in this order: %s", ShippedManifest, strings.Join(list, ", "))
	}
	return problems
}
