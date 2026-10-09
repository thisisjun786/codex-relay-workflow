//go:build dev

// Package laneparity is the parity acceptance harness for the CRW hook surface (CRW-203,
// `crw-dev parity`). It reads the hook declarations of a plugin root, fires the commands the root
// declares the way the host starts them (/bin/sh -c, PLUGIN_ROOT, an isolated CODEX_HOME) with the
// corpus payloads of contract/fixtures/cxc, and reports three cells apart for every leg:
//
//   - registration: the root declares the leg, under the right event, matcher, timeout and status
//     message (contract K1 after the name substitution);
//   - firing and effect: the declared command ran, and what it printed and wrote is what the
//     fixture expects (the same comparison TestDomain/cxc makes, reached through the declaration);
//   - latency: p50 and p95 of the Go command against the CXC v0.2.40 command for the same payload.
//
// It also verifies the receipts of a run, so a receipt of another run, plugin, build, event, agent
// or skill, a missing hook or a command that does nothing cannot pass. Nothing here is part of a
// release archive, and nothing touches the real Codex home: every case root is a temporary one.
package laneparity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// PluginName is the plugin a CRW root declares itself as.
const PluginName = "crw"

// Leg is one hook registration a CRW plugin root must declare.
type Leg struct {
	Leg     string
	File    string // the hook file under the plugin root
	Event   string // the host's event name: SessionStart, PreToolUse ...
	Matcher string
	Timeout int
	Status  string
	// Own marks a registration CRW adds beyond K1 (no oracle row): only its event and a timeout are held.
	Own bool
}

// Own registrations: the completion Stop the shipped plugin declares today, and the GitHub post guard
// (the component row of cmd/crw/component_hooks.go that no hook JSON declares yet).
const (
	CompletionLeg  = "stop-recording-completion"
	GitHubPostLeg  = "pre-tool-use-guarding-github-post"
	completionFile = "wiring/hooks/stop-recording-completion.json"
	githubPostFile = "wiring/hooks/pre-tool-use-guarding-github-post.json"
)

// ExpectedLegs is what a CRW plugin root must declare: the 32 registrations of K1 with the
// statusMessage rename (R24) and the file moved under wiring/, then the two own registrations.
func ExpectedLegs(root string) ([]Leg, error) {
	file, _, err := cxccorpus.LoadDeclarations(root)
	if err != nil {
		return nil, err
	}
	sub, err := cxccorpus.LoadSubstitution(root)
	if err != nil {
		return nil, err
	}
	var r24 *regexp.Regexp
	var replace string
	for _, rule := range sub.File.Rules {
		if rule.ID == "R24" {
			if r24, err = regexp.Compile(rule.Regex); err != nil {
				return nil, err
			}
			replace = rule.Replace
		}
	}
	if r24 == nil {
		return nil, errors.New("name-substitution.json has no rule R24 (the statusMessage rename)")
	}
	var legs []Leg
	for _, d := range file.Legs {
		legs = append(legs, Leg{
			Leg: d.Leg, File: "wiring/" + d.File, Event: d.Event, Matcher: d.Matcher, Timeout: d.Timeout,
			Status: r24.ReplaceAllString(d.Status, replace),
		})
	}
	legs = append(legs,
		Leg{Leg: CompletionLeg, File: completionFile, Event: "Stop", Timeout: 10, Status: "(crw) Checking the completion records", Own: true},
		Leg{Leg: GitHubPostLeg, File: githubPostFile, Event: "PreToolUse", Matcher: "^Bash$", Timeout: 10, Status: "(crw) Guarding GitHub posts", Own: true},
	)
	return legs, nil
}

// Command is the command line a generated root declares for a leg: the build under test, started
// the way the shipped plugin starts it (crw hook <event> --leg <leg>).
func Command(crw string, l Leg) string {
	if l.Leg == CompletionLeg {
		return `"` + crw + `" hook --plugin-launch; exit 0`
	}
	return `"` + crw + `" hook ` + cxccorpus.Kebab(l.Event) + ` --leg ` + l.Leg
}

var (
	routed       = regexp.MustCompile(`(?:^|\s|")hook ([a-z]+(?:-[a-z]+)*) --leg[= ]([A-Za-z0-9._-]+)(?:\s|;|$)`)
	routedLaunch = regexp.MustCompile(`(?:^|\s|")hook --plugin-launch(?:\s|;|$)`)
)

// Route is the event token and leg a declared command line starts, and false for a command that
// starts none: the registration is then not one of the leg table.
func Route(command string) (event, leg string, ok bool) {
	if m := routed.FindStringSubmatch(command); m != nil {
		return m[1], m[2], true
	}
	if routedLaunch.MatchString(command) {
		return "stop", CompletionLeg, true
	}
	return "", "", false
}

// hookFile is the JSON a host reads for one hook file.
type hookFile struct {
	Hooks map[string][]hookGroup `json:"hooks"`
}

type hookGroup struct {
	Matcher string      `json:"matcher,omitempty"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout,omitempty"`
	Async         bool   `json:"async,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
}

// GeneratePluginRoot writes a plugin root under dest from the CRW plugin at src: everything src
// holds except its manifest and hook files, which are generated so the root declares exactly legs,
// every command starting crw. The skills directory is linked, not copied. The root is for tests of
// the harness and for the first cells of a run before the activation PR lands; any other root can
// be named instead.
func GeneratePluginRoot(dest, src, crw string, legs []Leg) error {
	if strings.ContainsAny(crw, "\"$`\\\n") || !filepath.IsAbs(crw) {
		return fmt.Errorf("crw path %q must be absolute and free of quote, dollar, backtick and backslash", crw)
	}
	if err := os.MkdirAll(filepath.Join(dest, ".codex-plugin"), 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.Name() {
		case ".codex-plugin", "wiring":
			continue
		}
		if err := os.Symlink(filepath.Join(src, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return err
		}
	}
	if err := copyExceptHooks(filepath.Join(src, "wiring"), filepath.Join(dest, "wiring")); err != nil {
		return err
	}
	byFile := map[string]*hookFile{}
	var files []string
	for _, l := range legs {
		f := byFile[l.File]
		if f == nil {
			f = &hookFile{Hooks: map[string][]hookGroup{}}
			byFile[l.File] = f
			files = append(files, l.File)
		}
		f.Hooks[l.Event] = append(f.Hooks[l.Event], hookGroup{Matcher: l.Matcher, Hooks: []hookEntry{{
			Type: "command", Command: Command(crw, l), Timeout: l.Timeout, StatusMessage: l.Status,
		}}})
	}
	sort.Strings(files)
	var listed []string
	for _, rel := range files {
		raw, err := marshal(byFile[rel])
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dest, rel), raw); err != nil {
			return err
		}
		listed = append(listed, "./"+rel)
	}
	manifest := map[string]any{}
	raw, err := os.ReadFile(filepath.Join(src, ".codex-plugin", "plugin.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	manifest["hooks"] = listed
	out, err := marshal(manifest)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dest, ".codex-plugin", "plugin.json"), out)
}

func marshal(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	return append(raw, '\n'), err
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func copyExceptHooks(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "hooks" || strings.HasPrefix(rel, "hooks"+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dest, rel), 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, rel), data, info.Mode().Perm())
	})
}

// PluginDigest identifies a plugin root's declaration surface: a hash over the manifest and every
// hook file with their paths. Two plugins that declare the same event differ in it.
func PluginDigest(root string) (string, error) {
	sum := sha256.New()
	var paths []string
	for _, dir := range []string{".codex-plugin", filepath.Join("wiring", "hooks")} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.IsDir() {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(sum, "%s\x00%d\x00", rel, len(raw))
		sum.Write(raw)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// FileDigest is the sha256 of a file: the build under test is named by it in every receipt.
func FileDigest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
