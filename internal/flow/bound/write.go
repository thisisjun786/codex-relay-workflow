package bound

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Writer is the run's side of the contract: it appends binding lines. Only the run writes the files.
type Writer struct {
	// Dir is the flow-bound directory; empty is Dir().
	Dir string
	// Sync flushes a file to stable storage; nil is (*os.File).Sync. A test replaces it to see when a line was made durable.
	Sync func(*os.File) error
}

func (w *Writer) dir() (string, error) {
	if w != nil && w.Dir != "" {
		return w.Dir, nil
	}
	return Dir()
}

func (w *Writer) sync(f *os.File) error {
	if w != nil && w.Sync != nil {
		return w.Sync(f)
	}
	return f.Sync()
}

// Append adds l to the file of its thread and makes it durable before it returns: the directory is made (mode 0700) when it is
// not there and must be the current user's, mode 0700 and no symlink; the file is made with mode 0600, is opened without
// following a symlink and must be a regular file of the current user with mode 0600; the line goes in with one write, after a
// line feed if the file does not end in one (a line cut short by a crash stays a line of its own, so the new line is the last
// one); then the file (and, for a new file, the directory) is synced. An error means the line is not known to be durable: the
// run sends no turn.
func (w *Writer) Append(l Line) error {
	if !ValidThreadID(l.ThreadID) {
		return fmt.Errorf("bound: %q is not a thread id", l.ThreadID)
	}
	if err := l.Validate(l.ThreadID); err != nil {
		return fmt.Errorf("bound: %w", err)
	}
	raw, err := l.Encode()
	if err != nil {
		return err
	}
	dir, err := w.dir()
	if err != nil {
		return err
	}
	if err := ensureDir(dir); err != nil {
		return err
	}
	root, err := openDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	name := l.ThreadID + FileSuffix
	created := false
	f, err := root.OpenFile(name, os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		created = true
		if err := f.Chmod(0o600); err != nil { // a restrictive umask may have taken bits off; 0600 is the contract
			f.Close()
			return err
		}
	} else if errors.Is(err, fs.ErrExist) {
		f, err = root.OpenFile(name, os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW, 0)
	}
	if err != nil {
		return fmt.Errorf("bound: %s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if bad := trusted(info, name, false, 0o600); bad != nil {
		return bad
	}
	if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err != nil {
			return fmt.Errorf("bound: %s: %w", name, err)
		}
		if last[0] != '\n' {
			raw = append([]byte{'\n'}, raw...)
		}
	}
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		if err == nil {
			err = errors.New("short write")
		}
		return fmt.Errorf("bound: %s: %w", name, err)
	}
	if err := w.sync(f); err != nil {
		return fmt.Errorf("bound: %s: %w", name, err)
	}
	if created {
		return syncDir(dir)
	}
	return nil
}

// ensureDir makes the flow-bound directory when it is not there. A directory that is there is not touched: its owner and mode
// are judged by the caller.
func ensureDir(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return nil
	} else if !errorsIsAbsent(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil // another process made it first: its owner and mode are judged by the caller
		}
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil { // a restrictive umask may have taken bits off
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// syncDir flushes a directory's entries.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
