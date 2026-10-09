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
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
)

// SwitchBy is the by field of the switch file the harness writes.
const SwitchBy = "laneparity"

// SwitchReport is the hook switch every case root of a run held (CRW-392): the ported legs stay
// silent until <CODEX_HOME>/crw/switch.json says crw, so the harness writes that file into every
// isolated Codex home it fires in, and takes it out again before the case's tree is observed.
type SwitchReport struct {
	File   string `json:"file"`             // the switch file, relative to the case's CODEX_HOME
	Absent bool   `json:"absent,omitempty"` // no switch file was written: the ported legs are off
	Active string `json:"active,omitempty"` // the state written
	By     string `json:"by,omitempty"`
}

// switchFile is the switch file as the report names it.
var switchFile = hookswitch.Path("<CODEX_HOME>")

// switchReport is the report of a run whose case roots hold the switch at active, or none when
// absent.
func switchReport(absent bool) SwitchReport {
	if absent {
		return SwitchReport{File: switchFile, Absent: true}
	}
	return SwitchReport{File: switchFile, Active: hookswitch.CRW, By: SwitchBy}
}

// seedSwitch writes the switch file {"active":"crw","changedAt":...,"by":"laneparity"} into the case's
// CODEX_HOME, as crw install switch does: a temporary file in the same directory renamed into place.
// A directory it makes for it has the mode the hooks give <CODEX_HOME>/crw (0700, as every recorded
// expectation holds it). The undo removes the file, and that directory when it is empty again, so the
// observed tree holds only what the scenario and the hooks left.
func seedSwitch(c *cxccorpus.Case) (func() error, error) {
	codexHome := ""
	for _, kv := range c.Env {
		if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok {
			codexHome = v
		}
	}
	if codexHome == "" {
		return nil, errors.New("the case names no CODEX_HOME")
	}
	path := hookswitch.Path(codexHome)
	dir := filepath.Dir(path)
	_, statErr := os.Lstat(dir)
	made := errors.Is(statErr, fs.ErrNotExist)
	if made {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := writeSwitch(path, hookswitch.State{Active: hookswitch.CRW, ChangedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), By: SwitchBy}); err != nil {
		return nil, err
	}
	return func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if made {
			if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
				return os.Remove(dir)
			}
		}
		return nil
	}, nil
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
