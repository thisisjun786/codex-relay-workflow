// Package tools installs crw's pinned external tools: a release archive whose sha256 the
// product fixes is fetched, verified before anything is written, and unpacked into
// <tools_root>/<name>-<version>/ with a pin.json record beside the executable. The pin table in
// pins.go is the single place a tool's version and digest are decided, and a test holds it to the
// version and digest scripts/ci/secrets.sh scans with.
package tools

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// ReleaseBase is where a pinned archive is fetched from when no seam overrides it. The archive's
// URL is this base, the release tag v<version> and the archive name, exactly the shape
// scripts/ci/secrets.sh builds for its own download.
const ReleaseBase = "https://github.com/gitleaks/gitleaks/releases/download"

// Pin is one pinned external tool: the release it comes from, the archive that carries it, the
// sha256 the archive must have, the executable inside it, and the platform the archive is built
// for.
type Pin struct {
	Name       string
	Version    string
	Archive    string
	SHA256     string
	Executable string
	GOOS       string
	GOARCH     string
}

// Pins is every pinned tool, in a fixed order so a report over it is deterministic. It is a
// variable so a test can stand a synthetic release in for the published one; production reads it
// as it is declared here.
var Pins = []Pin{{
	Name:       "gitleaks",
	Version:    "8.30.1",
	Archive:    "gitleaks_8.30.1_linux_x64.tar.gz",
	SHA256:     "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb",
	Executable: "gitleaks",
	GOOS:       "linux",
	GOARCH:     "amd64",
}}

// Lookup answers the pin for a tool name.
func Lookup(name string) (Pin, bool) {
	for _, pin := range Pins {
		if pin.Name == name {
			return pin, true
		}
	}
	return Pin{}, false
}

// Names is every pinned tool name, in the table's order, for a message that lists the choices.
func Names() []string {
	names := make([]string, 0, len(Pins))
	for _, pin := range Pins {
		names = append(names, pin.Name)
	}
	return names
}

// ArchiveName is the release asset this pin installs from.
func (p Pin) ArchiveName() string { return p.Archive }

// ArchiveURL is where the archive is published: the release base, the tag v<version> and the
// archive name.
func (p Pin) ArchiveURL() string {
	return ReleaseBase + "/v" + p.Version + "/" + p.Archive
}

// Supports reports whether this pin's archive is built for a platform.
func (p Pin) Supports(goos, goarch string) bool {
	return p.GOOS == goos && p.GOARCH == goarch
}

// DirName is the directory the pin installs into under tools_root.
func (p Pin) DirName() string { return p.Name + "-" + p.Version }

// InstallDir is where the pin installs under a tools root. The path is joined without cleaning,
// through crwconfig.JoinRoot, so a tools_root whose spelling mixes a symbolic link and ".." names
// the directory the filesystem resolves it to rather than the one filepath.Clean would name.
func (p Pin) InstallDir(toolsRoot string) string { return crwconfig.JoinRoot(toolsRoot, p.DirName()) }

// ExecutablePath is the installed executable's path under a tools root.
func (p Pin) ExecutablePath(toolsRoot string) string {
	return crwconfig.JoinRoot(p.InstallDir(toolsRoot), p.Executable)
}

// RecordPath is the installed record's path under a tools root.
func (p Pin) RecordPath(toolsRoot string) string {
	return crwconfig.JoinRoot(p.InstallDir(toolsRoot), recordFile)
}

// LockPath is the file this pin's installs take an exclusive lock on while they change the install
// directory. It sits beside the install directory under the tools root, named after the pin, and it
// is never removed: two callers must lock the same inode, and unlinking it would let a later caller
// create and lock a different one.
func (p Pin) LockPath(toolsRoot string) string {
	return crwconfig.JoinRoot(toolsRoot, "."+p.DirName()+".lock")
}

// platformName renders a platform pair for a message.
func platformName(goos, goarch string) string { return goos + "/" + goarch }

// choiceList renders the pinned names for a message.
func choiceList() string {
	names := Names()
	out := ""
	for i, name := range names {
		if i > 0 {
			out += "', '"
		}
		out += name
	}
	return "'" + out + "'"
}

// describe renders a pin for a message.
func (p Pin) describe() string {
	return fmt.Sprintf("%s %s (%s)", p.Name, p.Version, platformName(p.GOOS, p.GOARCH))
}
