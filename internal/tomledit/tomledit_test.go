package tomledit

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

var dedicated = []string{"memories", "dedicated_tools"}

func TestGetForms(t *testing.T) {
	for _, c := range []struct {
		name, in string
		state    State
		raw      string
	}{
		{"bare", "[memories]\ndedicated_tools = false\n", Found, "false"},
		{"quoted header", "[\"memories\"]\ndedicated_tools = true # c\n", Found, "true"},
		{"spaced header", "[ memories ]\ndedicated_tools = true\n", Found, "true"},
		{"literal quoted key", "[memories]\n'dedicated_tools' = false\n", Found, "false"},
		{"basic quoted key with escape", "[memories]\n\"dedicated\\u005ftools\" = false\n", Found, "false"},
		{"root dotted key", "memories.dedicated_tools = false\n[x]\ny = 1\n", Found, "false"},
		{"dotted spaced", "memories . dedicated_tools = false\n", Found, "false"},
		{"dotted inside a parent table", "[a]\nb = 1\n[memories]\nother = 1\n", Absent, ""},
		{"string value", "[memories]\ndedicated_tools = \"a # b\" # c\n", Found, "\"a # b\""},
		{"absent", "[memories]\ngenerate_memories = true\n", Absent, ""},
		{"header inside a string", "[model]\nnote = \"\"\"\n[memories]\ndedicated_tools = false\n\"\"\"\n", Absent, ""},
		{"key-like line inside an array", "[memories]\nextra = [\n  [1, 2],\n  \"dedicated_tools = true\",\n]\n", Absent, ""},
		{"inline table", "memories = { dedicated_tools = true }\n", Unsupported, ""},
		{"array value", "[memories]\ndedicated_tools = [true]\n", Unsupported, ""},
		{"multi-line string value", "[memories]\ndedicated_tools = \"\"\"x\"\"\"\n", Unsupported, ""},
		{"table not a value", "[memories.dedicated_tools]\nx = 1\n", Unsupported, ""},
		{"value not a table", "memories = 1\n", Unsupported, ""},
		{"array of tables", "[[memories]]\nx = 1\n", Unsupported, ""},
		{"invalid duplicate table", "[\"memories\"]\nx = 1\n[memories]\ny = 1\n", Invalid, ""},
		{"invalid unterminated string", "[memories]\ndedicated_tools = \"oops\n", Invalid, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Get(c.in, dedicated)
			if got.State != c.state || got.State == Found && got.Raw != c.raw {
				t.Fatalf("Get = %+v, want %v %q", got, c.state, c.raw)
			}
			if c.state == Unsupported && got.Reason == "" || c.state == Invalid && got.Reason == "" {
				t.Fatalf("no reason: %+v", got)
			}
		})
	}
}

func TestSetMinimalEdits(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		prior          *string
	}{
		{"insert after the last line of the table", "[memories]\ngenerate_memories = true\n\n[x]\ny = 1\n", "[memories]\ngenerate_memories = true\ndedicated_tools = true\n\n[x]\ny = 1\n", nil},
		{"insert under a quoted header", "[\"memories\"]\ngenerate_memories = true\n", "[\"memories\"]\ngenerate_memories = true\ndedicated_tools = true\n", nil},
		{"insert under a spaced header with a comment", "[ memories ] # mine\n", "[ memories ] # mine\ndedicated_tools = true\n", nil},
		{"insert after a multi-line array, not inside it", "[memories]\nextra = [\n  [1, 2],\n  [3],\n]\n[z]\n", "[memories]\nextra = [\n  [1, 2],\n  [3],\n]\ndedicated_tools = true\n[z]\n", nil},
		{"insert keeps the indent", "[memories]\n  a = 1\n", "[memories]\n  a = 1\n  dedicated_tools = true\n", nil},
		{"insert next to root dotted keys", "memories.other = 1\n[x]\n", "memories.other = 1\nmemories.dedicated_tools = true\n[x]\n", nil},
		{"insert into a file without a final newline", "[memories]\na = 1", "[memories]\na = 1\ndedicated_tools = true", nil},
		{"insert uses the line's own ending", "[memories]\r\na = 1\r\n[x]\nb = 2\nc = 3\n", "[memories]\r\na = 1\r\ndedicated_tools = true\r\n[x]\nb = 2\nc = 3\n", nil},
		{"append a table", "# c\n[x]\ny = 1\n", "# c\n[x]\ny = 1\n\n[memories]\ndedicated_tools = true\n", nil},
		{"append a table to an empty file", "", "[memories]\ndedicated_tools = true\n", nil},
		{"append a table after a header-like string", "[model]\nnote = \"\"\"\n[memories]\n\"\"\"\n", "[model]\nnote = \"\"\"\n[memories]\n\"\"\"\n\n[memories]\ndedicated_tools = true\n", nil},
		{"append a table after a sub-table", "[memories.sub]\nx = 1\n", "[memories.sub]\nx = 1\n\n[memories]\ndedicated_tools = true\n", nil},
		{"append a table in a CRLF file", "[x]\r\ny = 1\r\n", "[x]\r\ny = 1\r\n\r\n[memories]\r\ndedicated_tools = true\r\n", nil},
		{"update a quoted key in place", "[memories]\n\"dedicated_tools\" = false # keep\r\nz = 1\n", "[memories]\n\"dedicated_tools\" = true # keep\r\nz = 1\n", ptr("false")},
		{"update a root dotted key in place", "memories.dedicated_tools = false\n", "memories.dedicated_tools = true\n", ptr("false")},
		{"update a string value", "[memories]\ndedicated_tools = \"x # y\"#c\n", "[memories]\ndedicated_tools = true#c\n", ptr("\"x # y\"")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Set(c.in, dedicated, "true")
			if err != nil || got.Content != c.want || !got.Changed || !samePtr(got.Prior, c.prior) {
				t.Fatalf("Set = %q, %v, %+v; want %q", got.Content, err, got, c.want)
			}
		})
	}
	same := "[memories]\ndedicated_tools = true \n"
	if got, err := Set(same, dedicated, "true"); err != nil || got.Changed || got.Content != same || *got.Prior != "true" {
		t.Fatalf("noop = %+v, %v", got, err)
	}
}

func TestSetRefusals(t *testing.T) {
	for _, in := range []string{
		"memories = { dedicated_tools = false }\n",
		"[memories]\ndedicated_tools = [false]\n",
		"[[memories]]\nx = 1\n",
		"memories = \"x\"\n",
		"[memories]\ndedicated_tools = \"oops\n",
		"[memories]\nx = 1\n[memories]\ny = 2\n",
	} {
		if got, err := Set(in, dedicated, "true"); err == nil {
			t.Fatalf("Set(%q) = %+v, want a refusal", in, got)
		} else if _, ok := IsRefusal(err); !ok {
			t.Fatalf("Set(%q) error %v is not a refusal", in, err)
		}
	}
	if _, err := Set("", dedicated, "not a value"); err == nil {
		t.Fatal("an invalid raw value was accepted")
	}
}

func TestRestore(t *testing.T) {
	in := "# c\n[memories]\ngenerate = 1\ndedicated_tools = true # mine\r\n[x]\n"
	got, err := Restore(in, dedicated, nil)
	if err != nil || got.Content != "# c\n[memories]\ngenerate = 1\n[x]\n" || !got.Changed || *got.Prior != "true" {
		t.Fatalf("remove = %q, %v", got.Content, err)
	}
	got, err = Restore(in, dedicated, ptr("false"))
	if err != nil || got.Content != "# c\n[memories]\ngenerate = 1\ndedicated_tools = false # mine\r\n[x]\n" {
		t.Fatalf("restore = %q, %v", got.Content, err)
	}
	got, err = Restore(in, dedicated, ptr("true"))
	if err != nil || got.Changed || got.Content != in {
		t.Fatalf("equal restore = %+v, %v", got, err)
	}
	got, err = Restore("[memories]\n", dedicated, ptr("false"))
	if err != nil || got.Changed || got.Content != "[memories]\n" || got.Prior != nil {
		t.Fatalf("absent restore = %+v, %v", got, err)
	}
	got, err = Restore("memories.dedicated_tools = true\nmemories.other = 1\n", dedicated, nil)
	if err != nil || got.Content != "memories.other = 1\n" {
		t.Fatalf("dotted remove = %q, %v", got.Content, err)
	}
	if _, err := Restore("[memories]\ndedicated_tools = [true]\n", dedicated, nil); err == nil {
		t.Fatal("restore of an array was accepted")
	}
	if _, err := Restore("[memories]\ndedicated_tools = true\n", dedicated, ptr("{")); err == nil {
		t.Fatal("restore of an invalid recorded value was accepted")
	}
}

func TestCheckRefusesAnEditThatChangesMore(t *testing.T) {
	pre := "[memories]\na = 1\n"
	if err := check(pre, "[memories]\na = 2\ndedicated_tools = true\n", dedicated, true, true); err == nil {
		t.Fatal("a candidate that changed another key passed")
	}
	if err := check(pre, "[memories]\na = 1\ndedicated_tools = false\n", dedicated, true, true); err == nil {
		t.Fatal("a candidate that holds another value passed")
	}
	if err := check(pre, "[memories]\na = 1\n[memories]\n", dedicated, true, true); err == nil {
		t.Fatal("an invalid candidate passed")
	}
	if err := check(pre, "[memories]\na = 1\ndedicated_tools = true\n", dedicated, true, true); err != nil {
		t.Fatal(err)
	}
}

func TestEqualAndSameValue(t *testing.T) {
	if !Equal(math.NaN(), math.NaN()) || Equal(1.0, 2.0) || Equal(int64(1), 1.0) {
		t.Fatal("float comparison")
	}
	a := time.Date(2020, 1, 1, 0, 0, 0, 0, time.FixedZone("", 3600))
	if !Equal(a, a.In(time.FixedZone("", 3600))) || Equal(a, a.UTC()) {
		t.Fatal("time comparison")
	}
	if !Equal([]any{int64(1), map[string]any{"x": "y"}}, []any{int64(1), map[string]any{"x": "y"}}) || Equal([]any{int64(1)}, []any{}) {
		t.Fatal("array comparison")
	}
	if !SameValue("true", "true") || SameValue("true", "\"true\"") || SameValue("{", "{") || !SameValue("0x10", "16") {
		t.Fatal("SameValue")
	}
	doc := "x = nan\nt = 1979-05-27T07:32:00-08:00\n[memories]\n"
	if _, err := Set(doc, dedicated, "true"); err != nil {
		t.Fatalf("a document holding NaN and a time could not be edited: %v", err)
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{Absent: "absent", Found: "found", Unsupported: "unsupported", Invalid: "invalid", State(9): "State(9)"} {
		if s.String() != want {
			t.Fatalf("%d = %q", int(s), s.String())
		}
	}
	if (&Refusal{Unsupported, "why"}).Error() != "why" {
		t.Fatal("refusal text")
	}
}

func ptr(s string) *string { return &s }

func samePtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func TestStatementsLocateHeadersAndKeysOutsideStrings(t *testing.T) {
	in := "[a]\nx = \"\"\"\n[not.a.header]\n\"\"\"\n[ \"b\" . c ] # t\ny.z = 1\n[[d]]\n"
	got, err := Statements(in)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		table, array bool
		path         string
		text         string
	}
	var rows []row
	for _, st := range got {
		rows = append(rows, row{st.Table, st.ArrayTable, strings.Join(st.Path, "."), in[st.Start:st.End]})
	}
	want := []row{
		{true, false, "a", "[a]\n"},
		{false, false, "a.x", "x = \"\"\"\n[not.a.header]\n\"\"\"\n"},
		{true, false, "b.c", "[ \"b\" . c ] # t\n"},
		{false, false, "b.c.y.z", "y.z = 1\n"},
		{false, true, "d", "[[d]]\n"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("statements = %#v", rows)
	}
}
