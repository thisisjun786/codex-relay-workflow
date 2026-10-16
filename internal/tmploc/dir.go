package tmploc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultDir is the temporary directory when TMPDIR is unset or empty (G-D1 = A): the POSIX and Go
// default, written once here. It is a location, not a ground for any safety judgement.
const DefaultDir = "/tmp"

// The sources DirFrom reports for its directory.
const (
	// SourceEnv: the directory is TMPDIR as written.
	SourceEnv = "env"
	// SourceDefault: TMPDIR is unset or empty, so the directory is DefaultDir.
	SourceDefault = "default"
)

// Lookup is the one read of TMPDIR. It returns the value exactly as written, with fromEnv true,
// when TMPDIR is set and not empty, and (DefaultDir, false) otherwise. It does not validate: a
// caller that keeps its own order of checks (crwconfig) takes the raw text from here. A nil getenv
// is an empty environment. No other variable (TMP, TEMP) is read.
func Lookup(getenv func(string) string) (value string, fromEnv bool) {
	if getenv != nil {
		if v := getenv("TMPDIR"); v != "" {
			return v, true
		}
	}
	return DefaultDir, false
}

// DirFrom is Lookup plus the check a chooser needs: a TMPDIR that is not absolute is
// tmpdir_not_absolute. The directory is returned as written (not cleaned or resolved), with its
// source (SourceEnv or SourceDefault). Nothing is chosen in place of a refused value.
func DirFrom(getenv func(string) string) (dir, source string, err error) {
	v, fromEnv := Lookup(getenv)
	if !fromEnv {
		return v, SourceDefault, nil
	}
	if !filepath.IsAbs(v) {
		return "", SourceEnv, fmt.Errorf("tmpdir_not_absolute: TMPDIR=%q; set TMPDIR to an absolute directory", v)
	}
	return v, SourceEnv, nil
}

// Dir is DirFrom over the process environment.
func Dir() (string, error) {
	dir, _, err := DirFrom(os.Getenv)
	return dir, err
}

// Env is TMPDIR's raw value from the process environment ("" when unset), for records that keep
// what the caller had (ci local's CallerTmpDir).
func Env() string {
	return os.Getenv("TMPDIR")
}

// unusable is the one form of a temporary directory that cannot be used.
func unusable(dir, source string, why error) error {
	return fmt.Errorf("temp_dir_unusable: %s (%s): %w; set TMPDIR to a writable directory", dir, source, why)
}

// MkdirTemp is os.MkdirTemp below Dir(). A failure names the directory and its source.
func MkdirTemp(pattern string) (string, error) {
	dir, source, err := DirFrom(os.Getenv)
	if err != nil {
		return "", err
	}
	made, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", unusable(dir, source, err)
	}
	return made, nil
}

// CreateTemp is os.CreateTemp below Dir(). A failure names the directory and its source.
func CreateTemp(pattern string) (*os.File, error) {
	dir, source, err := DirFrom(os.Getenv)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, unusable(dir, source, err)
	}
	return f, nil
}

// MkdirTempIn is os.MkdirTemp(parent, pattern) that refuses an empty or relative parent
// (temp_parent_not_absolute) before anything is made, so "" never means "TMPDIR, decided
// elsewhere". A site whose parent is a variable that may be empty uses it.
func MkdirTempIn(parent, pattern string) (string, error) {
	if !filepath.IsAbs(parent) {
		return "", fmt.Errorf("temp_parent_not_absolute: parent %q; give an absolute directory", parent)
	}
	return os.MkdirTemp(parent, pattern)
}

// Check returns Dir() resolved (symbolic links followed) once it is known to be a directory this
// process can write: a probe directory is made in it and removed. A refusal is temp_dir_unusable
// naming the directory as written, its source and why; a TMPDIR that is not absolute is DirFrom's
// tmpdir_not_absolute.
func Check() (string, error) {
	dir, source, err := DirFrom(os.Getenv)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", unusable(dir, source, fmt.Errorf("does not exist: %w", err))
		}
		if errors.Is(err, syscall.ENOTDIR) {
			return "", unusable(dir, source, fmt.Errorf("is not a directory: %w", err))
		}
		return "", unusable(dir, source, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", unusable(dir, source, err)
	}
	if !fi.IsDir() {
		return "", unusable(dir, source, fmt.Errorf("is not a directory (%s is a %s)", resolved, kind(fi.Mode())))
	}
	probe, err := os.MkdirTemp(resolved, ".tmploc-probe-")
	if err != nil {
		return "", unusable(dir, source, fmt.Errorf("is not writable: %w", err))
	}
	if err := os.Remove(probe); err != nil {
		return "", unusable(dir, source, fmt.Errorf("the write probe %s could not be removed: %w", probe, err))
	}
	return resolved, nil
}

// kind names a non-directory file mode for a refusal.
func kind(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "regular file"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	default:
		return "non-directory (" + m.Type().String() + ")"
	}
}
