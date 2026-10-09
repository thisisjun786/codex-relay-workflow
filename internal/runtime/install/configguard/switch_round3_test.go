package configguard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The tests below are the findings of the CRW-201 pre-merge evaluation of 2f010b1b (d1, d3, d4, d6).

// d1 and d3: TOML identifies a key and a table by their decoded names, whatever the spelling. The
// switch edits the one key that is there; it never adds a second one, and it never mistakes a valid
// spelling of the CXC table for a missing plugin.
func TestSwitchReadsKeysAndTablesByTheirDecodedNames(t *testing.T) {
	for name, tc := range map[string]struct{ config, key string }{
		"escaped key":          {"[plugins.\"codexclaw@codexclaw\"]\n\"en\\u0061bled\" = true\n", `"en\u0061bled"`},
		"escaped key capital":  {"[plugins.\"codexclaw@codexclaw\"]\n\"\\U00000065nabled\" = true # c\n", `"\U00000065nabled"`},
		"key with spaces":      {"[plugins.\"codexclaw@codexclaw\"]\n  \"enabled\"\t=true\n", `"enabled"`},
		"literal table":        {"[plugins.'codexclaw@codexclaw']\nenabled = true\n", "enabled"},
		"spaced header":        {"[ plugins . \"codexclaw@codexclaw\" ]  # cxc\nenabled = true\n", "enabled"},
		"escaped table":        {"[plugins.\"codexclaw\\u0040codexclaw\"]\nenabled = true\n", "enabled"},
		"quoted plugins":       {"[\"plugins\".\"codexclaw@codexclaw\"]\n'enabled' = true\n", "'enabled'"},
		"literal and escapes":  {"[plugins.'codexclaw@codexclaw']\n\"en\\u0061bled\" = true\n", `"en\u0061bled"`},
		"tab inside the brace": {"[\tplugins.\"codexclaw@codexclaw\"\t]\nenabled = true\n", "enabled"},
	} {
		t.Run(name, func(t *testing.T) {
			home := switchHost(t, "[plugins.\"crw@crw\"]\nenabled = true\n\n"+tc.config)
			before := switchTree(t, home)
			r := switchMustRun(t, home, "crw")
			mid := activationRead(t, home+"/config.toml")
			if !r.ConfigChanged || !strings.Contains(mid, tc.key+" = false") || strings.Contains(mid, "enabled = false") && tc.key != "enabled" {
				t.Fatalf("config after switch crw (changed %v):\n%s", r.ConfigChanged, mid)
			}
			if strings.Count(mid, "true") != 1 { // only crw@crw's key is still true
				t.Fatalf("CXC is still enabled or a key was added:\n%s", mid)
			}
			switchMustRun(t, home, "cxc")
			after := switchTreeWithoutSwitchFiles(t, home)
			delete(before, InstallManifestName)
			if d := switchDiff(before, after); d != "" {
				t.Fatalf("host differs after the round trip:\n%s", d)
			}
		})
	}
}

// A key or a table that is not the one named, in a spelling that merely looks like it, is left alone.
func TestSwitchDoesNotTakeOtherNamesForTheKeyOrTheTable(t *testing.T) {
	const config = "[plugins.\"codexclaw@codexclaw2\"]\nenabled = true\n\n[plugins.\"codexclaw@codexclaw\"]\n\"enabled2\" = true\n\"x.enabled\" = true\nenabled.sub = 1\nenabled = true\n"
	home := switchHost(t, config)
	switchMustRun(t, home, "crw")
	got := activationRead(t, home+"/config.toml")
	want := strings.Replace(config, "enabled = true\n", "enabled = false\n", -1)
	want = strings.Replace(want, "codexclaw2\"]\nenabled = false", "codexclaw2\"]\nenabled = true", 1)
	if got != want {
		t.Fatalf("config = %q, want %q", got, want)
	}
}

// d6: the documented refusal. A string-valued enabled is not a boolean Codex reads; it is refused
// before anything is written, in both directions, exactly as a list is.
func TestSwitchRefusesAStringValuedEnabledKey(t *testing.T) {
	for _, value := range []string{`"true"`, `'true'`, `"false"`, `1`, `True`} {
		t.Run(value, func(t *testing.T) {
			home := switchHost(t, "[plugins.\"codexclaw@codexclaw\"]\nenabled = "+value+"\n")
			before := switchTree(t, home)
			if _, err := RunSwitch(switchDeps(home), "crw"); err == nil || !strings.Contains(err.Error(), "will not rewrite") {
				t.Fatalf("err = %v", err)
			}
			if d := switchDiff(before, switchTree(t, home)); d != "" {
				t.Fatalf("a refused switch wrote:\n%s", d)
			}
		})
	}
}

// d4: the installer's cancellation reaches the switch. A cancelled context writes nothing, whether it
// was cancelled before the command or while it waited for the lock or ran a step.
func TestSwitchHonoursTheInstallerCancellation(t *testing.T) {
	const config = "[plugins.\"codexclaw@codexclaw\"]\nenabled = true\n"
	t.Run("cancelled before", func(t *testing.T) {
		home := switchHost(t, config)
		before := switchTree(t, home)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		deps := switchDeps(home)
		deps.Ctx = ctx
		for _, target := range []string{"crw", "cxc"} {
			if _, err := RunSwitch(deps, target); !errors.Is(err, context.Canceled) {
				t.Fatalf("switch %s: err = %v, want context.Canceled", target, err)
			}
		}
		if d := switchDiff(before, switchTree(t, home)); d != "" {
			t.Fatalf("a cancelled switch wrote:\n%s", d)
		}
	})
	t.Run("cancelled at every step", func(t *testing.T) {
		for at := 1; ; at++ {
			home := switchHost(t, config)
			before := switchTree(t, home)
			ctx, cancel := context.WithCancel(context.Background())
			deps := switchDeps(home)
			deps.Ctx = ctx
			n := 0
			deps.Fail = func(step string) error {
				if n++; n == at {
					cancel()
				}
				return nil
			}
			_, err := RunSwitch(deps, "crw")
			cancel()
			if err == nil {
				if at < 4 {
					t.Fatalf("a switch cancelled at boundary %d succeeded", at)
				}
				break
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("boundary %d: err = %v", at, err)
			}
			if d := switchDiff(before, switchTree(t, home)); d != "" {
				t.Fatalf("boundary %d: a cancelled switch left changes:\n%s", at, d)
			}
		}
	})
}
