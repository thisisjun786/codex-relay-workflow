package install_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// CRW-958 fix round 3: the unit carries the configuration file the registration used. A user manager
// does not have the registering shell's CRW_CONFIG or XDG_CONFIG_HOME, so the unit names the file as its
// own Environment=CRW_CONFIG= line, and the relay a boot start runs reads the same log_root, tool roots
// and relay_state. A file found at the default location is found by the manager too, and a missing
// file configures nothing: neither adds a line.
func TestRegisterCarriesTheSelectedConfigurationFile(t *testing.T) {
	t.Parallel()
	u := newUnitHost(t)
	config := filepath.Join(u.home, "custom", "crw.json")
	put(t, config, `{"paths":{"log_root":"`+filepath.Join(u.home, "logs")+`"}}`, 0o600)
	u.o.Env = append(scope.Env{}, u.o.Env...)
	u.o.Env = append(u.o.Env, "CRW_CONFIG="+config)
	u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	if !strings.Contains(u.text(), "\nEnvironment=CRW_CONFIG="+config+"\n") {
		t.Fatalf("the unit does not carry the configuration file:\n%s", u.text())
	}
	u.run(install.ServiceOptions{}, install.OK, install.UnitUnchanged)
	// the installer's own unit with the line is still one it can remove
	u.run(install.ServiceOptions{Remove: true}, install.OK, install.UnitRemoved)
	if _, err := os.Stat(u.path()); err == nil {
		t.Fatal("the unit with a configuration line was not removed")
	}
}

func TestRegisterCarriesTheConfigurationFileOfXDGConfigHome(t *testing.T) {
	t.Parallel()
	u := newUnitHost(t)
	xdg := filepath.Join(u.home, "xdg")
	config := filepath.Join(xdg, "crw", "config.json")
	put(t, config, `{}`, 0o600)
	u.o.Env = append(append(scope.Env{}, u.o.Env...), "XDG_CONFIG_HOME="+xdg)
	u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	if !strings.Contains(u.text(), "\nEnvironment=CRW_CONFIG="+config+"\n") {
		t.Fatalf("the unit does not carry the configuration file of XDG_CONFIG_HOME:\n%s", u.text())
	}
}

func TestRegisterAddsNoConfigurationLineWithoutASelectedFile(t *testing.T) {
	t.Parallel()
	// nothing selected, a default file: the manager finds it itself
	u := newUnitHost(t)
	put(t, filepath.Join(u.home, ".config", "crw", "config.json"), `{}`, 0o600)
	u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	if strings.Contains(u.text(), "CRW_CONFIG") {
		t.Fatalf("the unit names a configuration file the manager finds by itself:\n%s", u.text())
	}
	// selected through CRW_CONFIG, file missing: nothing is configured
	m := newUnitHost(t)
	m.o.Env = append(append(scope.Env{}, m.o.Env...), "CRW_CONFIG="+filepath.Join(m.home, "no-such.json"))
	m.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	if strings.Contains(m.text(), "CRW_CONFIG") {
		t.Fatalf("the unit names a configuration file that does not exist:\n%s", m.text())
	}
}

// A path systemd would split or expand is refused, as the state directory and socket are.
func TestRegisterRefusesAConfigurationPathSystemdWouldSplit(t *testing.T) {
	t.Parallel()
	u := newUnitHost(t)
	config := filepath.Join(u.home, "my config", "crw.json")
	put(t, config, `{}`, 0o600)
	u.o.Env = append(append(scope.Env{}, u.o.Env...), "CRW_CONFIG="+config)
	u.run(install.ServiceOptions{}, install.Usage, "usage")
}
