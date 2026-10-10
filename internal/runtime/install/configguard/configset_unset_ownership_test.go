package configguard

import (
	"strings"
	"testing"
)

// CRW-1149: unset restores the value from before crw set the key only while crw still owns it: crw set it, the live value
// is still the one crw applied, and the provenance of an originally absent key holds. Otherwise it writes nothing, keeps the
// record, and says why; an explicit release drops the record and leaves config.toml alone.

func TestConfigUnsetRefusesAKeyCrwNoLongerOwns(t *testing.T) {
	for _, c := range []struct {
		name, original, edit, reason string
		set                          bool
	}{
		{"a changed scalar", configSetOriginal, "dedicated_tools = false", "changed", true},
		{"a changed string scalar", configSetOriginal, "dedicated_tools = 'user-choice'", "changed", true},
		{"a deleted key", configSetOriginal, "", "missing", true},
		{"a true to false override of an original false", "[memories]\ndedicated_tools = false\n", "dedicated_tools = false", "changed", true},
		{"a set that changed nothing", "[memories]\ndedicated_tools = true\n", "dedicated_tools = true", "not set by crw", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, path := configSetHome(t, c.original, true)
			value := c.set
			configSetApply(t, home, path, &value)
			content := activationRead(t, path)
			edited := content
			for _, line := range strings.SplitAfter(content, "\n") {
				if strings.HasPrefix(line, "dedicated_tools = ") {
					replacement := ""
					if c.edit != "" {
						replacement = c.edit + "\n"
					}
					edited = strings.Replace(content, line, replacement, 1)
				}
			}
			activationWrite(t, path, edited)
			manifest := activationRead(t, manifestPath(home))
			r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, nil)
			if err != nil || r.OK || !strings.Contains(r.Reason, c.reason) || !strings.Contains(r.Reason, "--release") {
				t.Fatalf("unset of a key crw no longer owns: %+v %v", r, err)
			}
			if activationRead(t, path) != edited || activationRead(t, manifestPath(home)) != manifest {
				t.Fatalf("a refused unset wrote: config %q", activationRead(t, path))
			}
			// The explicit release drops the record and leaves config.toml as it is.
			rel, err := ReleaseManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey)
			if err != nil || !rel.OK || rel.Changed {
				t.Fatalf("release %+v %v", rel, err)
			}
			if _, kept := configSetManifest(t, home).TableKeys[configSetKey]; kept || activationRead(t, path) != edited {
				t.Fatalf("release kept the record or wrote config.toml: %q", activationRead(t, path))
			}
		})
	}
}

func TestConfigUnsetRestoresAKeyCrwStillOwns(t *testing.T) {
	for _, original := range []string{configSetOriginal, "[memories]\ngenerate_memories = true\ndedicated_tools = false # mine\n"} {
		home, path := configSetHome(t, original, true)
		value := true
		configSetApply(t, home, path, &value)
		r := configSetApply(t, home, path, nil)
		if !r.Changed || activationRead(t, path) != original {
			t.Fatalf("unset did not restore %q exactly: %+v %q", original, r, activationRead(t, path))
		}
		if _, kept := configSetManifest(t, home).TableKeys[configSetKey]; kept {
			t.Fatal("a completed unset kept the record")
		}
	}
}

func TestConfigUnsetRefusesAnUnprovenOriginalAbsence(t *testing.T) {
	home, path := configSetHome(t, configSetOriginal, true)
	value := true
	configSetApply(t, home, path, &value)
	// Another setting changed since, and no backup proves the key was absent before crw: removing it could delete a value a
	// user wrote with the same text.
	edited := "# another edit\n" + activationRead(t, path)
	activationWrite(t, path, edited)
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, nil)
	if err != nil || r.OK || !strings.Contains(r.Reason, "unverifiable") || activationRead(t, path) != edited {
		t.Fatalf("%+v %v", r, err)
	}
}
