package host

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
)

func TestCodexSQLiteHomePrecedence(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CODEX_SQLITE_HOME": "/sq", "CODEX_HOME": "/ch", "HOME": "/h"}, "/sq"},
		{map[string]string{"CODEX_HOME": "/ch", "HOME": "/h"}, "/ch"},
		{map[string]string{"CODEX_SQLITE_HOME": "", "CODEX_HOME": "", "HOME": "/h"}, "/h/.codex"}, // empty counts as unset
		{map[string]string{"HOME": ""}, ".codex"},                                                 // os.homedir() is empty and not repaired
	} {
		if got, err := CodexSQLiteHome(envOf(c.env)); err != nil || got != c.want {
			t.Errorf("%v: %q, %v", c.env, got, err)
		}
	}
}

// CRW_HOME resolves like CODEXCLAW_HOME in recall and skill-search: as given unless
// String.prototype.trim leaves nothing, else ~/.crw.
func TestCRWHome(t *testing.T) {
	for _, c := range []struct{ set, home, want string }{
		{"/data/crw", "/h", "/data/crw"}, {" /data/crw ", "/h", " /data/crw "}, {"", "/h", "/h/.crw"}, {"  \t", "/h", "/h/.crw"},
		{"\uFEFF", "/h", "/h/.crw"}, {"\u0085", "/h", "\u0085"}, {"", "", ".crw"},
	} {
		if got, err := CRWHome(envOf(map[string]string{"CRW_HOME": c.set, "HOME": c.home})); err != nil || got != c.want {
			t.Errorf("CRW_HOME=%q HOME=%q: %q, %v", c.set, c.home, got, err)
		}
	}
	if got, _ := CRWHome(envOf(map[string]string{"HOME": "/h"})); got != "/h/.crw" {
		t.Errorf("unset: %q", got)
	}
}

// CRW_BIN overrides, trimmed; otherwise the command is the runtime pointer's crw, an absolute path
// in double quotes as the oracle quotes its dispatcher. The destination <home>/.local/share/crw-runtime
// is also spelled in internal/runtime/install/cli.go:245 and internal/runtime/doctor/doctor.go:93.
func TestInvocation(t *testing.T) {
	command := func(home string) string {
		return `"` + filepath.Join(pointer.Path(filepath.Join(home, ".local", "share", "crw-runtime")), "bin", "crw") + `"`
	}
	passwd := func() (string, error) { return "/home/account", nil }
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CRW_BIN": "  node \"/x/crw.mjs\" \n"}, `node "/x/crw.mjs"`}, {map[string]string{"CRW_BIN": "\uFEFF/x/crw\u00a0"}, "/x/crw"},
		{map[string]string{"CRW_BIN": " \t", "HOME": "/home/it's u"}, command("/home/it's u")}, {map[string]string{"HOME": "/home/u"}, command("/home/u")},
		{map[string]string{"HOME": ""}, command("/home/account")}, {map[string]string{"HOME": "rel"}, command("/home/account")}, {nil, command("/home/account")},
	} {
		if got, err := invocationFrom(envOf(c.env), passwd); err != nil || got != c.want {
			t.Errorf("%v: %q, %v, want %q", c.env, got, err, c.want)
		}
	}
	for _, account := range []func() (string, error){func() (string, error) { return "", errors.New("no entry") }, func() (string, error) { return "rel", nil }} {
		if got, err := invocationFrom(envOf(map[string]string{"HOME": "rel"}), account); err == nil {
			t.Errorf("no absolute home, yet %q", got)
		}
	}
	if got, err := Invocation(envOf(map[string]string{"HOME": "/home/u"})); err != nil || got != command("/home/u") {
		t.Errorf("Invocation: %q, %v", got, err)
	}
}
