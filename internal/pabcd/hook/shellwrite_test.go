package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"unicode/utf16"
)

// The B cases of shell-write-destinations.test.ts:13-38,57-60. The arrow
// exception is intentionally changed: it redirects in a real POSIX shell.
func TestShellWriteDestinationsB(t *testing.T) {
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"echo hi > /m/n.md", []string{"/m/n.md"}},
		{"echo hi >> /m/n.md", []string{"/m/n.md"}},
		{"echo hi>/m/n.md", []string{"/m/n.md"}},
		{"echo hi>>/m/n.md", []string{"/m/n.md"}},
		{"echo hi >| /m/n.md", []string{"/m/n.md"}},
		{"echo hi 1> /m/n.md", []string{"/m/n.md"}},
		{"cmd &> /m/n.md", []string{"/m/n.md"}},
		{"rg foo /m/M.md 2>/dev/null", []string{}},
		{"cmd 2>&1", []string{}},
		{"x -> y", []string{"y"}},
		{"grep -- '->' /w/f", []string{}},
		{"echo 'a>b'", []string{}},
		{"echo 'a > b'", []string{}},
		{"rg '<prose>' /w/f", []string{}},
		{"cat > /w/x.md <<'EOF'\n/memories\nEOF", []string{"/w/x.md"}},
		{"cat <<EOF > /w/x.md\n/memories/n.md\nEOF", []string{"/w/x.md"}},
		{"cat <<< \"/memories/n.md\"", []string{}},
		{"mkdir -p /w/notes && cat > /w/notes/00.md <<'EOF'\n/memories\nEOF", []string{"/w/notes/00.md"}},
		{"cat /w/a && echo x > /m/n.md; ls", []string{"/m/n.md"}},
		{"cat /w/a || echo x>/m/n.md", []string{"/m/n.md"}},
	} {
		t.Run(c.command, func(t *testing.T) {
			got := ShellWriteDestinations(c.command)
			if got == nil || !slices.Equal(got, c.want) {
				t.Fatalf("got %q, want non-nil %q", got, c.want)
			}
		})
	}
}

func TestShellWriteRecordedOracle(t *testing.T) {
	var golden struct {
		Entry []struct {
			Input                  string
			Output, Expected       []string
			Classification, Reason string
		}
		Units []struct {
			Fn     string
			Input  []uint16
			At     int
			Output json.RawMessage
		}
	}
	raw, err := os.ReadFile("testdata/shellwrite/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Entry) != 70 || len(golden.Units) != 348 {
		t.Fatal("incomplete recording")
	}
	for i, c := range golden.Entry {
		t.Run(fmt.Sprintf("entry/%d", i), func(t *testing.T) {
			if c.Classification != "identical" && (c.Classification != "intentionally-changed" || c.Reason == "") {
				t.Fatal("unclassified entry")
			}
			got := ShellWriteDestinations(c.Input)
			if !slices.Equal(got, c.Expected) {
				t.Fatalf("%q: got %q want %q", c.Input, got, c.Expected)
			}
			if len(got) < len(c.Output) || !slices.Equal(got[:len(c.Output)], c.Output) {
				t.Fatal("lost oracle reports")
			}
		})
	}
	for i, c := range golden.Units {
		t.Run(fmt.Sprintf("%s/%d", c.Fn, i), func(t *testing.T) {
			var got any
			switch c.Fn {
			case "stripHeredocBodies":
				got = stripHeredocBodies(c.Input)
			case "heredocDelimiter":
				got = heredocDelimiter(c.Input, c.At)
			case "splitShellSegments":
				got = splitShellSegments(c.Input)
			case "skipQuoted":
				got = skipQuoted(c.Input, c.At)
			case "skipHeredoc":
				got = skipHeredoc(c.Input, c.At)
			case "readToken":
				r := readToken(c.Input, c.At)
				got = struct {
					Token []uint16 `json:"token"`
					Next  int      `json:"next"`
				}{r.token, r.next}
			case "redirectDestinations":
				got = redirectDestinations(c.Input)
			case "tokenize":
				got = tokenizeUnits(c.Input)
			default:
				t.Fatalf("unknown helper %s", c.Fn)
			}
			// Decode numbers as JSON values: []uint16 raw units never pass through
			// encoding/json's replacement of lone-surrogate string escapes.
			bytes, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			if err := json.Unmarshal(bytes, &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Output, &expected); err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(actual)
			e, _ := json.Marshal(expected)
			if string(a) != string(e) {
				t.Fatalf("input %v: got %s want %s", c.Input, a, e)
			}
		})
	}
}

func TestShellWriteLiteralSecurity(t *testing.T) {
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"echo x > a>b", []string{"a>b", "a", "b"}},
		{"echo x >\"a\"x", []string{"a", "ax"}},
		{"echo x > a\\ b", []string{"a\\", "a b"}},
		{"echo x 2> err", []string{"err"}},
		{"echo x 0<> both >&0", []string{"both"}},
		{"echo x -> arrow", []string{"arrow"}},
		{"echo x >& both", []string{"both"}},
		{"cat <<-EOF > doc\n\tbody\n\tEOF\necho x > next", []string{"doc", "next"}},
		{"cat <<EOF-X > doc\nbody\nEOF-X\necho x > next", []string{"doc", "next"}},
		{"cat <<A <<B > out\nfirst\nA\nsecond > leaked\nB\necho x > next", []string{"out", "leaked", "next"}},
		{"cat <<A <<B > out\nfirst > hidden\nA\nsecond > leaked\nB\necho x > next", []string{"out", "leaked", "next"}},
		{"echo hi > a; echo hi > a", []string{"a", "a"}},
		{"cat <<\\EOF > doc\nbody > ignored\nEOF\necho x > next", []string{"doc", "ignored", "next"}},
		{"echo hi > 'a'\"b\"c", []string{"a", "abc"}},
		{"echo hi > a\\;b", []string{"a\\", "a;b"}},
		{"echo hi > \"a\\\"b\"", []string{"a\\\"b", "a\"b"}},
		{"cat <<< '> ignored' > out", []string{"out"}},
		{"echo x 2>&1; echo x >&-", []string{}},
		{"echo x > \"unterminated", []string{"unterminate"}},
	} {
		t.Run(c.command, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); !slices.Equal(got, c.want) {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
	if got := tokenize("tee 'a b' c"); !slices.Equal(got, []string{"tee", "a b", "c"}) {
		t.Fatalf("verb tokens %q", got)
	}
	if got := ShellWriteDestinations("tee out"); len(got) != 0 {
		t.Fatalf("deferred verbs implemented: %q", got)
	}
	// Malformed quotes retain the oracle's one-unit truncation; UTF-8 output
	// replaces the remaining high surrogate as Node Buffer.from does.
	u := readToken(utf16.Encode([]rune("\"🧪")), 0)
	if !slices.Equal(u.token, []uint16{0xd83e}) {
		t.Fatalf("raw surrogate %v", u.token)
	}
	if got := ShellWriteDestinations("echo > \"🧪"); !slices.Equal(got, []string{"\ufffd"}) {
		t.Fatalf("boundary %q", got)
	}
}

func TestShellWriteLiteralReviewRegressions(t *testing.T) {
	for _, command := range []string{
		"printf x \\ #word 2>target",
		": 2> \\\n target",
		": >\\\n| target",
		"cat <<EOF$X\n'\nEOF$X\n: 2>target",
		"cat <<''\n'\n\n: 2>target",
		"cat <\\\n<EOF\n'\nEOF\n: 2>target",
		"cat <<EOF\n'\nEO\\\nF\n: 2>target",
	} {
		t.Run(command, func(t *testing.T) {
			if got := ShellWriteDestinations(command); !slices.Contains(got, "target") {
				t.Fatalf("literal target missing: %q", got)
			}
		})
	}
	if got := ShellWriteDestinations(": >a\rb"); !slices.Equal(got, []string{"a", "a\rb"}) {
		t.Fatalf("carriage return filename: %q", got)
	}
}
