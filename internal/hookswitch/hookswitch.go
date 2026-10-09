// Package hookswitch is the on/off switch of the ported hooks (CRW-392, J2): CRW installs beside
// CXC with its K1 legs and the GitHub post guard declared, and they stay silent until the switch
// says crw. The switch is the file <CODEX_HOME>/crw/switch.json, {"active":"crw"|"cxc",
// "changedAt","by"}, which crw install switch writes (CRW-201) with a temporary file renamed into
// place; the hook dispatch only reads it. The command a declaration names does not change with the
// switch, so the hooks' trust hashes stay stable across it.
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
	case state.Active == CRW:
		r.On = true
	case state.Active == CXC:
	default:
		r.On, r.Problem = true, fmt.Sprintf("active is %q, neither %q nor %q", state.Active, CRW, CXC)
	}
	return r
}

// errAbsent is a switch path with nothing at it: no entry, not a link whose target is gone.
var errAbsent = fmt.Errorf("switch absent: %w", fs.ErrNotExist)

// readState reads the switch document. It never waits on the file: the open is non-blocking, and
// the opened handle must be a regular file, so a FIFO, a device or a socket is a read error at
// once, whether it is the entry or the target of a link, and whatever a writer does with it.
// ENOENT is an absent switch only when nothing is at the path; a link whose target is gone is
// there and cannot be read.
func readState(path string) (State, error) {
	var s State
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
				return s, errAbsent
			}
			return s, fmt.Errorf("%s: %w", path, err)
		}
		return s, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() {
		return s, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return s, err
	}
	if len(data) > MaxBytes {
		return s, fmt.Errorf("%s is over %d bytes", path, MaxBytes)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
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
