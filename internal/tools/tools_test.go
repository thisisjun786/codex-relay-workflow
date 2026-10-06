package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinsOf reads the pins array out of a crw tools list report.
func pinsOf(t *testing.T, out string) []map[string]any {
	t.Helper()
	report := decodeObject(t, out)
	raw, ok := report["pins"].([]any)
	if !ok {
		t.Fatalf("crw tools list carries no pins array: %s", out)
	}
	pins := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a pins entry is not an object: %v", entry)
		}
		pins = append(pins, row)
	}
	return pins
}

// C3: path reports the installed executable and refuses a missing one, and list reports the pin
// table with each pin's install state in both states.
func TestPathAndListReportTheInstallState(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	withPin(t, testPin(hex.EncodeToString(sum[:])))
	release := newFakeRelease(t, archive)
	seams := &Seams{URLBase: release.server.URL}
	executable := filepath.Join(tree.toolsRoot, "gitleaks-8.30.1", "gitleaks")

	// Before the install: path refuses with not_installed, list says not installed.
	code, out, errOut := runTools(t, context.Background(), tree, seams, "path", "gitleaks")
	if code != 1 || out != "" || !strings.Contains(errOut, "not_installed") {
		t.Fatalf("path before install: exit %d stdout %q stderr %q", code, out, errOut)
	}
	code, out, errOut = runTools(t, context.Background(), tree, seams, "list")
	if code != 0 || errOut != "" {
		t.Fatalf("list before install: exit %d stderr %q", code, errOut)
	}
	before := pinsOf(t, out)
	if len(before) != 1 || before[0]["name"] != "gitleaks" || before[0]["installed"] != false || before[0]["path"] != "" {
		t.Fatalf("list before install = %v", before)
	}
	if before[0]["archive"] != "gitleaks_8.30.1_linux_x64.tar.gz" || before[0]["executable"] != "gitleaks" || before[0]["version"] != "8.30.1" {
		t.Errorf("list does not carry the pin table: %v", before[0])
	}

	if code, _, errOut := runTools(t, context.Background(), tree, seams, "install", "gitleaks"); code != 0 {
		t.Fatalf("install: exit %d stderr %q", code, errOut)
	}

	// After the install: path prints the executable and exits 0, list reports it installed.
	code, out, errOut = runTools(t, context.Background(), tree, seams, "path", "gitleaks")
	if code != 0 || out != executable+"\n" || errOut != "" {
		t.Fatalf("path after install: exit %d stdout %q stderr %q", code, out, errOut)
	}
	code, out, errOut = runTools(t, context.Background(), tree, seams, "list")
	if code != 0 || errOut != "" {
		t.Fatalf("list after install: exit %d stderr %q", code, errOut)
	}
	after := pinsOf(t, out)
	if len(after) != 1 || after[0]["installed"] != true || after[0]["path"] != executable {
		t.Fatalf("list after install = %v", after)
	}

	// A record that is gone reads as not installed.
	if err := os.Remove(filepath.Join(tree.toolsRoot, "gitleaks-8.30.1", "pin.json")); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runTools(t, context.Background(), tree, seams, "path", "gitleaks")
	if code != 1 || out != "" {
		t.Fatalf("path without a record: exit %d stdout %q", code, out)
	}
	code, out, _ = runTools(t, context.Background(), tree, seams, "list")
	if code != 0 {
		t.Fatalf("list without a record: exit %d", code)
	}
	if pinsOf(t, out)[0]["installed"] != false {
		t.Error("list reports an install whose record is gone")
	}
}

// The record is the source of truth: an executable without its pin.json is not an install.
func TestPathRequiresTheRecordNotJustTheExecutable(t *testing.T) {
	tree := newTestTree(t)
	installDir := filepath.Join(tree.toolsRoot, "gitleaks-8.30.1")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "gitleaks"), []byte("x\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runTools(t, context.Background(), tree, nil, "path", "gitleaks")
	if code != 1 || out != "" || !strings.Contains(errOut, "not_installed") {
		t.Fatalf("path with an executable but no record: exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// The command surface: -h/--help/help write the usage, an unknown verb, a missing name and an
// unknown tool name are usage errors.
func TestCommandSurface(t *testing.T) {
	tree := newTestTree(t)
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		code, out, errOut := runTools(t, context.Background(), tree, nil, args...)
		if code != 0 || errOut != "" || !strings.Contains(out, "usage: crw tools") || !strings.Contains(out, "install") {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
	for _, args := range [][]string{{}, {"nope"}, {"install"}, {"path"}, {"install", "nope"}, {"path", "nope"}, {"install", "gitleaks", "extra"}} {
		code, out, errOut := runTools(t, context.Background(), tree, nil, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "usage: crw tools") {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
	if _, _, errOut := runTools(t, context.Background(), tree, nil, "install", "nope"); !strings.Contains(errOut, "invalid tool") {
		t.Errorf("an unknown tool does not name the choice: %q", errOut)
	}
}

// The command reads the configured roots, so a tools_root the configuration overrides is honoured
// rather than re-derived from the environment.
func TestRootsComeFromTheConfiguration(t *testing.T) {
	tree := newTestTree(t)
	override := filepath.Join(tree.home, "elsewhere", "tools")
	config := filepath.Join(tree.home, "config", "crw", "config.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	document := "{\"schema\":\"crw-config/1\",\"paths\":{\"tools_root\":\"" + override + "\"}}"
	if err := os.WriteFile(config, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	withPin(t, testPin(hex.EncodeToString(sum[:])))
	release := newFakeRelease(t, archive)

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	want := filepath.Join(override, "gitleaks-8.30.1", "gitleaks")
	if code != 0 || errOut != "" || out != want+"\n" {
		t.Fatalf("install under an overridden tools_root: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the executable is not under the configured root: %v", err)
	}
}
