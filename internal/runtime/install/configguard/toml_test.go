package configguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The tests are the 17 B-class tests of CXC v0.2.40 config-guard/test/toml-edit.test.ts (the two managed-keys policy tests of that file
// belong to the managed-keys port), the branch and line-ending cases the oracle's tests leave implicit, and a replay of
// testdata/oracle-toml-edit.json: what the v0.2.40 build answered over the grids of testdata/record-toml-edit-oracle.mjs, recorded once
// under Node (no Node runs here).

const tomlLF = "[memories]\ngenerate_memories = true\nuse_memories = true\n"

func tomlRef(s string) *string { return &s }

func tomlShow(p *string) string {
	if p == nil {
		return "nil"
	}
	return strconv.Quote(*p)
}

// tomlWant is the whole expected outcome of an edit.
type tomlWant struct {
	content string
	prior   *string
	changed bool
	action  TomlEditAction
}

func tomlCheck(t *testing.T, got TomlEditResult, want tomlWant) {
	t.Helper()
	if got.Content != want.content {
		t.Errorf("content = %q, want %q", got.Content, want.content)
	}
	if !reflect.DeepEqual(got.PriorValue, want.prior) {
		t.Errorf("priorValue = %s, want %s", tomlShow(got.PriorValue), tomlShow(want.prior))
	}
	if got.Changed != want.changed || got.Action != want.action {
		t.Errorf("changed, action = %v, %q; want %v, %q", got.Changed, got.Action, want.changed, want.action)
	}
}

func TestPortedCases(t *testing.T) {
	const table, key = "memories", "dedicated_tools"
	t.Run("case 1: inserts into an existing table, leaving the existing keys alone", func(t *testing.T) {
		r := SetTableKey(tomlLF, table, key, true)
		tomlCheck(t, r, tomlWant{tomlLF + "dedicated_tools = true\n", nil, true, TomlInsertedIntoTable})
		lines := text.SplitLines(r.Content) // inserted inside [memories], not appended after the file
		if slices.Index(lines, "dedicated_tools = true") <= slices.Index(lines, "[memories]") {
			t.Errorf("the key is not after the header: %q", lines)
		}
	})
	t.Run("case 2: updates an existing key and reports the prior value", func(t *testing.T) {
		tomlCheck(t, SetTableKey(tomlLF+"dedicated_tools = false\n", table, key, true),
			tomlWant{tomlLF + "dedicated_tools = true\n", tomlRef("false"), true, TomlUpdated})
	})
	t.Run("case 3: a user comment tail on the key line survives the rewrite", func(t *testing.T) {
		tomlCheck(t, SetTableKey("[memories]\ndedicated_tools = false  # 사용자 주석\n", table, key, true),
			tomlWant{"[memories]\ndedicated_tools = true  # 사용자 주석\n", tomlRef("false"), true, TomlUpdated})
	})
	t.Run("case 4: creates the table at the end without disturbing earlier content", func(t *testing.T) {
		tomlCheck(t, SetTableKey("[features]\nmulti_agent = true\n", table, key, true),
			tomlWant{"[features]\nmulti_agent = true\n\n[memories]\ndedicated_tools = true\n", nil, true, TomlCreatedTable})
	})
	t.Run("case 5: a CRLF file stays CRLF (no bare LF survives)", func(t *testing.T) {
		r := SetTableKey(strings.ReplaceAll(tomlLF, "\n", "\r\n"), table, key, true)
		tomlCheck(t, r, tomlWant{"[memories]\r\ngenerate_memories = true\r\nuse_memories = true\r\ndedicated_tools = true\r\n", nil, true, TomlInsertedIntoTable})
		if strings.Contains(strings.ReplaceAll(r.Content, "\r\n", ""), "\n") {
			t.Errorf("a bare LF survived: %q", r.Content)
		}
	})
	t.Run("case 6: insertion never crosses into the following table", func(t *testing.T) {
		in := "[memories]\ngenerate_memories = true\n\n[features]\nmulti_agent = true\n"
		tomlCheck(t, SetTableKey(in, table, key, true),
			tomlWant{"[memories]\ngenerate_memories = true\ndedicated_tools = true\n\n[features]\nmulti_agent = true\n", nil, true, TomlInsertedIntoTable})
	})
	t.Run("case 7: writing the value it already has is an exact no-op", func(t *testing.T) {
		in := "[memories]\ndedicated_tools = true\n"
		tomlCheck(t, SetTableKey(in, table, key, true), tomlWant{in, tomlRef("true"), false, TomlNoop})
	})
	t.Run("case 8: a '#' inside a quoted value is not mistaken for a comment", func(t *testing.T) {
		found, state := FindKeyLine(text.SplitLines("[memories]\nnote = \"a # b\"\n"), 0, "note")
		if state != TomlKeyFound || found.Value != "\"a # b\"" || found.Comment != "" {
			t.Errorf("FindKeyLine = %+v, %v", found, state)
		}
	})
	t.Run("case 9: restore with priorValue=nil removes only the key line and keeps the header", func(t *testing.T) {
		tomlCheck(t, RestoreTableKey("[memories]\ngenerate_memories = true\ndedicated_tools = true\n", table, key, nil),
			tomlWant{"[memories]\ngenerate_memories = true\n", tomlRef("true"), true, TomlRemoved})
	})
	t.Run("case 9b: the header survives even when our key was the only one, with a comment kept", func(t *testing.T) {
		tomlCheck(t, RestoreTableKey("[memories]\n# 사용자가 남긴 메모\ndedicated_tools = true\n", table, key, nil),
			tomlWant{"[memories]\n# 사용자가 남긴 메모\n", tomlRef("true"), true, TomlRemoved})
	})
	t.Run("case 10: restore to a prior value puts that value back", func(t *testing.T) {
		tomlCheck(t, RestoreTableKey("[memories]\ndedicated_tools = true\n", table, key, tomlRef("false")),
			tomlWant{"[memories]\ndedicated_tools = false\n", tomlRef("true"), true, TomlUpdated})
	})
	t.Run("blocker 3: multi-line, literal, array and inline-table values are refused, not rewritten", func(t *testing.T) {
		for _, raw := range []string{"\"\"\"x\ny\"\"\"", "'''x'''", "[1, 2] # c", "{ a = \"#\" }"} {
			in := "[memories]\ndedicated_tools = " + raw + "\n"
			tomlCheck(t, SetTableKey(in, table, key, true), tomlWant{in, nil, false, TomlUnsupportedValue})
		}
	})
	t.Run("blocker 3: a '#' with no leading space still splits as a comment", func(t *testing.T) {
		tomlCheck(t, SetTableKey("[memories]\ndedicated_tools = false#c\n", table, key, true),
			tomlWant{"[memories]\ndedicated_tools = true#c\n", tomlRef("false"), true, TomlUpdated})
	})
	t.Run("blocker 3: an unterminated quote is refused rather than guessed at", func(t *testing.T) {
		in := "[memories]\ndedicated_tools = \"oops\n"
		tomlCheck(t, SetTableKey(in, table, key, true), tomlWant{in, nil, false, TomlUnsupportedValue})
	})
	t.Run("blocker 1: the shared grammar handles dotted headers and header comments", func(t *testing.T) {
		in := "[features.multi_agent_v2]  # tuning\nenabled = true\nmax_concurrent_threads_per_session = 8\n"
		if got := FindTableHeader(text.SplitLines(in), "features.multi_agent_v2"); got != 0 {
			t.Errorf("FindTableHeader = %d, want 0", got)
		}
		if body, ok := TomlTableBody(in, "features.multi_agent_v2"); !ok || !strings.Contains(body, "max_concurrent_threads_per_session = 8") {
			t.Errorf("TomlTableBody = %q, %v", body, ok)
		}
		if got := FindTableHeader(text.SplitLines("[featuresXmulti_agent_v2]\n"), "features.multi_agent_v2"); got != -1 { // the dot is no wildcard
			t.Errorf("FindTableHeader matched a wildcard: %d", got)
		}
	})
	t.Run("readTableKey reports the live value, or null when absent", func(t *testing.T) {
		for _, c := range []struct {
			in   string
			want string
			ok   bool
		}{{tomlLF, "", false}, {tomlLF + "dedicated_tools = false\n", "false", true}, {"[features]\n", "", false}} {
			if got, ok := ReadTableKey(c.in, table, key); got != c.want || ok != c.ok {
				t.Errorf("ReadTableKey(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
			}
		}
	})
	t.Run("a missing table makes restore a no-op instead of an error", func(t *testing.T) {
		in := "[features]\nmulti_agent = true\n"
		tomlCheck(t, RestoreTableKey(in, table, key, nil), tomlWant{in, nil, false, TomlNoop})
	})
}

// TestBranches activates every branch of SetTableKey and RestoreTableKey by a named input.
func TestBranches(t *testing.T) {
	set := func(c string, v bool) func() TomlEditResult {
		return func() TomlEditResult { return SetTableKey(c, "t", "k", v) }
	}
	restore := func(c string, prior *string) func() TomlEditResult {
		return func() TomlEditResult { return RestoreTableKey(c, "t", "k", prior) }
	}
	for _, c := range []struct {
		name string
		run  func() TomlEditResult
		want tomlWant
	}{
		{"created-table: empty content", set("", true), tomlWant{"[t]\nk = true\n", nil, true, TomlCreatedTable}},
		{"created-table: trailing blank lines are popped (LF)", set("x = 1\n\n\n\n", true), tomlWant{"x = 1\n\n[t]\nk = true\n", nil, true, TomlCreatedTable}},
		{"created-table: trailing blank lines are popped (CRLF)", set("x = 1\r\n\r\n\r\n", true), tomlWant{"x = 1\r\n\r\n[t]\r\nk = true\r\n", nil, true, TomlCreatedTable}},
		{"inserted-into-table: after a trailing comment", set("[t]\nx = 1\n\n\n# c\n\n[u]\n", false), tomlWant{"[t]\nx = 1\n\n\n# c\nk = false\n\n[u]\n", nil, true, TomlInsertedIntoTable}},
		{"inserted-into-table: a body of only blank lines", set("[t]\n\n\n[u]\n", false), tomlWant{"[t]\nk = false\n\n\n[u]\n", nil, true, TomlInsertedIntoTable}},
		{"inserted-into-table: header without a final newline", set("[t]", true), tomlWant{"[t]\nk = true", nil, true, TomlInsertedIntoTable}},
		{"updated: the value differs and the comment tail is kept", set("[t]\nk = 1 # c\n", true), tomlWant{"[t]\nk = true # c\n", tomlRef("1"), true, TomlUpdated}},
		{"updated: a found empty value (k = #c) is not absent", set("[t]\nk = #c\n", true), tomlWant{"[t]\nk = true#c\n", tomlRef(""), true, TomlUpdated}},
		{"updated: only the first of two keys", set("[t]\nk = false\nk = true\n", true), tomlWant{"[t]\nk = true\nk = true\n", tomlRef("false"), true, TomlUpdated}},
		{"noop: the same value, trailing spaces kept byte for byte", set("[t]\nk = true \n", true), tomlWant{"[t]\nk = true \n", tomlRef("true"), false, TomlNoop}},
		{"unsupported-value: the first of two keys decides", set("[t]\nk = [1]\nk = false\n", true), tomlWant{"[t]\nk = [1]\nk = false\n", nil, false, TomlUnsupportedValue}},
		{"restore updated: a different non-null prior", restore("[t]\nk = true\n", tomlRef("false")), tomlWant{"[t]\nk = false\n", tomlRef("true"), true, TomlUpdated}},
		{"restore updated: the empty prior writes k = ", restore("[t]\nk = true\n", tomlRef("")), tomlWant{"[t]\nk = \n", tomlRef("true"), true, TomlUpdated}},
		{"restore noop: an equal prior", restore("[t]\nk = true\n", tomlRef("true")), tomlWant{"[t]\nk = true\n", tomlRef("true"), false, TomlNoop}},
		{"restore noop: a missing table", restore("[u]\nk = 1\n", nil), tomlWant{"[u]\nk = 1\n", nil, false, TomlNoop}},
		{"restore noop: a missing key", restore("[t]\nx = 1\n", tomlRef("false")), tomlWant{"[t]\nx = 1\n", nil, false, TomlNoop}},
		{"restore removed: the header stays", restore("[t]\nk = true\n", nil), tomlWant{"[t]\n", tomlRef("true"), true, TomlRemoved}},
		{"restore unsupported-value", restore("[t]\nk = '''x'''\n", nil), tomlWant{"[t]\nk = '''x'''\n", nil, false, TomlUnsupportedValue}},
	} {
		t.Run(c.name, func(t *testing.T) { tomlCheck(t, c.run(), c.want) })
	}
}

// TestLineEndings pins the bytes an edit writes: the file's dominant ending everywhere, whether the text ended with a newline, and the
// oracle's quirks (a tie gives LF, a lone CR is not part of a line ending).
func TestLineEndings(t *testing.T) {
	set := func(c string) TomlEditResult { return SetTableKey(c, "t", "k", true) }
	for _, c := range []struct{ name, in, want string }{
		{"CRLF update", "[t]\r\nk = false\r\n", "[t]\r\nk = true\r\n"},
		{"CRLF insert", "[t]\r\nx = 1\r\n", "[t]\r\nx = 1\r\nk = true\r\n"},
		{"CRLF create", "x = 1\r\n", "x = 1\r\n\r\n[t]\r\nk = true\r\n"},
		{"a CRLF majority rewrites the whole file as CRLF", "[t]\r\nx = 1\r\ny = 2\nz = 3\r\n", "[t]\r\nx = 1\r\ny = 2\r\nz = 3\r\nk = true\r\n"},
		{"an LF majority rewrites the whole file as LF", "[t]\r\nx = 1\ny = 2\n", "[t]\nx = 1\ny = 2\nk = true\n"},
		{"a tie gives LF", "[t]\r\nx = 1\n", "[t]\nx = 1\nk = true\n"},
		{"update without a final newline keeps none", "[t]\nk = false", "[t]\nk = true"},
		{"insert without a final newline keeps none", "[t]\nx = 1", "[t]\nx = 1\nk = true"},
		{"created-table always ends with a newline", "x = 1", "x = 1\n\n[t]\nk = true\n"},
		{"a BOM in front of a non-blank line survives", "\ufeff[t]\nk = false\n", "\ufeff[t]\nk = true\n"},
		{"a text that is only a BOM counts as blank", "\ufeff", "[t]\nk = true\n"},
		{"a lone CR ends no line: the key reads as absent and is duplicated", "[t]\nk = false\r", "[t]\nk = false\nk = true"},
		{"a lone CR in front of a CRLF is dropped", "[t]\r\nx = 1\r\r\ny = 2\r\n", "[t]\r\nx = 1\r\ny = 2\r\nk = true\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := set(c.in).Content; got != c.want {
				t.Errorf("SetTableKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	for name, in := range map[string]string{"noop keeps mixed endings": "[t]\r\nk = true\ny = 2\n", "unsupported keeps CRLF": "[t]\r\nk = [1]\r\n"} {
		if r := set(in); r.Content != in || r.Changed {
			t.Errorf("%s: SetTableKey(%q) = %q, changed %v; want the input back", name, in, r.Content, r.Changed)
		}
	}
	if r := RestoreTableKey("[t]\r\nk = true\r\n", "t", "k", nil); r.Content != "[t]\r\n" {
		t.Errorf("CRLF restore removed = %q", r.Content)
	}
	if r := RestoreTableKey("[t]\r\nk = true\r\n", "t", "k", tomlRef("false")); r.Content != "[t]\r\nk = false\r\n" {
		t.Errorf("CRLF restore updated = %q", r.Content)
	}
}

// TestMultilineStringContentIsNeverEdited is the one place the port differs from the oracle on purpose: a line that is the inside of a
// multi-line string is never a table header, a table start or a key. The oracle rewrote such a line as the key and deleted it on restore,
// which loses text of another setting. The oracle replay below proves nothing else differs.
func TestMultilineStringContentIsNeverEdited(t *testing.T) {
	const q3, a3 = "\"\"\"", "'''"
	set := func(c string, v bool) TomlEditResult { return SetTableKey(c, "memories", "dedicated_tools", v) }
	t.Run("a key-looking line inside a string is not the key", func(t *testing.T) {
		in := "[memories]\nnote = " + q3 + "\ndedicated_tools = false\n" + q3 + "\n"
		tomlCheck(t, set(in, true), tomlWant{in + "dedicated_tools = true\n", nil, true, TomlInsertedIntoTable})
		tomlCheck(t, RestoreTableKey(in, "memories", "dedicated_tools", nil), tomlWant{in, nil, false, TomlNoop})
	})
	t.Run("the real key beside a string is the one edited", func(t *testing.T) {
		in := "[memories]\nnote = " + a3 + "\ndedicated_tools = true\n" + a3 + "\ndedicated_tools = false\n"
		tomlCheck(t, set(in, true), tomlWant{"[memories]\nnote = " + a3 + "\ndedicated_tools = true\n" + a3 + "\ndedicated_tools = true\n", tomlRef("false"), true, TomlUpdated})
		tomlCheck(t, RestoreTableKey(in, "memories", "dedicated_tools", nil), tomlWant{"[memories]\nnote = " + a3 + "\ndedicated_tools = true\n" + a3 + "\n", tomlRef("false"), true, TomlRemoved})
	})
	t.Run("a header-looking line inside a string is not the table", func(t *testing.T) {
		in := "note = " + q3 + "\n[memories]\ndedicated_tools = false\n" + q3 + "\n"
		tomlCheck(t, set(in, true), tomlWant{in + "\n[memories]\ndedicated_tools = true\n", nil, true, TomlCreatedTable})
		if v, ok := ReadTableKey(in, "memories", "dedicated_tools"); ok {
			t.Errorf("ReadTableKey = %q, want absent", v)
		}
		if b, ok := TomlTableBody(in, "memories"); ok {
			t.Errorf("TomlTableBody = %q, want absent", b)
		}
	})
	t.Run("a table-start line inside a string does not end the body", func(t *testing.T) {
		in := "[memories]\nnote = " + q3 + "\n[features]\n" + q3 + "\ndedicated_tools = false\n"
		tomlCheck(t, set(in, true), tomlWant{"[memories]\nnote = " + q3 + "\n[features]\n" + q3 + "\ndedicated_tools = true\n", tomlRef("false"), true, TomlUpdated})
		want := "note = " + q3 + "\n[features]\n" + q3 + "\ndedicated_tools = false\n"
		if b, ok := TomlTableBody(in, "memories"); !ok || b != want {
			t.Errorf("TomlTableBody = %q, %v; want %q", b, ok, want)
		}
	})
	// Where a string opens and closes, valid TOML 1.0 (the reading of each is in the comment).
	for _, c := range []struct{ name, in, want string }{
		{"a one-line string holding three single quotes opens nothing", "[t]\na = '" + q3 + "'\nk = false\n", "false"},
		{"a one-line string holding three apostrophes opens nothing", "[t]\na = \"" + a3 + "\"\nk = false\n", "false"},
		{"a comment holding three quotes opens nothing", "[t]\n# " + q3 + "\nk = false\n", "false"},
		{"an escaped quote does not close: k = 1 is inside, k = false follows the closer", "[t]\na = " + q3 + "x\\" + q3 + "\nk = 1\n" + q3 + "\nk = false\n", "false"},
		{"four quotes close a string", "[t]\na = " + q3 + "x" + q3 + "\"\nk = 1\n", "1"},
		{"six quotes are an empty string", "[t]\na = " + q3 + q3 + "\nk = 1\n", "1"},
		{"five quotes close a string", "[t]\na = " + q3 + "x" + q3 + "\"\"\nk = 1\n", "1"},
		{"a closer of four quotes, then a second string opens on the same line", "[t]\na = [" + q3 + "x" + q3 + "\", " + q3 + "\nk = false\n" + q3 + "]\nk = true\n[u]\nx = 1\n", "true"},
		{"the same with apostrophes", "[t]\na = [" + a3 + "x" + a3 + "', " + a3 + "\nk = false\n" + a3 + "]\nk = true\n[u]\nx = 1\n", "true"},
		{"a longer literal run closes as well", "[t]\na = " + a3 + a3 + "\nk = 1\n", "1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := ReadTableKey(c.in, "t", "k"); !ok || got != c.want {
				t.Errorf("ReadTableKey(%q) = %q, %v; want %q", c.in, got, ok, c.want)
			}
		})
	}
	t.Run("after a string closes on the line of a second opener, the following header is real", func(t *testing.T) {
		in := "[t]\na = [" + q3 + "x" + q3 + "\", " + q3 + "\nk = false\n" + q3 + "]\nk = true\n[u]\nx = 1\n"
		tomlCheck(t, SetTableKey(in, "t", "k", true), tomlWant{in, tomlRef("true"), false, TomlNoop})
		if got := FindTableHeader(text.SplitLines(in), "u"); got != 5 {
			t.Errorf("FindTableHeader(u) = %d, want 5", got)
		}
	})
}

// A table or key that is not valid UTF-8 has no counterpart in the oracle, whose strings cannot hold such bytes; the port reads it as
// matching nothing (the pattern does not compile), so the edit appends or inserts the raw bytes and never panics.
func TestInvalidUTF8Names(t *testing.T) {
	tomlCheck(t, SetTableKey("[t]\nx = 1\n", "\xff", "k", true), tomlWant{"[t]\nx = 1\n\n[\xff]\nk = true\n", nil, true, TomlCreatedTable})
	tomlCheck(t, SetTableKey("[t]\nx = 1\n", "t", "\xff", true), tomlWant{"[t]\nx = 1\n\xff = true\n", nil, true, TomlInsertedIntoTable})
	if _, ok := ReadTableKey("[t]\nk = 1\n", "\xff", "k"); ok {
		t.Error("ReadTableKey read a key of a table that cannot match")
	}
}

type tomlOracleRow struct {
	Fn  string
	In  []json.RawMessage
	Out json.RawMessage
}

func tomlOracleRows(t *testing.T) []tomlOracleRow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-toml-edit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []tomlOracleRow
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func tomlArg[T any](t *testing.T, row tomlOracleRow, i int) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(row.In[i], &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// tomlCanon reads v through JSON, so that a Go nil (null) differs from the oracle's empty string and an empty list from null.
func tomlCanon(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func tomlEditJSON(r TomlEditResult) map[string]any {
	return map[string]any{"content": r.Content, "priorValue": r.PriorValue, "changed": r.Changed, "action": string(r.Action)}
}

func tomlOptional(s string, ok bool) any {
	if !ok {
		return nil
	}
	return s
}

// tomlRun applies the Go function the row's oracle function is the port of, to the row's arguments, in the oracle's JSON shape.
func tomlRun(t *testing.T, row tomlOracleRow, fn string) any {
	t.Helper()
	switch fn {
	case "setTableKey":
		return tomlEditJSON(SetTableKey(tomlArg[string](t, row, 0), tomlArg[string](t, row, 1), tomlArg[string](t, row, 2), tomlArg[bool](t, row, 3)))
	case "restoreTableKey":
		return tomlEditJSON(RestoreTableKey(tomlArg[string](t, row, 0), tomlArg[string](t, row, 1), tomlArg[string](t, row, 2), tomlArg[*string](t, row, 3)))
	case "readTableKey":
		return tomlOptional(ReadTableKey(tomlArg[string](t, row, 0), tomlArg[string](t, row, 1), tomlArg[string](t, row, 2)))
	case "tomlTableBody":
		return tomlOptional(TomlTableBody(tomlArg[string](t, row, 0), tomlArg[string](t, row, 1)))
	case "findTableHeader":
		return FindTableHeader(tomlArg[[]string](t, row, 0), tomlArg[string](t, row, 1))
	case "findKeyLine":
		found, state := FindKeyLine(tomlArg[[]string](t, row, 0), tomlArg[int](t, row, 1), tomlArg[string](t, row, 2))
		switch state {
		case TomlKeyUnsupported:
			return "unsupported"
		case TomlKeyFound:
			return map[string]any{"index": found.Index, "indent": found.Indent, "value": found.Value, "comment": found.Comment}
		}
		return nil
	}
	t.Fatalf("unknown oracle function %q", fn)
	return nil
}

// TestOracleTomlEdit replays every recorded row. The "neutral/" rows are the answers for a text whose in-string lines the oracle cannot
// mistake for a header or key (see the recorder); Go must equal each of them, and a plain row may differ from the oracle only where a
// neutral row for the same function and the same whole input exists and Go equals that row instead.
func TestOracleTomlEdit(t *testing.T) {
	rows := tomlOracleRows(t)
	neutral := map[string]any{}
	key := func(row tomlOracleRow, fn string) string {
		parts := []string{fn}
		for _, in := range row.In {
			parts = append(parts, string(in))
		}
		return strings.Join(parts, "\x00")
	}
	seen := map[string]int{}
	for _, row := range rows {
		fn, isNeutral := strings.CutPrefix(row.Fn, "neutral/")
		if !isNeutral {
			continue
		}
		seen[row.Fn]++
		var want any
		if err := json.Unmarshal(row.Out, &want); err != nil {
			t.Fatal(err)
		}
		if got := tomlCanon(t, tomlRun(t, row, fn)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s %s: got %v, want %v", row.Fn, row.In, got, want)
		}
		neutral[key(row, fn)] = want
	}
	exempt := 0
	for _, row := range rows {
		if strings.HasPrefix(row.Fn, "neutral/") || !slices.Contains([]string{"setTableKey", "restoreTableKey", "readTableKey", "tomlTableBody", "findTableHeader", "findKeyLine"}, row.Fn) {
			continue
		}
		seen[row.Fn]++
		var want any
		if err := json.Unmarshal(row.Out, &want); err != nil {
			t.Fatal(err)
		}
		got := tomlCanon(t, tomlRun(t, row, row.Fn))
		if reflect.DeepEqual(got, want) {
			continue
		}
		if twin, ok := neutral[key(row, row.Fn)]; ok && reflect.DeepEqual(got, twin) {
			exempt++
			continue
		}
		t.Errorf("%s %s: got %v, oracle %v", row.Fn, row.In, got, want)
	}
	if exempt == 0 {
		t.Error("no row differs from the oracle: the neutral rows are not exercised")
	}
	t.Logf("%d rows, %d differ from the oracle on a multi-line string and equal their neutral row: %v", len(rows), exempt, seen)
}
