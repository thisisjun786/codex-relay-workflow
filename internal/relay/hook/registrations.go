package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The user hook file's registrations of this adapter, as the installer reads them before it
// takes the Stop surface: AdapterIdentities and AdapterCommands (completion.adapter_entries).

// entryPointName is the Python-era adapter entry point a registration may still run.
const entryPointName = "completion_hook.py"

// registration is one entry of the user hook file that runs this adapter.
type registration struct{ Identity, Command string }

// readRegistrations lists the entries of the hook file at path that run this adapter for event:
// the Python completion_hook.py entry point, or a native crw hook command. readable is false when
// the file could not be read or does not hold a hook document; a missing file is readable and
// empty.
func readRegistrations(path, event string) ([]registration, bool) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	if info, err := os.Lstat(path); err == nil && info.Mode().Perm()&0o444 == 0 {
		return nil, false
	}
	// Decoded as json.loads decodes it, so a string keeps a lone surrogate escape ("\udcff") as
	// the code point Python holds.
	value, err := Decode(raw)
	doc, isObject := plainJSON(value).(map[string]any)
	if err != nil || !isObject && value != nil {
		return nil, false
	}
	hooksObj, ok := doc["hooks"].(map[string]any)
	if !ok && doc["hooks"] != nil {
		return nil, false
	}
	groups, _ := hooksObj[event].([]any)
	ours := []registration{}
	for gi, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for hi, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			command := text(em["command"])
			words, ok := shellSplit(command)
			if !ok {
				continue
			}
			python := false
			for _, w := range words {
				if filepath.Base(w) == entryPointName {
					python = true
					break
				}
			}
			if python || nativeRegistration(command) {
				ours = append(ours, registration{Identity: fmt.Sprintf("user:%s:%d:%d", event, gi, hi), Command: command})
			}
		}
	}
	return ours, true
}

// nativeExitSuffix is the documented host fail-open suffix of the shipped command (decision 12).
var nativeExitSuffix = regexp.MustCompile(`;\s*exit\s+0\s*$`)

// nativeRegistration recognizes the shipped direct command (decision 23): `crw hook`, optionally
// with a settings path or `--plugin-launch` (decision 26) and the '; exit 0' suffix, the program
// named by path, by a quoted or unquoted $HOME/, ${HOME}/ or unquoted ~/ prefix, or bare. A
// substring, a shell wrapper we cannot interpret, `crw relay` or a neighbouring name is not one.
// A home prefix counts only while the home directory can be named.
func nativeRegistration(command string) bool {
	command = strings.TrimSpace(command)
	command = nativeExitSuffix.ReplaceAllString(command, "")
	words, parsed := shellSplit(command)
	if !parsed || len(words) < 2 || len(words) > 3 {
		return false
	}
	if filepath.Base(words[0]) != "crw" || words[1] != "hook" {
		return false
	}
	for _, prefix := range []string{`"$HOME/`, `"${HOME}/`, "$HOME/", "${HOME}/", "~/"} {
		if strings.HasPrefix(command, prefix) {
			_, err := os.UserHomeDir()
			return err == nil
		}
	}
	return true
}

// shellSplit implements the POSIX quoting subset emitted and accepted by shlex for hook commands.
func shellSplit(s string) ([]string, bool) {
	var out []string
	var b strings.Builder
	quote := byte(0)
	escaped := false
	word := false
	// Byte by byte: every character the split acts on is ASCII, and every other byte is copied as
	// it is, so a word keeps a str's lone surrogate (WTF-8) and any other byte it holds.
	for i := 0; i < len(s); i++ {
		r := s[i]
		if escaped {
			b.WriteByte(r)
			escaped = false
			word = true
			continue
		}
		if quote == 0 {
			switch r {
			case '\\':
				escaped = true
				word = true
			case '\'', '"':
				quote = r
				word = true
			case ' ', '\t', '\n':
				if word {
					out = append(out, b.String())
					b.Reset()
					word = false
				}
			default:
				b.WriteByte(r)
				word = true
			}
		} else if r == quote {
			quote = 0
		} else if r == '\\' && quote == '"' {
			escaped = true
		} else {
			b.WriteByte(r)
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if word {
		out = append(out, b.String())
	}
	return out, true
}

// AdapterIdentities is the identity of every entry in the user hook file at path that runs this
// adapter for event - the Python completion_hook.py entry point or a native crw hook command -
// and whether the file could be read at all (completion.adapter_entries). An unread file is
// not an empty one: the installer refuses a second owner on it rather than defaulting.
func AdapterIdentities(path, event string) ([]string, bool) {
	ours, readable := readRegistrations(path, event)
	identities := make([]string, 0, len(ours))
	for _, registration := range ours {
		identities = append(identities, registration.Identity)
	}
	return identities, readable
}

// AdapterCommand is one registration of this adapter in the user hook file: its identity, its
// command line and that line's words as the POSIX quoting subset splits them.
type AdapterCommand struct {
	Identity, Command string
	Words             []string
}

// AdapterCommands is AdapterIdentities with each registration's command and words, so a caller
// can tell what a registration runs: the installer refuses to move the owned pointer out from
// under an interpreter or script a registration reaches through it. readable is false when the
// file could not be read, which is never an empty file.
func AdapterCommands(path, event string) ([]AdapterCommand, bool) {
	ours, readable := readRegistrations(path, event)
	out := make([]AdapterCommand, 0, len(ours))
	for _, registration := range ours {
		words, _ := shellSplit(nativeExitSuffix.ReplaceAllString(registration.Command, ""))
		out = append(out, AdapterCommand{Identity: registration.Identity, Command: registration.Command, Words: words})
	}
	return out, readable
}

// plainJSON is a Decode value as encoding/json would have produced it for the hook file's
// readers: objects as maps and every number a float64, with each str kept as Decode holds it.
func plainJSON(value any) any {
	switch v := value.(type) {
	case Object:
		out := make(map[string]any, len(v))
		for _, field := range v {
			out[field.Key] = plainJSON(field.Value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = plainJSON(item)
		}
		return out
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return value
}
