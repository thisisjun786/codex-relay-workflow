package spawn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func spawnNormalizeTestSkills(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"crw-dev", "crw-search"} {
		path := filepath.Join(root, name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# synthetic skill\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func spawnNormalizeTestLink(root, name string) string {
	return "[$" + name + "](skill://" + filepath.Join(root, name, "SKILL.md") + ")"
}

// These are the 24 direct B-class tests, recorded by running the original
// upstream test callbacks. Large messages are generated here, not committed.
func TestNormalizeSkillMentionsBClass(t *testing.T) {
	var fixture struct {
		Groups []struct {
			Number       int
			Name, Recipe string
			Cases        []struct{ Input, Skills, Expected, Classification, Reason string }
		}
	}
	raw, err := os.ReadFile("testdata/normalize/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Groups) != 24 {
		t.Fatalf("got %d B-class groups, want 24", len(fixture.Groups))
	}
	root := t.TempDir()
	skills, alt, unsafe := filepath.Join(root, "skills"), filepath.Join(root, "alternate"), filepath.Join(root, "unsafe skills")
	for _, dir := range []string{skills, alt, unsafe} {
		spawnNormalizeTestSkills(t, dir)
	}
	expand := strings.NewReplacer("${SKILLS}", skills, "${ALT1}", alt, "${UNSAFE1}", unsafe).Replace
	for i, group := range fixture.Groups {
		if group.Number != i+1 {
			t.Fatalf("group %d has number %d", i+1, group.Number)
		}
		t.Run(fmt.Sprintf("B%02d_%s", group.Number, group.Name), func(t *testing.T) {
			for n, c := range group.Cases {
				if c.Classification != "identical" && (c.Classification != "intentionally-changed" || c.Reason == "") {
					t.Fatal("unclassified oracle case")
				}
				input, want := expand(c.Input), expand(c.Expected)
				if got := NormalizeSkillMentions(input, expand(c.Skills)); got != want {
					t.Errorf("case %d: got %q, want %q", n+1, got, want)
				}
			}
			switch group.Recipe {
			case "adversarial-floods":
				spawnNormalizeTestFloods(t, skills)
			case "over-limit":
				input := "$crw-dev " + strings.Repeat("x", 256*1024)
				if got := NormalizeSkillMentions(input, skills); got != input {
					t.Errorf("over-limit input changed: got %d bytes, want %d", len(got), len(input))
				}
			case "":
				if len(group.Cases) == 0 {
					t.Fatal("group has no assertions")
				}
			default:
				t.Fatalf("unknown large-input recipe %q", group.Recipe)
			}
			if group.Number == 24 {
				var manifest struct{ Name string }
				raw, err := os.ReadFile("../../../plugins/crw/.codex-plugin/plugin.json")
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				if manifest.Name != "crw" {
					t.Fatalf("plugin namespace %q", manifest.Name)
				}
				if got, want := NormalizeSkillMentions("$"+manifest.Name+":crw-dev", skills), spawnNormalizeTestLink(skills, "crw-dev"); got != want {
					t.Errorf("manifest namespace: got %q, want %q", got, want)
				}
			}
		})
	}
}

func spawnNormalizeTestFloods(t *testing.T, skills string) {
	t.Helper()
	const size = 128 * 1024
	title, repair := `[$crw-dev](/x "(") `, "[$crw-dev](/x)\n"
	cases := []struct {
		name, input string
		identity    bool
	}{
		{"unmatched bracket", strings.Repeat("[", size), true},
		{"mid-line tilde", "x " + strings.Repeat("~", size), true},
		{"paren title", strings.Repeat(title, (size+len(title)-1)/len(title)), true},
		{"standalone repair", strings.Repeat(repair, (size+len(repair)-1)/len(repair)), false},
	}
	for _, c := range cases {
		if len(c.input) < size {
			t.Fatal("flood below 128 KiB")
		}
		start := time.Now()
		got := NormalizeSkillMentions(c.input, skills)
		elapsed := time.Since(start)
		if elapsed >= time.Second {
			t.Errorf("%s took %s, budget 1s on native Linux", c.name, elapsed)
		}
		if c.identity && got != c.input {
			t.Errorf("%s was not preserved", c.name)
		}
		if !c.identity && !strings.Contains(got, spawnNormalizeTestLink(skills, "crw-dev")) {
			t.Errorf("%s did not repair", c.name)
		}
		if !c.identity {
			want := strings.Repeat(spawnNormalizeTestLink(skills, "crw-dev")+"\n", (size+len(repair)-1)/len(repair))
			if got != want {
				t.Errorf("%s incomplete output: got %d bytes, want %d", c.name, len(got), len(want))
			}
		}
		t.Logf("%s: %d input bytes in %s", c.name, len(c.input), elapsed)
	}
}

func TestNormalizeUTF16Limit(t *testing.T) {
	skills := t.TempDir()
	spawnNormalizeTestSkills(t, skills)
	const limit = 256 * 1024
	prefix := "$crw-dev "
	for _, unit := range []string{"x", "\U0001f600", "\xed\xa0\x80"} {
		units := 1
		if unit == "\U0001f600" {
			units = 2
		}
		tail := strings.Repeat(unit, (limit-len(prefix))/units)
		if units == 2 {
			tail += "x"
		}
		input := prefix + tail
		want := spawnNormalizeTestLink(skills, "crw-dev") + " " + tail
		if got := NormalizeSkillMentions(input, skills); got != want {
			t.Errorf("unit %q at limit: got %d bytes, want %d", unit, len(got), len(want))
		}
		input += "x"
		if got := NormalizeSkillMentions(input, skills); got != input {
			t.Errorf("unit %q above limit changed", unit)
		}
	}
}

func TestNormalizeWhitespaceAndTokenBoundaries(t *testing.T) {
	for _, suffix := range []string{"space here", "(paren)", "\t", "\u00a0", "\ufeff", "\u0085", "<angle>", "quote'"} {
		t.Run(fmt.Sprintf("root_%q", suffix), func(t *testing.T) {
			skills := filepath.Join(t.TempDir(), suffix)
			spawnNormalizeTestSkills(t, skills)
			want := "$crw:crw-dev" // an angle bracket or a quote cannot stand in a raw link target either (CRW-1114)
			if suffix == "\u0085" {
				want = spawnNormalizeTestLink(skills, "crw-dev")
			}
			if got := NormalizeSkillMentions("$crw-dev", skills); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	skills := t.TempDir()
	spawnNormalizeTestSkills(t, skills)
	link := spawnNormalizeTestLink(skills, "crw-dev")
	for _, extended := range []string{"_", ":", "A", "0", "-unknown"} {
		input := "$crw-dev" + extended
		if got := NormalizeSkillMentions(input, skills); got != input {
			t.Errorf("extended token %q changed", input)
		}
	}
	for _, input := range []string{"$crw-dev. $crw:crw-dev!", "$crw-dev\u0085", " $crw-dev", "($crw-dev)"} {
		want := strings.NewReplacer("$crw:crw-dev", link, "$crw-dev", link).Replace(input)
		if got := NormalizeSkillMentions(input, skills); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	for _, input := range []string{"prefix$crw-dev", "\\$crw-dev", "a_$crw-dev", "9$crw-dev"} { // not at a token start (CRW-1114)
		if got := NormalizeSkillMentions(input, skills); got != input {
			t.Errorf("got %q, want it unchanged", got)
		}
	}
	for _, target := range []string{"/bad\u00a0target", "/bad\ufefftarget", "/bad\ttarget", "/bad\\target"} {
		input := "[$crw-dev](" + target + ")"
		if got := NormalizeSkillMentions(input, skills); got != input {
			t.Errorf("unsafe target %q changed", input)
		}
	}
	input := "[$crw-dev](/bad\u0085target)\t\r"
	if got := NormalizeSkillMentions(input, skills); got != link+"\t\r" {
		t.Errorf("non-JS whitespace target: %q", got)
	}
}

func TestNormalizeFilesystemAndPrefixSemantics(t *testing.T) {
	skills := t.TempDir()
	spawnNormalizeTestSkills(t, skills)
	link := spawnNormalizeTestLink(skills, "crw-dev")
	for _, input := range []string{"$crw-dev", "[$crw-dev](/broken)"} {
		for _, dir := range []string{"", filepath.Join(skills, "missing"), "\x00"} {
			if got := NormalizeSkillMentions(input, dir); got != input {
				t.Errorf("invalid directory %q changed %q", dir, input)
			}
		}
	}
	for n := 8; n <= 9; n++ {
		prefix := strings.Repeat(" ", n)
		input, want := prefix+"[$crw-dev](/broken)", prefix+link
		if n == 9 {
			want = input
		}
		if got := NormalizeSkillMentions(input, skills); got != want {
			t.Errorf("%d prefix tokens: got %q, want %q", n, got, want)
		}
	}
	for _, trailer := range []string{"", "/", "//"} {
		input := "[$crw-dev](" + filepath.Join(skills, "crw-dev", "SKILL.md") + trailer + ")"
		want := input
		if trailer != "" {
			want = link
		}
		if got := NormalizeSkillMentions(input, skills); got != want {
			t.Errorf("raw target %q: got %q, want %q", trailer, got, want)
		}
	}
	for _, prefix := range []string{"123456789. ", "> + ", "- - "} {
		input := prefix + "[$crw-dev](/broken)"
		if got := NormalizeSkillMentions(input, skills); got != prefix+link {
			t.Errorf("prefix %q: %q", prefix, got)
		}
	}
	input := "1234567890. [$crw-dev](/broken)"
	if got := NormalizeSkillMentions(input, skills); got != input {
		t.Errorf("ten-digit prefix changed: %q", got)
	}
	dirTarget := filepath.Join(skills, "crw-directory", "SKILL.md")
	if err := os.MkdirAll(dirTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory named SKILL.md is no skill: no load link, and a link to one is repaired to the real skill (CRW-1114).
	if got := NormalizeSkillMentions("$crw-directory", skills); got != "$crw-directory" {
		t.Errorf("directory SKILL.md: got %q", got)
	}
	input = "[$crw-dev](" + dirTarget + ")"
	if got := NormalizeSkillMentions(input, skills); got != link {
		t.Errorf("existing directory target: got %q, want %q", got, link)
	}
	alias := filepath.Join(skills, "crw-alias")
	if err := os.Symlink(filepath.Join(skills, "crw-dev"), alias); err != nil {
		t.Fatal(err)
	}
	if got, want := NormalizeSkillMentions("$crw-alias", skills), spawnNormalizeTestLink(skills, "crw-alias"); got != want {
		t.Errorf("symlink alias: got %q, want %q", got, want)
	}
}

func TestNormalizeRecordedCharacterization(t *testing.T) {
	var fixture struct {
		Characterization []struct{ Input, Skills, Expected string }
	}
	raw, err := os.ReadFile("testdata/normalize/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Characterization) != 8 {
		t.Fatalf("got %d characterization cases, want 8", len(fixture.Characterization))
	}
	root := t.TempDir()
	skills, angle := filepath.Join(root, "skills"), filepath.Join(root, "angle<skills")
	spawnNormalizeTestSkills(t, skills)
	spawnNormalizeTestSkills(t, angle)
	if err := os.MkdirAll(filepath.Join(skills, "crw-directory", "SKILL.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	expand := strings.NewReplacer("${SKILLS}", skills, "${ANGLE}", angle).Replace
	for i, c := range fixture.Characterization {
		if got, want := NormalizeSkillMentions(expand(c.Input), expand(c.Skills)), expand(c.Expected); got != want {
			t.Errorf("case %d: got %q, want %q", i+1, got, want)
		}
	}
}
