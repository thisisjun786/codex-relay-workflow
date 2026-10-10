// Package hookswitch is the on/off switch of the ported hooks (CRW-392, J2): CRW installs beside
// CXC with its K1 legs and the GitHub post guard declared, and they stay silent until the switch
// says crw. The switch is the file <CODEX_HOME>/crw/switch.json, {"active":"crw"|"cxc",
// "changedAt","by"}. This package is both sides of that file, with one definition of it (the path,
// the document, the two states and the rule for what a valid state is, State.check).
//
// The hook side (Read) is lenient: a switch that is there but cannot be read or parsed, in any way,
// is on with a warning, because a protective guard is never silenced by a damaged file, and a field
// it does not know is ignored. The installer side (Parse, Load, Marshal, Write, WriteRaw, KeepAside)
// is strict: it refuses a document with an unknown field or content after the document, and a state
// without its provenance, and crw install switch (CRW-201) repairs a switch.json it cannot use by
// setting that entry aside (KeepAside) and publishing a whole document with a temporary file renamed
// into place. The command a declaration names does not change with the switch, so the hooks' trust
// hashes stay stable across it.
package hookswitch

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// The two states of the switch.
const (
	CRW = "crw" // the ported hooks run
	CXC = "cxc" // the ported hooks are silent; CXC's own plugin serves the session
)

// MaxBytes bounds the switch file a hook reads; a larger one is unreadable.
const MaxBytes = 64 * 1024

// State is the switch file's document.
type State struct {
	Active    string `json:"active"`
	ChangedAt string `json:"changedAt"`
	By        string `json:"by"`
}

// Valid reports whether active names one of the two states.
func Valid(active string) bool { return active == CRW || active == CXC }

// check is the one rule for a state's validity, which the hook's Read, Parse and Marshal all apply:
// active must name one of the two states. A reader that guessed would turn the wrong plugin on.
func (s State) check() error {
	if !Valid(s.Active) {
		return fmt.Errorf("active %q is neither %q nor %q", s.Active, CRW, CXC)
	}
	return nil
}

// Path is the switch file under a Codex home.
func Path(codexHome string) string { return filepath.Join(codexHome, "crw", "switch.json") }

// WarningPath is where a hook that could not read the switch leaves its warning: beside the
// invocation records of <CODEX_HOME>/crw/hook-observations, the latest replacing the one before.
func WarningPath(codexHome string) string {
	return filepath.Join(codexHome, "crw", "hook-observations", "switch-warning.json")
}

// CodexHome is CODEX_HOME when it is set and not empty, else ~/.codex, which is how Codex and
// the installer resolve it.
func CodexHome(env host.LookupEnv) (string, error) {
	if named, _ := env("CODEX_HOME"); named != "" {
		return named, nil
	}
	home, err := host.Home(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// Reading is what a hook made of the switch.
type Reading struct {
	On        bool   // the ported legs run
	CodexHome string // empty when it could not be resolved
	Problem   string // non-empty: the switch could not be read, and On is true
}

// Read decides the switch for a hook. No file is off, so an installation stays silent until the
// switch is turned; cxc is off and crw on. A switch that is there but cannot be read (an error
// other than its absence, a link whose target is gone, an entry that is not a regular file, a file
// over MaxBytes, a document that does not parse, an active value that is neither state), and a
// Codex home that cannot be resolved, are on: a protective guard is never silenced by a damaged
// switch. Problem then says why. Read never waits on the switch file.
func Read(env host.LookupEnv) Reading {
	codexHome, err := CodexHome(env)
	if err != nil {
		return Reading{On: true, Problem: "codex home unresolved: " + err.Error()}
	}
	r := Reading{CodexHome: codexHome}
	state, err := readState(Path(codexHome))
	switch {
	case errors.Is(err, errAbsent):
		return r
	case err != nil:
		r.On, r.Problem = true, err.Error()
	default:
		if cerr := state.check(); cerr != nil {
			r.On, r.Problem = true, cerr.Error()
		} else {
			r.On = state.Active == CRW
		}
	}
	return r
}

// errAbsent is a switch path with nothing at it: no entry, not a link whose target is gone.
var errAbsent = fmt.Errorf("switch absent: %w", fs.ErrNotExist)

// readState reads the switch document, as readFile gives it. It is the lenient reading of the hook:
// a field it does not know is ignored (Parse, the installer's reading, refuses it).
func readState(path string) (State, error) {
	var s State
	data, err := readFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// readFile reads the switch file's bytes. It never waits on the file: the open is non-blocking, and
// the opened handle must be a regular file, so a FIFO, a device or a socket is a read error at
// once, whether it is the entry or the target of a link, and whatever a writer does with it.
// ENOENT is an absent switch only when nothing is at the path; a link whose target is gone is
// there and cannot be read, and the problem says so with the link's target (CRW-1142).
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			info, lerr := os.Lstat(path)
			if errors.Is(lerr, fs.ErrNotExist) {
				return nil, errAbsent
			}
			// The open error already names the path, so it is not named again.
			if lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
				if target, rerr := os.Readlink(path); rerr == nil {
					return nil, fmt.Errorf("switch file %s is a symlink to a target that does not exist (%s)", path, target)
				}
			}
			return nil, err
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("%s is over %d bytes", path, MaxBytes)
	}
	return data, nil
}

// Warn records that leg ran on a switch it could not read. It is a diagnostic: it never fails the
// hook, writes nothing to either stream, and reports only whether the warning was written.
func (r Reading) Warn(leg string, now time.Time) bool {
	if r.Problem == "" || r.CodexHome == "" {
		return false
	}
	path := WarningPath(r.CodexHome)
	doc, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"observedAt":    now.UTC().Format("2006-01-02T15:04:05.000Z"),
		"leg":           leg,
		"switchFile":    Path(r.CodexHome),
		"problem":       r.Problem,
		"treatedAs":     CRW,
	})
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return false
	}
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return false
	}
	tmp := filepath.Join(filepath.Dir(path), "."+hex.EncodeToString(suffix)+".tmp")
	if err := os.WriteFile(tmp, append(doc, '\n'), 0o600); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	if os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}
