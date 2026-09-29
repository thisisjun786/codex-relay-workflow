package faults

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every observation's facts carry faultsweep.INSTALLATION.version, which is the relay
// package's __version__; the Go constant is read against the real package so a later bump
// cannot drift silently. ownership.PythonBuild is the fence identity and is not checked here.
func TestRelayPackageVersion_is_the_python_package_version(t *testing.T) {
	command := exec.Command("uv", "run", "--no-sync", "--project", f1Root(), "python", "-c",
		"import codex_session_relay, codex_session_relay.faultsweep as s; print(codex_session_relay.__version__); print(s.INSTALLATION['version'])")
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	python, err := command.Output()
	if err != nil {
		t.Fatalf("the Python reference is required: %v", err)
	}
	if lines := strings.Fields(string(python)); len(lines) != 2 || lines[0] != RelayPackageVersion || lines[1] != RelayPackageVersion {
		t.Fatalf("python %q, go %q", python, RelayPackageVersion)
	}
}
