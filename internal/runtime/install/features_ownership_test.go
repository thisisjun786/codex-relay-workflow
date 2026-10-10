package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// CRW-1145: a repeated enable keeps each flag's first prior state and crw's ownership, so enable, enable, disable returns the
// flags crw turned on to their original state; an enable that changes nothing publishes nothing; a completed disable releases
// the ownership so the next enable starts a new baseline; and a flag that could not be disabled is reported with a nonzero
// exit.

func ownershipFlags(t *testing.T, h featureHome) map[string]bool {
	t.Helper()
	doc, err := tomledit.Decode(h.read("config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	features, _ := doc["features"].(map[string]any)
	for k, v := range features {
		if b, ok := v.(bool); ok {
			out[k] = b
		}
	}
	return out
}

func ownershipBackups(t *testing.T, h featureHome) []string {
	t.Helper()
	backups, err := filepath.Glob(filepath.Join(h.home, "config.toml.crw-*.bak"))
	if err != nil {
		t.Fatal(err)
	}
	return backups
}

func TestFeaturesEnableTwiceThenDisableRestoresTheOriginalFlags(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"all four off":          "[features]\nmulti_agent = false\ngoals = false\nhooks = false\ndefault_mode_request_user_input = false\n",
		"one on from the start": "[features]\nhooks = true\nundeclared_flag = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newFeatureHome(t, content)
			before := ownershipFlags(t, h)
			h.success("enable")
			for _, key := range configguard.DeclaredFeatures() {
				if !ownershipFlags(t, h)[string(key)] {
					t.Fatalf("enable left %s off", key)
				}
			}
			h.success("enable")
			out := h.success("disable")
			after := ownershipFlags(t, h)
			for _, key := range append(configguard.DeclaredFeatures(), "undeclared_flag") {
				if after[string(key)] != before[string(key)] {
					t.Fatalf("%s = %v after enable, enable, disable; it was %v (disable said %q)", key, after[string(key)], before[string(key)], out)
				}
			}
		})
	}
}

func TestFeaturesUnchangedEnablePublishesNothingAndDisableStartsANewBaseline(t *testing.T) {
	t.Parallel()
	h := newFeatureHome(t, "[features]\nmulti_agent = false\n")
	h.success("enable")
	manifest, config := h.read(configguard.InstallManifestName), h.read("config.toml")
	if n := len(ownershipBackups(t, h)); n != 1 {
		t.Fatalf("%d backups after the first enable", n)
	}
	out := h.success("enable")
	if n := len(ownershipBackups(t, h)); n != 1 {
		t.Fatalf("an enable that changed nothing wrote another backup (%d): %q", n, out)
	}
	if h.read(configguard.InstallManifestName) != manifest || h.read("config.toml") != config {
		t.Fatalf("an enable that changed nothing rewrote the manifest or config.toml: %q", out)
	}
	if strings.Contains(out, "backup:") {
		t.Fatalf("an enable that changed nothing named a backup: %q", out)
	}
	h.success("disable")
	if flags := ownershipFlags(t, h); flags["hooks"] || flags["goals"] || flags["multi_agent"] {
		t.Fatalf("disable left flags on: %v", flags)
	}
	// After the completed disable the user turns hooks on themselves; the next enable must take that as the new baseline,
	// so the disable after it leaves the user's hooks on.
	content := strings.Replace(h.read("config.toml"), "hooks = false", "hooks = true", 1)
	if !strings.Contains(content, "hooks = true") {
		t.Fatalf("no hooks line: %q", content)
	}
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h.success("enable")
	h.success("disable")
	if flags := ownershipFlags(t, h); !flags["hooks"] || flags["goals"] {
		t.Fatalf("the new baseline was not kept: %v", flags)
	}
	// A second disable of a released install changes nothing and asks Codex for nothing.
	if out := h.success("disable"); !strings.Contains(out, "nothing to revert") {
		t.Fatalf("second disable: %q", out)
	}
}

func TestFeaturesDisableFailureIsReportedNonzero(t *testing.T) {
	t.Parallel()
	h := newFeatureHome(t, "[features]\n")
	h.success("enable")
	h.env = h.env.With("CRW499_FAKE_DISABLE_FAIL", "1")
	code, out, errOut := h.run("disable")
	if code == 0 || !strings.Contains(errOut, "multi_agent") || !strings.Contains(errOut, "exit 2") {
		t.Fatalf("a failed disable was reported as success: exit %d stdout=%q stderr=%q", code, out, errOut)
	}
	// The ownership is not released: the retry disables the flags once Codex can.
	h.env = h.env.Without("CRW499_FAKE_DISABLE_FAIL")
	out = h.success("disable")
	if !strings.Contains(out, "disabled [multi_agent, goals, hooks, default_mode_request_user_input]") {
		t.Fatalf("retry: %q", out)
	}
}
