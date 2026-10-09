package configguard

import (
	"errors"
	"testing"
)

// The oracle's editor normalises the line it rewrites, so SetTableKey then RestoreTableKey does not
// give `enabled=true` back; the exact pair does, for every line ending (CRW-201).
func TestExactEditGivesBackWhatTheOracleEditorNormalises(t *testing.T) {
	const table = `plugins."codexclaw@codexclaw"`
	for name, content := range map[string]string{
		"tight":  "[" + table + "]\nenabled=true\n",
		"tab":    "[" + table + "]\n\tenabled\t=\ttrue # c\n",
		"crlf":   "[" + table + "]\r\nenabled=true\r\nx = 1\r\n",
		"mixed":  "[" + table + "]\r\nenabled=true\nx = 1\r\n",
		"no eol": "[" + table + "]\nenabled=true",
		"absent": "[" + table + "]\nx = 1\n\n[next]\ny = 2\n",
		"quoted": "[" + table + "]\n\"enabled\" = true\n",
		"single": "[" + table + "]\n  'enabled'=true # c\r\nx = 1\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			st := ReadTableKeyLine(content, table, "enabled")
			var prior *string
			if st.Found {
				prior = &st.Line
			}
			set, _, changed := SetTableKeyExact(content, table, "enabled", false)
			if !changed {
				t.Fatal("not changed")
			}
			if got := ReadTableKeyLine(set, table, "enabled"); !got.Found || got.Value != "false" {
				t.Fatalf("after set: %+v\n%q", got, set)
			}
			back, changed, err := RestoreTableKeyExact(set, table, "enabled", prior)
			if err != nil || !changed || back != content {
				t.Fatalf("exact restore = %q, want %q", back, content)
			}
			if name == "absent" {
				return
			}
			// The oracle pair, for contrast: set false, restore "true".
			oracleSet := SetTableKey(content, table, "enabled", false).Content
			oracle := RestoreTableKey(oracleSet, table, "enabled", ptr("true")).Content
			if name == "tight" && oracle == content {
				t.Fatal("the oracle editor was expected to normalise `enabled=true`; its behaviour changed")
			}
		})
	}
}

func TestExactEditRefusesWhatItCannotRewriteAndNeverCreatesATable(t *testing.T) {
	const table = `plugins."codexclaw@codexclaw"`
	odd := "[" + table + "]\nenabled = [true]\n"
	if out, st, changed := SetTableKeyExact(odd, table, "enabled", false); changed || out != odd || !st.Unsupported {
		t.Fatalf("unsupported: %q %+v %v", out, st, changed)
	}
	none := "[other]\nx = 1\n"
	if out, st, changed := SetTableKeyExact(none, table, "enabled", false); changed || out != none || st.TablePresent {
		t.Fatalf("missing table: %q %+v %v", out, st, changed)
	}
	if out, changed, err := RestoreTableKeyExact(none, table, "enabled", nil); err != nil || changed || out != none {
		t.Fatalf("restore without table: %q %v %v", out, changed, err)
	}
	if out, changed, err := RestoreTableKeyExact(none, table, "enabled", ptr("enabled = false")); !errors.Is(err, ErrRestoreTableKey) || changed || out != none {
		t.Fatalf("restore of a line whose table is gone: %q %v %v", out, changed, err)
	}
	if out, changed, err := RestoreTableKeyExact(odd, table, "enabled", ptr("enabled = true")); !errors.Is(err, ErrRestoreTableKey) || changed || out != odd {
		t.Fatalf("restore over an unsupported value: %q %v %v", out, changed, err)
	}
	twice := "[" + table + "]\nenabled = true\n'enabled' = true\n"
	if out, st, changed := SetTableKeyExact(twice, table, "enabled", false); changed || out != twice || !st.Unsupported {
		t.Fatalf("the key twice: %q %+v %v", out, st, changed)
	}
	same := "[" + table + "]\nenabled = false\n"
	if out, _, changed := SetTableKeyExact(same, table, "enabled", false); changed || out != same {
		t.Fatalf("already false: %q %v", out, changed)
	}
}

func ptr[T any](v T) *T { return &v }

// A key line deleted after the switch is put back at the end of its table (CRW-201 round 2).
func TestExactRestorePutsADeletedLineBack(t *testing.T) {
	const table = `plugins."codexclaw@codexclaw"`
	for name, c := range map[string]struct{ content, prior, want string }{
		"lf":   {"[" + table + "]\nx = 1\n\n[next]\n", "enabled = false # off", "[" + table + "]\nx = 1\nenabled = false # off\n\n[next]\n"},
		"crlf": {"[" + table + "]\r\nx = 1\r\n", "enabled=false\r", "[" + table + "]\r\nx = 1\r\nenabled=false\r\n"},
	} {
		t.Run(name, func(t *testing.T) {
			out, changed, err := RestoreTableKeyExact(c.content, table, "enabled", &c.prior)
			if err != nil || !changed || out != c.want {
				t.Fatalf("restore = %q %v %v, want %q", out, changed, err, c.want)
			}
		})
	}
}

// A spelling that is not valid TOML names no key and no table: it is never taken for enabled.
func TestExactEditIgnoresMalformedKeyAndHeaderSpellings(t *testing.T) {
	const table = `plugins."codexclaw@codexclaw"`
	for name, content := range map[string]string{
		"unknown escape":     "[" + table + "]\n\"en\\qabled\" = true\n",
		"short unicode":      "[" + table + "]\n\"en\\u61bled\" = true\n",
		"surrogate":          "[" + table + "]\n\"\\ud800nabled\" = true\n",
		"unterminated":       "[" + table + "]\n\"enabled = true\n",
		"dotted":             "[" + table + "]\nenabled.x = true\n",
		"other key":          "[" + table + "]\n\"enabled2\" = true\n",
		"array of tables":    "[[" + table + "]]\nenabled = true\n",
		"header with a tail": "[" + table + "] x\nenabled = true\n",
		"header not closed":  "[" + table + "\nenabled = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if st := ReadTableKeyLine(content, table, "enabled"); st.Found || st.Unsupported {
				t.Fatalf("read %+v", st)
			}
		})
	}
	if st := pluginTableStateOf(t, "[plugins.'codexclaw@codexclaw']\n\"enabled\" = false\n"); !st.Present || st.Enabled {
		t.Fatalf("quoted false = %+v", st)
	}
	if st := pluginTableStateOf(t, "[plugins.\"codexclaw@codexclaw\"]\nother = 1\n"); !st.Present || !st.Enabled {
		t.Fatalf("no key = %+v", st)
	}
	if st := pluginTableStateOf(t, "[plugins.\"codexclaw@codexclaw\"]\nenabled = \"false\"\n"); !st.Present || !st.Enabled || !st.Unsupported {
		t.Fatalf("string = %+v", st)
	}
	if st := pluginTableStateOf(t, "[other]\nenabled = false\n"); st.Present || st.Enabled {
		t.Fatalf("absent = %+v", st)
	}
}

func pluginTableStateOf(t *testing.T, content string) PluginTableState {
	t.Helper()
	return ReadPluginTableState(content, "codexclaw@codexclaw")
}
