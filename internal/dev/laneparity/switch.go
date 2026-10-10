//go:build dev

package laneparity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
)

// SwitchBy is the by field of the switch file the harness writes.
const SwitchBy = "laneparity"

// The states the harness holds the hook switch in while it fires: the ported legs run at crw, and are
// silent with no switch file (off, the state of an installation nobody switched) and at cxc.
const (
	SwitchOn  = hookswitch.CRW
	SwitchOff = "off"
	SwitchCXC = hookswitch.CXC
)

// SwitchReport is the hook switch every case root of a run held (CRW-392): the ported legs stay
// silent until <CODEX_HOME>/crw/switch.json says crw, so the harness writes that file into the Codex
// home of every step it fires, and takes it out again before the case's tree is observed.
type SwitchReport struct {
	File   string `json:"file"`             // the switch file, relative to the case's CODEX_HOME
	Absent bool   `json:"absent,omitempty"` // no switch file was written: the ported legs are off
	Active string `json:"active,omitempty"` // the state written
	By     string `json:"by,omitempty"`
}

// switchFile is the switch file as the report names it.
var switchFile = hookswitch.Path("<CODEX_HOME>")

// switchReport is the report of a run whose case roots hold the switch in state ("" is crw).
func switchReport(state string) SwitchReport {
	switch state {
	case SwitchOff:
		return SwitchReport{File: switchFile, Absent: true}
	case SwitchCXC:
		return SwitchReport{File: switchFile, Active: hookswitch.CXC, By: SwitchBy}
	}
	return SwitchReport{File: switchFile, Active: hookswitch.CRW, By: SwitchBy}
}

// runtimeCommand is the runtime the shipped plugin's declarations start (docs/plugin-packaging.md):
// the command is "$HOME/" + runtimeBin, and HOME is the one of the step.
const runtimeBin = ".local/share/crw-runtime/current/bin/crw"

// seedPlan is what the harness puts into a case for the declared commands to find: the hook switch
// in the state it holds, and, for a plugin root whose commands start the installed runtime, a link
// at that path to the build under test (the harness's own stand-in for the installation, which is
// never read or written for real).
type seedPlan struct {
	Switch  string // SwitchOn (or empty), SwitchOff or SwitchCXC
	CRW     string // the build the runtime link points at
	Runtime bool   // link the runtime into the step's HOME
}

// seed puts the plan into the case for one step and returns the undo that takes it out again. A file
// that is already there (an earlier step's, or the scenario's own) stays as it is and is not undone.
// The switch goes where the hooks of this step look for it, <CODEX_HOME>/crw/switch.json with
// CODEX_HOME as the step's environment has it, else $HOME/.codex: a step may name a home of its own
// (env) or unset CODEX_HOME, and the switch must be there too. Nothing is written outside the case
// root, and a step that names no home is refused (the account's home is never resolved here). A directory made for it has the mode
// the hooks give what they make there (MkdirAll 0700, as every recorded expectation holds it);
// the undo removes the files, and the directories it made when they are empty again, so the observed
// tree holds only what the scenario and the hooks left.
func (p seedPlan) seed(c *cxccorpus.Case, env []string) (func() error, error) {
	lookup := func(k string) (string, bool) {
		for i := len(env) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(env[i], k+"="); ok {
				return v, true
			}
		}
		return "", false
	}
	var undos []func() error
	undo := func() error {
		var first error
		for i := len(undos) - 1; i >= 0; i-- {
			if err := undos[i](); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	fail := func(err error) (func() error, error) {
		_ = undo()
		return nil, err
	}
	if p.Runtime {
		home, _ := lookup("HOME")
		if !filepath.IsAbs(home) || !within(c.Root, home) {
			return fail(fmt.Errorf("the step's HOME %q is not under the case root %s: no runtime is linked there", home, c.Root))
		}
		link := filepath.Join(home, runtimeBin)
		if err := resolvedWithin(c.Root, filepath.Dir(link)); err != nil {
			return fail(fmt.Errorf("the runtime link under HOME %q: %w", home, err))
		}
		if _, err := os.Lstat(link); errors.Is(err, fs.ErrNotExist) {
			made, err := makeDirs(filepath.Dir(link), 0o755)
			undos = append(undos, removeEmpty(made))
			if err != nil {
				return fail(err)
			}
			if err := os.Symlink(p.CRW, link); err != nil {
				return fail(err)
			}
			undos = append(undos, func() error { return removeBelow(c.Root, link) })
		}
	}
	if p.Switch != SwitchOff {
		codexHome := ""
		if v, _ := lookup("CODEX_HOME"); v != "" {
			codexHome = v
		} else if h, _ := lookup("HOME"); h != "" {
			codexHome = filepath.Join(h, ".codex")
		}
		if !filepath.IsAbs(codexHome) || !within(c.Root, codexHome) {
			return fail(fmt.Errorf("the step's Codex home %q is not under the case root %s: nothing is written there", codexHome, c.Root))
		}
		path := hookswitch.Path(codexHome)
		if err := resolvedWithin(c.Root, filepath.Dir(path)); err != nil {
			return fail(fmt.Errorf("the switch file under the Codex home %q: %w", codexHome, err))
		}
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			made, err := makeDirs(filepath.Dir(path), 0o700) // as the hooks make <CODEX_HOME>/crw: MkdirAll 0700
			undos = append(undos, removeEmpty(made))
			if err != nil {
				return fail(err)
			}
			state := hookswitch.State{Active: p.Switch, ChangedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), By: SwitchBy}
			if state.Active == "" {
				state.Active = hookswitch.CRW
			}
			if err := writeSwitch(path, state); err != nil {
				return fail(err)
			}
			undos = append(undos, func() error { return removeBelow(c.Root, path) })
		}
	}
	return undo, nil
}

// within is whether path is root or below it, by name only. Whatever the harness creates goes
// through resolvedWithin as well: a link in the tree (the scenario's given.symlinks, or one a step
// makes) may point out of the case root.
func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvedWithin is whether path, with every link on the way followed, is root or below it. The
// part of path that does not exist yet is taken as written below the deepest part that does; a link
// that points nowhere, or a path that cannot be examined, is refused. A link that stays inside the
// case root is fine.
func resolvedWithin(root, path string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("the case root %s cannot be resolved: %w", root, err)
	}
	var rest []string
	cur := filepath.Clean(path)
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return fmt.Errorf("%s cannot be resolved (a link that points nowhere?): %w", cur, err)
			}
			slices.Reverse(rest)
			full := filepath.Join(append([]string{real}, rest...)...)
			if !within(realRoot, full) {
				return fmt.Errorf("%s resolves to %s, outside the case root %s: nothing is created there", path, full, realRoot)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("%s has no existing ancestor", path)
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// removeBelow removes a file the harness made, after checking again that the way to it still stays
// in the case root (a step may have replaced a directory above it with a link).
func removeBelow(root, path string) error {
	if err := resolvedWithin(root, filepath.Dir(path)); err != nil {
		return fmt.Errorf("not removing %s: %w", path, err)
	}
	return removeIfExists(path)
}

// makeDirs makes dir and the missing directories above it with mode perm, and returns the ones it
// made, outermost first.
func makeDirs(dir string, perm os.FileMode) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	slices.Reverse(missing)
	var made []string
	for _, d := range missing {
		if err := os.Mkdir(d, perm); err != nil {
			return made, err
		}
		made = append(made, d)
		if err := os.Chmod(d, perm); err != nil {
			return made, err
		}
	}
	return made, nil
}

// removeEmpty is the undo of makeDirs: the directories it made, innermost first, each only when it
// is empty again (a hook may have written below one, and what a hook wrote stays).
func removeEmpty(made []string) func() error {
	return func() error {
		for i := len(made) - 1; i >= 0; i-- {
			if err := os.Remove(made[i]); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
				return err
			}
		}
		return nil
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeSwitch writes the switch document atomically: a temporary file beside it, renamed into place.
func writeSwitch(path string, s hookswitch.State) error {
	doc, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".switch-"+hex.EncodeToString(suffix)+".tmp")
	if err := os.WriteFile(tmp, append(doc, '\n'), 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("switch file: %w", err)
	}
	return nil
}

// newSeedPlan is the plan for a run that holds the hook switch in state and fires the declared
// commands: it links the runtime into the HOME of a step when any command starts it.
func newSeedPlan(state, crw string, declared map[string]string) seedPlan {
	p := seedPlan{Switch: state, CRW: crw}
	for _, command := range declared {
		if word, ok := firstWord(command); ok && (word == "$HOME/"+runtimeBin || word == "${HOME}/"+runtimeBin) {
			p.Runtime = true
		}
	}
	return p
}
