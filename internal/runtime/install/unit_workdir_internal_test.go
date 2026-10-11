package install

import (
	"strings"
	"testing"
)

// CRW-1265. The user manager starts a unit that names no directory in the account home, and a relay
// that kept that cwd would be a candidate sandbox root the rule refuses. The unit starts the relay in
// its state directory; a unit written before this line existed is still the installer's own to remove.
func TestRenderUnitSetsWorkingDirectoryToState(t *testing.T) {
	for _, scope := range []string{"", "/srv/scope"} {
		for _, config := range []string{"", "/etc/crw/config.json"} {
			checkWorkingDirectory(t, scope, config)
		}
	}
}

func checkWorkingDirectory(t *testing.T, scope, config string) {
	t.Helper()
	{
		text := renderUnit("/opt/crw/bin/relay", "/var/lib/relay-state", "/run/app.sock", scope, config)
		lines := strings.Split(text, "\n")
		var service, workdir, execStart = -1, -1, -1
		for i, line := range lines {
			switch {
			case line == "[Service]":
				service = i
			case line == "WorkingDirectory=/var/lib/relay-state":
				workdir = i
			case strings.HasPrefix(line, "ExecStart="):
				execStart = i
			}
		}
		if service < 0 || workdir <= service || execStart <= workdir {
			t.Fatalf("scope %q config %q: the [Service] section has no WorkingDirectory= the state directory before ExecStart:\n%s", scope, config, text)
		}
		if strings.Count(text, "WorkingDirectory=") != 1 {
			t.Fatalf("scope %q config %q: more than one WorkingDirectory:\n%s", scope, config, text)
		}
		if !rendered(text) {
			t.Fatalf("scope %q config %q: the installer does not recognise its own unit", scope, config)
		}
		legacy := renderUnitText("/opt/crw/bin/relay", "/var/lib/relay-state", "/run/app.sock", scope, config, false)
		if strings.Contains(legacy, "WorkingDirectory=") || !rendered(legacy) {
			t.Fatalf("scope %q config %q: the unit written before CRW-1265 is not recognised for removal:\n%s", scope, config, legacy)
		}
	}
}
