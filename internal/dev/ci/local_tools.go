//go:build dev

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// CRW-964: the pinned tool versions and the ones this host actually has. The pins come from the
// files that fix them (go.mod's toolchain, ci.yml's Node, scripts/ci/secrets.sh's Gitleaks, the
// staticcheck module go.mod requires); a tool whose observed version differs from its pin is
// named in the record's pinMismatch and that record is never reused (answer 3).

// localToolNames is the tools the record names, in a stable order.
var localToolNames = []string{"go", "node", "gitleaks", "staticcheck"}

var (
	goModToolchain = regexp.MustCompile(`(?m)^toolchain go([0-9][^\s]*)$`)
	goModRequire   = regexp.MustCompile(`(?m)^\s*(honnef\.co/go/tools)\s+v([0-9][^\s]*)$`)
	ciNodeVersion  = regexp.MustCompile(`(?m)^\s*node-version: '([^']+)'$`)
	secretsVersion = regexp.MustCompile(`(?m)^scan_version=([0-9][^\s]*)$`)
	goVersionLine  = regexp.MustCompile(`(?m)^go version go([0-9][^\s]*)`)
)

// localToolPins reads the pinned versions from the verified tree. A pin the tree does not carry
// is left empty rather than guessed.
func localToolPins(root string) (map[string]string, error) {
	pins := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		if m := goModToolchain.FindSubmatch(data); m != nil {
			pins["go"] = string(m[1])
		}
		if m := goModRequire.FindSubmatch(data); m != nil {
			pins["staticcheck"] = string(m[2])
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml")); err == nil {
		if m := ciNodeVersion.FindSubmatch(data); m != nil {
			pins["node"] = string(m[1])
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "scripts", "ci", "secrets.sh")); err == nil {
		if m := secretsVersion.FindSubmatch(data); m != nil {
			pins["gitleaks"] = string(m[1])
		}
	}
	return pins, nil
}

// localToolVersions observes the tools this host has, resolving each through PATH. A tool that is
// absent is reported with an empty version, so a step that needs it fails as missing_tool rather
// than passing.
func localToolVersions(pathEnv string) map[string]string {
	versions := map[string]string{}
	for _, name := range localToolNames {
		versions[name] = localObserveTool(name, pathEnv)
	}
	return versions
}

// localObservedVersions is what the steps actually used. Two tools are fetched by the step rather
// than inherited: secrets.sh downloads the pinned Gitleaks, and the lint leg runs staticcheck
// through the module the tree requires. When the host has no copy, the record names the pin the
// step will fetch, so the field says what ran rather than staying empty.
func localObservedVersions(versions, pins map[string]string) map[string]string {
	for _, name := range []string{"gitleaks", "staticcheck"} {
		if versions[name] == "" && pins[name] != "" {
			versions[name] = pins[name]
		}
	}
	return versions
}

// localObserveTool is one tool's version, or "" when it is not on PATH or does not answer.
func localObserveTool(name, pathEnv string) string {
	var args []string
	switch name {
	case "go":
		args = []string{"version"}
	case "node":
		args = []string{"--version"}
	case "gitleaks":
		args = []string{"version"}
	case "staticcheck":
		args = []string{"-version"}
	default:
		return ""
	}
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "PATH="+pathEnv)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return localParseToolVersion(name, string(out))
}

// localParseToolVersion reads a tool's version out of its own output.
func localParseToolVersion(name, output string) string {
	text := strings.TrimSpace(output)
	switch name {
	case "go":
		if m := goVersionLine.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	case "node":
		return strings.TrimPrefix(strings.SplitN(text, "\n", 2)[0], "v")
	case "gitleaks", "staticcheck":
		// Both print their version last (gitleaks: "8.30.1"; staticcheck: "version 0.8.1").
		fields := strings.Fields(text)
		if len(fields) > 0 {
			return strings.TrimPrefix(fields[len(fields)-1], "v")
		}
	}
	return ""
}

// localPinMismatch names every tool whose observed version differs from its pin. A tool the tree
// pins but the host does not have is a mismatch too: the run cannot claim the pinned toolchain.
//
// Two tools are fetched by the step rather than inherited from the host, so an absent host copy is
// not a mismatch: secrets.sh downloads the pinned Gitleaks and verifies its checksum itself, and
// the lint leg runs staticcheck through the module the tree requires. A host copy, when there is
// one, is still compared.
func localPinMismatch(pins, observed map[string]string) []string {
	var mismatches []string
	for _, name := range localToolNames {
		pin, pinned := pins[name]
		if !pinned || pin == "" {
			continue
		}
		seen := observed[name]
		if seen == "" && (name == "gitleaks" || name == "staticcheck") {
			continue
		}
		if seen != pin {
			mismatches = append(mismatches, name)
		}
	}
	return localSortedUnique(mismatches)
}
