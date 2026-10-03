package doctor

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// ServiceUnit is the default name of the user unit `crw install register-service` writes, and
// UnitOwnerLine the line that marks a unit file as the installer's own.
const (
	ServiceUnit   = "crw-relay.service"
	UnitOwnerLine = "X-CRW-Owner=crw-install"
)

// UnitDir is the directory a user unit is written in by default: ${XDG_CONFIG_HOME:-~/.config}/systemd/user,
// where a relative XDG_CONFIG_HOME is ignored as the XDG specification says.
func UnitDir(env scope.Env) (string, error) {
	if base := env.Get("XDG_CONFIG_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "systemd", "user"), nil
	}
	home, err := record.Home(environ(env))
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// alwaysActiveEvidence is what the alwaysActive field says. The field stays not_verified in every
// case: a unit file is a registration, and surviving a host restart is only established by one
// being observed. Only the default unit name and directory are read (a unit registered under
// another name or directory is not seen), and neither the unit's enabled state nor systemd is asked.
func alwaysActiveEvidence(env scope.Env) string {
	const tail = " No host restart was observed. Installation is not activation; this command enables no daemon."
	dir, err := UnitDir(env)
	if err != nil {
		return "the unit directory could not be established (" + err.Error() + "), so no registration was looked for." + tail
	}
	path := filepath.Join(dir, ServiceUnit)
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return "no relay unit file at " + path + ", so nothing registered by crw install register-service is set to start the relay when the user manager starts." + tail
	case err != nil:
		return "the unit file " + path + " could not be read (" + err.Error() + ")." + tail
	case !strings.Contains("\n"+string(raw), "\n"+UnitOwnerLine+"\n"):
		return "a file at " + path + " is not the installer's unit file (it lacks " + UnitOwnerLine + ")." + tail
	}
	return "the installer's relay unit file " + path + " exists; whether it is enabled, and whether the relay it starts is running, are not read here." + tail
}
