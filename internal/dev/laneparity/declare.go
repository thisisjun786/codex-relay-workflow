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
	"slices"
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
		// The GitHub post guard is the declaration the shipped plugin generates for it (CRW-392): its
		// matcher is the guard's own set of shell tools.
		Leg{Leg: GitHubPostLeg, File: githubPostFile, Event: cxccorpus.GitHubPostGuard.Event, Matcher: cxccorpus.GitHubPostGuard.Matcher,
			Timeout: cxccorpus.GitHubPostGuard.Timeout, Status: cxccorpus.GitHubPostGuard.Status, Own: true},
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
	eventToken = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*$`)
	legToken   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Route is the event token and leg a declared command line starts, and false for a command that
// starts none: the registration is then not one of the leg table. Only the forms a CRW root declares
// are read, and only as the shell runs them: one simple command
//
//	<crw> hook <event> --leg <leg>    <crw> hook <event> --leg=<leg>    <crw> hook --plugin-launch
//
// optionally followed by `; exit 0`, where <crw> is crw, $CRW_BIN, or the path of a file named crw
// (absolute, or under $HOME or $PLUGIN_ROOT), bare or in double quotes. Hook syntax anywhere else is
// not a registration: in a comment, as the argument of another command, after && or || or in a
// branch, behind a redirect or a pipe, followed by another command, or with an extra argument.
func Route(command string) (event, leg string, ok bool) {
	words, rest, ok := firstCommand(command)
	if !ok || len(words) < 3 || !crwWord(words[0]) || words[1] != (shellWord{text: "hook"}) {
		return "", "", false
	}
	if tail := strings.TrimSpace(rest); tail != "" && tail != "exit 0" {
		return "", "", false
	}
	args := words[2:]
	for _, a := range args {
		if a.quoted {
			return "", "", false
		}
	}
	switch {
	case len(args) == 1 && args[0].text == "--plugin-launch":
		return "stop", CompletionLeg, true
	case len(args) == 3 && args[1].text == "--leg":
		event, leg = args[0].text, args[2].text
	case len(args) == 2 && strings.HasPrefix(args[1].text, "--leg="):
		event, leg = args[0].text, strings.TrimPrefix(args[1].text, "--leg=")
	default:
		return "", "", false
	}
	if !eventToken.MatchString(event) || !legToken.MatchString(leg) {
		return "", "", false
	}
	return event, leg, true
}

// shellWord is a word of a command line with its quotes removed; quoted is whether any of it was.
type shellWord struct {
	text   string
	quoted bool
}

// firstCommand splits the first simple command of a command line into words, the way /bin/sh would,
// for the plain words a declaration uses: blanks separate words, double quotes group them, and `;`
// ends the command (rest is what follows it). It refuses (false) whatever it does not read exactly,
// so nothing is taken for a word the shell would treat otherwise: a comment, single quotes, a
// backslash, a command substitution, a newline, an operator (&, |, <, >, parentheses) or a glob.
func firstCommand(command string) (words []shellWord, rest string, ok bool) {
	var cur strings.Builder
	inWord, quoted := false, false
	flush := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), quoted: quoted})
		}
		cur.Reset()
		inWord, quoted = false, false
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch c {
		case ' ', '\t':
			flush()
		case ';':
			flush()
			return words, command[i+1:], true
		case '"':
			end := strings.IndexByte(command[i+1:], '"')
			if end < 0 {
				return nil, "", false
			}
			seg := command[i+1 : i+1+end]
			if strings.ContainsAny(seg, "\\`\n") || strings.Contains(seg, "$(") {
				return nil, "", false
			}
			cur.WriteString(seg)
			inWord, quoted = true, true
			i += end + 1
		case '#':
			if !inWord {
				return nil, "", false
			}
			cur.WriteByte(c)
		case '\'', '\\', '`', '\n', '\r', '&', '|', '<', '>', '(', ')', '*', '?', '[':
			return nil, "", false
		default:
			if c == '$' && strings.HasPrefix(command[i:], "$(") {
				return nil, "", false
			}
			cur.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return words, "", true
}

// crwWord is whether a command word names the crw runtime: crw or $CRW_BIN, or the path of a file
// named crw, absolute or under $HOME or $PLUGIN_ROOT, with no other expansion in it.
func crwWord(w shellWord) bool {
	switch w.text {
	case "crw", "$CRW_BIN", "${CRW_BIN}":
		return true
	}
	path := w.text
	for _, prefix := range []string{"$HOME/", "${HOME}/", "$PLUGIN_ROOT/", "${PLUGIN_ROOT}/"} {
		if strings.HasPrefix(path, prefix) {
			path = "/" + strings.TrimPrefix(path, prefix)
			break
		}
	}
	return strings.HasPrefix(path, "/") && !strings.ContainsAny(path, "$*?[") && filepath.Base(path) == "crw"
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
	// The links point at src from dest, a directory elsewhere: a relative src would be read from there.
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
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

// PluginDigest identifies a plugin root's declaration surface: a hash over the manifest, every
// file under wiring/hooks and every hook file the manifest lists (wherever in the root it sits),
// with their paths. Two plugins that declare the same event differ in it.
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
	// A hook file the manifest lists outside those directories is a declaration all the same. A
	// manifest that cannot be read leaves only the files above; the registration cell reports it.
	if manifest, err := readManifest(root); err == nil {
		for _, rel := range manifest.Hooks {
			if clean, err := hookPath(rel); err == nil {
				paths = append(paths, filepath.Join(root, clean))
			}
		}
	}
	sort.Strings(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // listed but missing: the registration cell fails it
			}
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
