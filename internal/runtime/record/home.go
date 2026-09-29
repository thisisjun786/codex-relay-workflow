package record

import (
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"strings"
)

// Environ is os.environ.get with presence: HOME set to "" is not HOME unset, and the two expand
// differently (os.path.expanduser reads an empty HOME as the root, an unset one from passwd).
type Environ func(string) (string, bool)

// Getenv is an Environ over a getenv that cannot tell unset from empty: an empty value is unset.
func Getenv(getenv func(string) string) Environ {
	return func(key string) (string, bool) {
		value := getenv(key)
		return value, value != ""
	}
}

// ErrNoHome is pathlib's RuntimeError("Could not determine home directory."): a leading ~ or
// ~user that no HOME and no passwd entry answers.
var ErrNoHome = errors.New("could not determine home directory")

// currentHome is pwd.getpwuid(os.getuid()).pw_dir.
func currentHome() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" {
		return "", errors.New("the passwd entry names no home directory")
	}
	return u.HomeDir, nil
}

// ExpandUser is Path(path).expanduser() as Python 3.14 answers it over os.path.expanduser: a
// leading ~ is HOME when HOME is set at all (an empty HOME is the root), else the passwd entry
// of this user; ~user is that user's passwd entry; anything else is unchanged. Where pathlib
// raises, because nothing answers the ~ or ~user, this is ErrNoHome rather than the path left
// as written: a literal "~" is a relative path, which would be read against the working
// directory.
func ExpandUser(path string, lookup Environ) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	end := strings.IndexByte(path, '/')
	if end < 0 {
		end = len(path)
	}
	var home string
	if end == 1 {
		if value, set := lookup("HOME"); set {
			home = value
		} else {
			found, err := currentHome()
			if err != nil {
				return "", fmt.Errorf("%w: HOME is not set and this user's passwd entry could not be read (%v)", ErrNoHome, err)
			}
			home = found
		}
	} else {
		u, err := user.Lookup(path[1:end])
		if err != nil {
			return "", fmt.Errorf("%w for %s: %v", ErrNoHome, path[:end], err)
		}
		home = u.HomeDir
	}
	expanded := strings.TrimRight(home, "/") + path[end:]
	if expanded == "" {
		expanded = "/"
	}
	return expanded, nil
}

// Home is Path.home(): the expansion of "~".
func Home(lookup Environ) (string, error) { return ExpandUser("~", lookup) }

// StateHomeOf is hostrecord.state_home: Path($XDG_STATE_HOME).expanduser(), else
// Path($HOME, or "~" when HOME is not set).expanduser()/.local/state. One Python would read
// relative to its working directory (an empty or relative HOME, a relative XDG_STATE_HOME) is
// refused here with the home it could not establish, because the record read there would be
// another directory's, and its absence would read as a clean host.
func StateHomeOf(lookup Environ) (string, error) {
	var home string
	if base, _ := lookup("XDG_STATE_HOME"); base != "" {
		expanded, err := ExpandUser(base, lookup)
		if err != nil {
			return "", fmt.Errorf("XDG_STATE_HOME=%s: %w", base, err)
		}
		home = expanded
	} else {
		base, set := lookup("HOME")
		if !set {
			base = "~"
		}
		expanded, err := ExpandUser(base, lookup)
		if err != nil {
			return "", err
		}
		home = filepath.Join(expanded, ".local", "state")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("%w: the state home %s is not an absolute path, so it would be read wherever this command runs", ErrNoHome, home)
	}
	return home, nil
}

// StateHome is StateHomeOf over a getenv, for a caller with no way to report the failure: a home
// that cannot be established leaves the spelling unexpanded, as before.
func StateHome(getenv func(string) string) string {
	if home, err := StateHomeOf(Getenv(getenv)); err == nil {
		return home
	}
	if base := getenv("XDG_STATE_HOME"); base != "" {
		return base
	}
	home := getenv("HOME")
	if home == "" {
		home = "~"
	}
	return filepath.Join(home, ".local", "state")
}

// PathOf is hostrecord.record_path, or why the state home it lives under is not established.
func PathOf(lookup Environ) (string, error) {
	home, err := StateHomeOf(lookup)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "codex-relay-workflow", Name), nil
}
