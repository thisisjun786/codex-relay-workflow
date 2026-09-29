package record_test

import (
	"errors"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

func environ(pairs ...string) record.Environ {
	values := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		values[pairs[i]] = pairs[i+1]
	}
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// Finding 40. hostrecord.state_home expands with pathlib: HOME when it is set (even to the root),
// this user's passwd entry when it is not, ~user from that user's entry. What pathlib cannot
// expand, and a state home that would be read relative to the working directory, is an error,
// never a relative path whose absence reads as a clean host.
func TestStateHomeExpandsTheHomeAsPathlibDoes(t *testing.T) {
	me, err := user.Current()
	if err != nil || me.HomeDir == "" || me.Username == "" {
		t.Skip("no passwd entry for this user")
	}
	for name, c := range map[string]struct {
		env  record.Environ
		want string
	}{
		"HOME unset":                 {environ(), filepath.Join(me.HomeDir, ".local", "state")},
		"HOME with a trailing slash": {environ("HOME", "/h/"), "/h/.local/state"},
		"XDG_STATE_HOME under ~":     {environ("HOME", "/h", "XDG_STATE_HOME", "~/s"), "/h/s"},
		"XDG_STATE_HOME under ~user": {environ("HOME", "/h", "XDG_STATE_HOME", "~"+me.Username+"/s"), filepath.Join(me.HomeDir, "s")},
		"an empty XDG_STATE_HOME":    {environ("HOME", "/h", "XDG_STATE_HOME", ""), "/h/.local/state"},
	} {
		if got, err := record.StateHomeOf(c.env); err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, c.want)
		}
	}
	for name, env := range map[string]record.Environ{
		"an unknown ~user":        environ("XDG_STATE_HOME", "~no-such-user-crw-record/s"),
		"an empty HOME":           environ("HOME", ""),
		"a relative HOME":         environ("HOME", "home"),
		"a relative state home":   environ("HOME", "/h", "XDG_STATE_HOME", "state"),
		"HOME naming no one's ~u": environ("HOME", "~no-such-user-crw-record"),
	} {
		if got, err := record.StateHomeOf(env); !errors.Is(err, record.ErrNoHome) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	if got := record.StateHome(func(string) string { return "" }); got != filepath.Join(me.HomeDir, ".local", "state") {
		t.Errorf("StateHome without HOME = %q", got)
	}
	if got, err := record.PathOf(environ("XDG_STATE_HOME", "/s")); err != nil || got != "/s/codex-relay-workflow/host-record.json" {
		t.Errorf("PathOf = %q, %v", got, err)
	}
	for spelling, want := range map[string]string{"~": me.HomeDir, "/x": "/x", "~" + me.Username: me.HomeDir} {
		if got, err := record.ExpandUser(spelling, environ()); err != nil || got != want {
			t.Errorf("ExpandUser(%q) = %q, %v", spelling, got, err)
		}
	}
	if got, err := record.Home(environ("HOME", "")); err != nil || got != "/" {
		t.Errorf("Home with an empty HOME = %q, %v; Path.home() answers /", got, err)
	}
}
