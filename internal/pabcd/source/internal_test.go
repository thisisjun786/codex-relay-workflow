package source

import (
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestParseStatusZ(t *testing.T) {
	u := func(s string) []uint16 { return utf16.Encode([]rune(s)) }
	rec := func(xy, path, orig string, hasOrig bool) statusRecord {
		r := statusRecord{xy: u(xy), path: u(path), hasOrig: hasOrig}
		if orig != "" {
			r.origPath = u(orig)
		}
		return r
	}
	for _, c := range []struct {
		name, in string
		want     []statusRecord
	}{
		{"rename and copy carry their source", "R  b.ts\x00a.ts\x00C  d.ts\x00c.ts\x00 M e.ts\x00", []statusRecord{rec("R ", "b.ts", "a.ts", true), rec("C ", "d.ts", "c.ts", true), rec(" M", "e.ts", "", false)}},
		{"a rename without its source field", "R  b.ts\x00", []statusRecord{rec("R ", "b.ts", "", true)}},
		// Oracle defect: the source is consumed only when the index column is R or C, so the source of a
		// worktree-side rename (git add -N) becomes an entry of its own, cut in UTF-16 units.
		{"a worktree rename source becomes an entry", " R moved.ts\x00tracked.ts\x00 R moved.ts\x00\u00e9abc.ts\x00", []statusRecord{rec(" R", "moved.ts", "", false), rec("tr", "cked.ts", "", false), rec(" R", "moved.ts", "", false), rec("\u00e9a", "c.ts", "", false)}},
		{"cutting an astral character leaves a lone surrogate", " R moved.ts\x00ab\U0001F600.ts\x00", []statusRecord{rec(" R", "moved.ts", "", false), {xy: u("ab"), path: []uint16{0xDE00, '.', 't', 's'}}}},
		{"an unterminated tail is dropped and short fields are skipped", "?? a.ts\x00\x00?? \x00ab\x00?? b.ts", []statusRecord{rec("??", "a.ts", "", false)}},
		{"invalid UTF-8 is replaced", "?? a\xffb.ts\x00", []statusRecord{rec("??", "a\uFFFDb.ts", "", false)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := parseStatusZ([]byte(c.in)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// The limit is Node's maxBuffer: stdout and stderr together, more than the limit is refused, and the child is killed
// at once even when a grandchild still holds the pipes.
func TestRunOutputLimit(t *testing.T) {
	hermetic(t)
	for limit, ok := range map[int]bool{8: true, 7: false, 5: false} {
		out, err := run(t.TempDir(), limit, "sh", "-c", "printf aaaa; printf bbbb >&2")
		if (err == nil) != ok || (ok && string(out) != "aaaa") {
			t.Errorf("limit %d: %q, %v", limit, out, err)
		}
	}
	started := time.Now()
	if _, err := run(t.TempDir(), 4, "sh", "-c", "sleep 5 & printf 12345; wait"); err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("a child over the limit must be killed at once: %v after %v", err, time.Since(started))
	}
}

func TestCaptureUnavailableAndLimits(t *testing.T) {
	base := hermetic(t)
	root := newRepo(t, base)
	// rev-parse (41 bytes) overflows but the clean tree's empty status does not: resolved, without a commit.
	if id := captureWithLimit(root, Options{}, 40); id.Kind != KindResolved || id.CommitSha != "" || id.Dirty {
		t.Fatalf("rev-parse over the limit: %+v", id)
	}
	writeFile(t, root, strings.Repeat("long-name-", 8)+".ts", "x")
	if id := captureWithLimit(root, Options{}, 41); id.Kind != KindUnavailable || id.CommitSha != "" {
		t.Fatalf("status over the limit: %+v", id)
	}
	writeFile(t, base, "a-file", "x")
	for _, cwd := range []string{base + "/missing", base + "/a-file"} {
		if id := Capture(cwd, Options{}); id.Kind != KindUnavailable {
			t.Errorf("%s: %+v", cwd, id)
		}
	}
	t.Setenv("PATH", t.TempDir())
	if id := Capture(root, Options{}); id.Kind != KindUnavailable {
		t.Errorf("git absent: %+v", id)
	}
}

func TestCaptureOptions(t *testing.T) {
	root := newRepo(t, hermetic(t))
	clean := Capture(root, Options{})
	writeFile(t, root, ".crw/sessions/s.json", "{}")
	for _, c := range []struct {
		o    Options
		want ComparisonKind
	}{{Options{}, ComparisonDifferent}, {Options{ExcludeStateArtifacts: true}, ComparisonSame}} {
		if got := Compare(clean, Capture(root, c.o)).Kind; got != c.want {
			t.Errorf("%+v: %q, want %q", c.o, got, c.want)
		}
	}
	at := time.Date(2026, 10, 3, 5, 6, 7, 89_000_000, time.FixedZone("KST", 9*3600))
	if got := Capture(root, Options{Now: func() time.Time { return at }}).CapturedAt; got != "2026-10-02T20:06:07.089Z" {
		t.Errorf("capturedAt = %q", got)
	}
}

func TestCompareAndDescribe(t *testing.T) {
	root := func(s string) *string { return &s }
	id := func(sha string, dirty bool, tree string) Identity {
		return Identity{Kind: KindResolved, CommitSha: sha, Dirty: dirty, TreeHash: tree}
	}
	different := func(detail string) Comparison { return Comparison{Kind: ComparisonDifferent, Detail: detail} }
	for _, c := range []struct {
		a, b Identity
		want Comparison
	}{
		{Identity{Kind: KindUnavailable}, id("a", false, ""), Comparison{Kind: ComparisonUnavailable, Reason: "git could not resolve the source identity on at least one side"}},
		{Identity{Kind: KindResolved, SourceRoot: root("")}, Identity{Kind: KindResolved}, different("source root changed or binding is missing")},
		{id("abcdef0123", false, ""), id("1234567890", false, ""), different("commit abcdef0 -> 1234567")},
		{id("a", true, "x"), id("a", false, ""), different("working tree went clean")},
		{id("a", false, ""), id("a", true, "x"), different("working tree went dirty")},
		{id("a", true, "x"), id("a", true, "y"), different("uncommitted changes differ")},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("%+v vs %+v: %+v, want %+v", c.a, c.b, got, c.want)
		}
	}
	for want, in := range map[string]Identity{"source unavailable (no git)": {Kind: KindUnavailable}, "no-commit": id("", false, ""), "no-commit+dirty": id("", true, "x"), "abcdef0+dirty": id("abcdef0123", true, "x")} {
		if got := Describe(in); got != want {
			t.Errorf("Describe(%+v) = %q, want %q", in, got, want)
		}
	}
}
