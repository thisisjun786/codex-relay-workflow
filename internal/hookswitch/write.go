package hookswitch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// This file is the installer's side of the switch file, which crw install switch (CRW-201) uses: the
// same path, document, states and validity rule the hooks read (hookswitch.go), so there is one
// definition of the file. Where a hook is lenient, this side is strict, and it repairs the file it
// owns: see the package comment.

// Parse reads the document strictly: a field the document does not know, content after the document
// and an active value that is neither state are refused. (A hook reads the same file leniently and
// runs on a damaged one; the installer replaces it, KeepAside first, so a stranger's field never
// survives a switch.)
func Parse(b []byte) (State, error) {
	var s State
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return State{}, fmt.Errorf("switch state is not valid: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return State{}, errors.New("switch state is not valid: content after the document")
	}
	if err := s.check(); err != nil {
		return State{}, fmt.Errorf("switch state is not valid: %w", err)
	}
	return s, nil
}

// Marshal is the bytes Write publishes: indented JSON ending in a newline. It refuses a state with
// an unknown active value and one without its provenance (changedAt and by), so no document without
// a source is published.
func Marshal(s State) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, fmt.Errorf("switch state: %w", err)
	}
	if s.ChangedAt == "" || s.By == "" {
		return nil, errors.New("switch state: changedAt and by are required")
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReadRaw is the switch file's bytes under a Codex home, or nil when it is absent. It reads the
// file as a hook does: without waiting on it, and refusing an entry that is not a regular file. An
// error says the entry cannot be used as it is; the installer then sets it aside (KeepAside).
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
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: %s is in place but the sync of %s failed: %w", ErrNotDurable, path, dir, err)
	}
	return nil
}

// ErrNotDurable is the error of a WriteRaw (or Write) whose file was renamed into place but whose
// directory could not be synced: the new file is published and a reader sees it, and a power loss
// may still undo the rename. A directory that does not support a sync (EINVAL, ENOTSUP, ENOSYS) is
// not an error: there is nothing to confirm there.
var ErrNotDurable = errors.New("switch file published, durability not confirmed")

// syncDir opens a directory and syncs it; it is a variable so a test can inject a failure.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return classifySyncError(err)
}

// classifySyncError is nil for the answer of a directory that cannot be synced at all (EINVAL on a
// filesystem that gives a directory no sync, ENOTSUP, ENOSYS): there is nothing to confirm there, so
// it is not a failure. Any other error, including a failed open, is returned.
func classifySyncError(err error) error {
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) {
		return nil
	}
	return err
}

// Aside is a switch file entry that KeepAside set aside before the installer replaced it.
type Aside struct {
	Path string // where the entry is kept
	home string
	dir  bool
}

// KeepAside keeps the entry at the switch path, whatever it is (a file the hook cannot parse, a
// file over MaxBytes, a link whose target is gone, a FIFO, a directory), as
// switch.json.crw-<stamp>.bak beside it, so the publication that follows loses nothing. A symbolic
// link is kept as the link itself, a new link to the same target made with Symlink (a platform's
// link(2) may follow a link and then fail on a dangling one or keep the target instead of the
// link); any other entry that is not a directory is hard linked, which keeps it to the byte. Both
// leave the switch path in place until the rename replaces it, so a hook never sees the switch
// absent. A directory cannot be replaced by a file and is moved, and the path is absent until the
// publication. It returns nil when nothing is at the path.
func KeepAside(codexHome, stamp string) (*Aside, error) {
	path := Path(codexHome)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := &Aside{Path: path + ".crw-" + stamp + ".bak", home: codexHome, dir: info.IsDir()}
	if _, err := os.Lstat(a.Path); err == nil {
		return nil, fmt.Errorf("%s exists already", a.Path)
	}
	switch {
	case a.dir:
		err = os.Rename(path, a.Path)
	case info.Mode()&fs.ModeSymlink != 0:
		var target string
		if target, err = os.Readlink(path); err == nil {
			err = os.Symlink(target, a.Path)
		}
	default:
		err = os.Link(path, a.Path)
	}
	if err != nil {
		return nil, fmt.Errorf("set %s aside: %w", path, err)
	}
	return a, nil
}

// Restore puts the entry back where it was, over whatever was published since.
func (a *Aside) Restore() error {
	path := Path(a.home)
	if a.dir {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Rename(a.Path, path)
	}
	// Nothing was published: both names are the one entry, and a rename between them does nothing.
	pi, perr := os.Lstat(path)
	ai, aerr := os.Lstat(a.Path)
	if perr == nil && aerr == nil && os.SameFile(pi, ai) {
		return os.Remove(a.Path)
	}
	return os.Rename(a.Path, path)
}

// Remove deletes the switch file; an absent file is not an error.
func Remove(codexHome string) error {
	err := os.Remove(Path(codexHome))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
