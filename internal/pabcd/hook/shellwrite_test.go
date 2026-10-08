package hook

import (
	"slices"
	"testing"
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
			got := shellWriteDestsTest(c.command)
			if got == nil || !slices.Equal(got, c.want) {
				t.Fatalf("got %q, want non-nil %q", got, c.want)
			}
		})
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
		"cat <<EOF\n\\\\\nEOF\n: 2>target",
	} {
		t.Run(command, func(t *testing.T) {
			if got := shellWriteDestsTest(command); !slices.Contains(got, "target") {
				t.Fatalf("literal target missing: %q", got)
			}
		})
	}
	if got := shellWriteDestsTest(": >a\rb"); !slices.Equal(got, []string{"a", "a\rb"}) {
		t.Fatalf("carriage return filename: %q", got)
	}
}
