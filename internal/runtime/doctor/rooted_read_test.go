package doctor

// CRW-1015 d1: the file a manifest names is judged inside the plugin root and then read. The
// containment verdict is made on a path; a read that opens the same path again follows whatever
// link stands there by then. These tests swap a link inside the root for a link to a file outside it
// at the moment between the verdict and the read (doctorRootedReadSeam) and require that the outside
// file is never read.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	rootedOutsideMCP = `{"mcpServers":{"a":{"args":["leak-a.js"]},"b":{"args":["leak-b.js"]},"c":{}}}`
	rootedInsideMCP  = `{"mcpServers":{"only":{"command":"x"}}}`
)

// rootedPlugin builds a plugin root whose manifest names ./mcp-link.json, a link to the inside file
// real.json, and an outside file next to the plugin directory. It returns the plugin root, the link
// path and the outside file path.
func rootedPlugin(t *testing.T, manifest string) (plugin, link, outside string) {
	t.Helper()
	base := t.TempDir()
	plugin = filepath.Join(base, "plugin")
	outside = filepath.Join(base, "outside.json")
	harnessDriftWriteTree(t, plugin, map[string]string{
		".codex-plugin/plugin.json": manifest,
		"real.json":                 rootedInsideMCP,
	}, nil)
	if err := os.WriteFile(outside, []byte(rootedOutsideMCP), 0o644); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(plugin, "mcp-link.json")
	if err := os.Symlink("real.json", link); err != nil {
		t.Fatal(err)
	}
	return plugin, link, outside
}

// rootedSwap installs a seam that replaces link with a link to target once, at the first read of
// link, and reports through the returned counter how often the seam saw it.
func rootedSwap(t *testing.T, link, target string) *int {
	t.Helper()
	calls := new(int)
	doctorRootedReadSeam = func(path string) {
		if filepath.Base(path) != filepath.Base(link) {
			return
		}
		*calls++
		if *calls != 1 {
			return
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { doctorRootedReadSeam = nil })
	return calls
}

func TestHarnessDriftMCPReadCannotLeaveTheRootWhenALinkIsSwapped(t *testing.T) {
	for _, form := range []string{"absolute", "relative"} {
		t.Run(form, func(t *testing.T) {
			plugin, link, outside := rootedPlugin(t, `{"version":"1.0.0","mcpServers":"./mcp-link.json"}`)
			target := outside
			if form == "relative" {
				target = filepath.Join("..", "outside.json")
			}
			calls := rootedSwap(t, link, target)
			checks := HarnessDriftChecks(plugin)
			if *calls == 0 {
				t.Fatal("the seam never ran: the MCP file was not read through the rooted read")
			}
			mcp := checks[1]
			if mcp.Name != "drift:mcp" {
				t.Fatalf("checks = %+v, want drift:mcp second", checks)
			}
			if mcp.Severity == HarnessPass || strings.Contains(mcp.Evidence, "server(s) declared") {
				t.Fatalf("drift:mcp = %+v: the file outside the plugin root was read", mcp)
			}
			if mcp.Severity != HarnessFail {
				t.Fatalf("drift:mcp = %+v, want FAIL", mcp)
			}
		})
	}
}

func TestHarnessDriftMCPReadFollowsAnInsideLink(t *testing.T) {
	plugin, _, _ := rootedPlugin(t, `{"version":"1.0.0","mcpServers":"./mcp-link.json"}`)
	checks := HarnessDriftChecks(plugin)
	want := HarnessCheck{Name: "drift:mcp", Severity: HarnessPass, Evidence: "./mcp-link.json parses, 1 server(s) declared"}
	if checks[1] != want {
		t.Fatalf("drift:mcp = %+v, want %+v", checks[1], want)
	}
}

func TestManifestTargetsMCPReadCannotLeaveTheRootWhenALinkIsSwapped(t *testing.T) {
	plugin, link, outside := rootedPlugin(t, `{"mcpServers":"./mcp-link.json"}`)
	calls := rootedSwap(t, link, outside)
	issues, err := ValidateManifestTargets(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if *calls == 0 {
		t.Fatal("the seam never ran: the MCP file was not read through the rooted read")
	}
	for _, issue := range issues {
		if strings.Contains(issue.Message, "leak-") {
			t.Fatalf("issues = %+v: the file outside the plugin root was read", issues)
		}
	}
	if len(issues) != 1 || issues[0].Kind != TargetMCP || !strings.Contains(issues[0].Message, "escapes plugin root") {
		t.Fatalf("issues = %+v, want the one escape finding", issues)
	}
}

func TestManifestTargetsHookFileReadCannotLeaveTheRootWhenALinkIsSwapped(t *testing.T) {
	plugin, link, outside := rootedPlugin(t, `{"hooks":["./mcp-link.json"]}`)
	if err := os.WriteFile(outside, []byte(`{"hooks":{"Stop":[{"hooks":[{"command":"${PLUGIN_ROOT}/leak-hook.sh"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := rootedSwap(t, link, outside)
	issues, err := ValidateManifestTargets(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if *calls == 0 {
		t.Fatal("the seam never ran: the hook file was not read through the rooted read")
	}
	for _, issue := range issues {
		if strings.Contains(issue.Message, "leak-") {
			t.Fatalf("issues = %+v: the file outside the plugin root was read", issues)
		}
	}
	if len(issues) != 1 || issues[0].Kind != TargetHook || !strings.Contains(issues[0].Message, "escapes plugin root") {
		t.Fatalf("issues = %+v, want the one escape finding", issues)
	}
}

// The Root refuses an absolute link even when it points back inside the plugin root: the read is
// bound to the root by relative resolution only, so the check answers the fail-closed direction
// (hookTrustEntriesReadContained does the same). A relative link that stays inside is followed
// (TestHarnessDriftMCPReadFollowsAnInsideLink).
func TestHarnessDriftMCPReadRefusesAnAbsoluteLinkBackInsideTheRoot(t *testing.T) {
	plugin, link, _ := rootedPlugin(t, `{"version":"1.0.0","mcpServers":"./mcp-link.json"}`)
	real, err := filepath.EvalSymlinks(filepath.Join(plugin, "real.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	checks := HarnessDriftChecks(plugin)
	want := HarnessCheck{Name: "drift:mcp", Severity: HarnessFail, Evidence: "mcpServers -> ./mcp-link.json resolves outside the plugin root"}
	if checks[1] != want {
		t.Fatalf("drift:mcp = %+v, want %+v", checks[1], want)
	}
}

// A link that leaves the root toward a file that does not exist is missing, not an escape, and a
// directory named as the file is the read's own error (EISDIR), as the stat-then-read it replaces
// answered.
func TestDoctorRootedReadClassifiesMissingAndDirectory(t *testing.T) {
	plugin, link, _ := rootedPlugin(t, `{}`)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "nowhere.json"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := doctorRootedRead(plugin, link); !errors.Is(err, errDoctorRootMissing) {
		t.Fatalf("a link leaving the root toward nothing: err = %v, want missing", err)
	}
	if err := os.Mkdir(filepath.Join(plugin, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := doctorRootedRead(plugin, filepath.Join(plugin, "dir")); !errors.Is(err, syscall.EISDIR) || errors.Is(err, errDoctorRootMissing) {
		t.Fatalf("a directory: err = %v, want EISDIR", err)
	}
	if _, err := doctorRootedRead(plugin, filepath.Join(plugin, "absent.json")); !errors.Is(err, errDoctorRootMissing) {
		t.Fatalf("an absent file: err = %v, want missing", err)
	}
	if _, err := doctorRootedRead(plugin, filepath.Join(filepath.Dir(plugin), "outside.json")); !errors.Is(err, errDoctorRootEscape) {
		t.Fatalf("a path outside the root: err = %v, want escape", err)
	}
}

// CRW-1015 E1: a file the root can see but cannot open (mode 000 for an unprivileged user) is not
// absent. Only a name the root cannot reach is "missing"; an open that fails for any other reason
// is the read's own error, as the stat-then-ReadFile it replaces gave it.
func rootedUnreadable(t *testing.T) (plugin, file string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode 000 file")
	}
	plugin, link, _ := rootedPlugin(t, `{"version":"1.0.0","mcpServers":"./real.json"}`)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(plugin, "real.json")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o644) })
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the fixture file must stat: %v", err)
	}
	return plugin, file
}

func TestDoctorRootedReadKeepsAnOpenFailureThatIsNotAbsence(t *testing.T) {
	plugin, file := rootedUnreadable(t)
	_, err := doctorRootedRead(plugin, file)
	if err == nil {
		t.Fatal("a mode 000 file was read")
	}
	if errors.Is(err, errDoctorRootMissing) || errors.Is(err, errDoctorRootEscape) {
		t.Fatalf("err = %v, want the read's own permission error, not a missing or escape verdict", err)
	}
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want EACCES", err)
	}
}

func TestHarnessDriftMCPUnreadableFileIsNotReportedMissing(t *testing.T) {
	plugin, _ := rootedUnreadable(t)
	checks := HarnessDriftChecks(plugin)
	mcp := checks[1]
	if mcp.Name != "drift:mcp" || mcp.Severity != HarnessFail {
		t.Fatalf("drift:mcp = %+v, want FAIL", mcp)
	}
	if strings.Contains(mcp.Evidence, "file is missing") || !strings.Contains(mcp.Evidence, "permission denied") {
		t.Fatalf("drift:mcp = %+v, want the permission failure, not missing", mcp)
	}
}

func TestManifestTargetsUnreadableFileIsAReadErrorNotMissing(t *testing.T) {
	plugin, _ := rootedUnreadable(t)
	issues, err := ValidateManifestTargets(plugin)
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("MCP file: issues = %+v, err = %v, want the EACCES read error", issues, err)
	}
	plugin, file := rootedUnreadable(t)
	manifest := filepath.Join(plugin, ".codex-plugin", "plugin.json")
	if werr := os.WriteFile(manifest, []byte(`{"hooks":["./real.json"]}`), 0o644); werr != nil {
		t.Fatal(werr)
	}
	_ = file
	issues, err = ValidateManifestTargets(plugin)
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("hook file: issues = %+v, err = %v, want the EACCES read error", issues, err)
	}
}
