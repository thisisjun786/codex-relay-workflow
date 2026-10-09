package configguard

import "testing"

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
			back, changed := RestoreTableKeyExact(set, table, "enabled", prior)
			if !changed || back != content {
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
	if out, changed := RestoreTableKeyExact(none, table, "enabled", nil); changed || out != none {
		t.Fatalf("restore without table: %q %v", out, changed)
	}
	same := "[" + table + "]\nenabled = false\n"
	if out, _, changed := SetTableKeyExact(same, table, "enabled", false); changed || out != same {
		t.Fatalf("already false: %q %v", out, changed)
	}
}

func ptr[T any](v T) *T { return &v }
