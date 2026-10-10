package hookswitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// This file is the writing side of the switch file, which crw install switch (CRW-201) uses: the
// same path, document and states the hooks read above, so there is one definition of the file.

// Valid reports whether active names one of the two states.
func Valid(active string) bool { return active == CRW || active == CXC }

// Parse reads the document the way a hook does, and refuses an active value that is neither state:
// a reader that guessed would turn the wrong plugin on.
func Parse(b []byte) (State, error) {
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("switch state is not valid: %w", err)
	}
	if !Valid(s.Active) {
		return State{}, fmt.Errorf("switch state is not valid: active %q is neither %q nor %q", s.Active, CRW, CXC)
	}
	return s, nil
}

// Marshal is the bytes Write publishes: indented JSON ending in a newline.
func Marshal(s State) ([]byte, error) {
	if !Valid(s.Active) {
		return nil, fmt.Errorf("switch state: active %q is neither %q nor %q", s.Active, CRW, CXC)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReadRaw is the switch file's bytes under a Codex home, or nil when it is absent. It reads the
// file as a hook does: without waiting on it, and refusing an entry that is not a regular file.
func ReadRaw(codexHome string) ([]byte, error) {
	b, err := readFile(Path(codexHome))
	if errors.Is(err, errAbsent) {
		return nil, nil
	}
	return b, err
}

// Load is the state under a Codex home, or nil when no switch has ever been written.
func Load(codexHome string) (*State, error) {
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

// WriteRaw publishes bytes as the switch file: a temporary file in the same directory, synced, and
// renamed into place, then the directory synced.
func WriteRaw(codexHome string, b []byte) error {
	path := Path(codexHome)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".switch.json.*.tmp")
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

// Remove deletes the switch file; an absent file is not an error.
func Remove(codexHome string) error {
	err := os.Remove(Path(codexHome))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
