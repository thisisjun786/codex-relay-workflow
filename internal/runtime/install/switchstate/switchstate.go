// Package switchstate is the one file that says which plugin's hooks act: <CODEX_HOME>/crw/switch.json.
// `crw install switch` writes it; the CRW hook side reads it. The package imports only the standard
// library, so both sides can import it without a cycle.
package switchstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Active names the plugin whose hooks act.
type Active string

// The two values the file holds.
const (
	CRW Active = "crw"
	CXC Active = "cxc"
)

// State is the content of switch.json: {"active":"crw"|"cxc","changedAt":"<RFC 3339 UTC>","by":"<writer>"}.
type State struct {
	Active    Active `json:"active"`
	ChangedAt string `json:"changedAt"`
	By        string `json:"by"`
}

// Dir and File are the directory below the Codex home and the file name inside it.
const (
	Dir  = "crw"
	File = "switch.json"
)

// Path is the file's location under a Codex home.
func Path(codexHome string) string { return filepath.Join(codexHome, Dir, File) }

// Parse reads the file's bytes. An unknown active value or unknown field is an error: a reader
// that guessed would turn the wrong plugin on.
func Parse(b []byte) (State, error) {
	var s State
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return State{}, fmt.Errorf("switch state is not valid: %w", err)
	}
	if dec.More() {
		return State{}, errors.New("switch state is not valid: trailing content")
	}
	if s.Active != CRW && s.Active != CXC {
		return State{}, fmt.Errorf("switch state is not valid: active %q is neither %q nor %q", s.Active, CRW, CXC)
	}
	return s, nil
}

// Marshal is the bytes Write publishes: indented JSON ending in a newline.
func Marshal(s State) ([]byte, error) {
	if s.Active != CRW && s.Active != CXC {
		return nil, fmt.Errorf("switch state: active %q is neither %q nor %q", s.Active, CRW, CXC)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReadRaw is the file's bytes, or nil when it is absent.
func ReadRaw(codexHome string) ([]byte, error) {
	b, err := os.ReadFile(Path(codexHome))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Read is the state under a Codex home, or nil when no switch has ever been written.
func Read(codexHome string) (*State, error) {
	b, err := ReadRaw(codexHome)
	if err != nil || b == nil {
		return nil, err
	}
	s, err := Parse(b)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Write publishes the state atomically: a reader sees the previous file or the new one whole.
func Write(codexHome string, s State) error {
	b, err := Marshal(s)
	if err != nil {
		return err
	}
	return WriteRaw(codexHome, b)
}

// WriteRaw publishes bytes as the file through a synced temporary file and a rename.
func WriteRaw(codexHome string, b []byte) error {
	path := Path(codexHome)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+File+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Remove deletes the file; an absent file is not an error.
func Remove(codexHome string) error {
	err := os.Remove(Path(codexHome))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
