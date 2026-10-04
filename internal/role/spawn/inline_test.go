package spawn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type spawnInlineFixture struct {
	Skills map[string]string
	Groups []struct {
		Number int
		Name   string
		Cases  []struct {
			Op, Input, Expected, Classification, Reason, Suffix string
			Folders                                             []string
			Identity                                            bool
			Recipe                                              struct {
				Unit, Tail string
				N, Closes  int
			}
		}
	}
	Guards  map[string]struct{ Expected, Classification, Reason string }
	Decoder []struct {
		Bytes    []byte
		Expected string
	}
	CatalogCases []struct{ Body, Expected string }
}

func spawnInlineTestFixture(t *testing.T) spawnInlineFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/inline/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var f spawnInlineFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func spawnInlineTestWrite(t *testing.T, dir, folder, body string) {
	t.Helper()
	p := filepath.Join(dir, folder, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func spawnInlineTestSkills(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range spawnInlineTestFixture(t).Skills {
		spawnInlineTestWrite(t, root, name, body)
	}
	return root
}

// Each group executes the same direct call inputs as an original B-class callback.
func TestSpawnInlineBClass(t *testing.T) {
	f := spawnInlineTestFixture(t)
	root := spawnInlineTestSkills(t)
	if len(f.Groups) != 13 {
		t.Fatalf("got %d callback groups, want 13", len(f.Groups))
	}
	expand := func(s string) string { return strings.ReplaceAll(s, "${SKILLS}", root) }
	for i, group := range f.Groups {
		if group.Number != i+1 {
			t.Fatal("non-sequential callback groups")
		}
		t.Run(fmt.Sprintf("B%02d_%s", group.Number, group.Name), func(t *testing.T) {
			if len(group.Cases) == 0 {
				t.Fatal("callback has no direct calls")
			}
			for n, c := range group.Cases {
				if c.Classification != "identical" && (c.Classification != "intentionally-changed" || c.Reason == "") {
					t.Fatal("unclassified oracle case")
				}
				input := expand(c.Input)
				want := expand(c.Expected)
				if c.Recipe.N > 0 {
					input = strings.Repeat(c.Recipe.Unit, c.Recipe.N) + strings.Repeat("</skill>", c.Recipe.Closes) + c.Recipe.Tail
					want = input
					if !c.Identity {
						want += expand(c.Suffix)
					}
				}
				var got string
				switch c.Op {
				case "inlineSkillBodies":
					got = InlineSkillBodies(input, root)
				case "skillAffordanceBlock":
					got = SkillAffordanceBlock(root)
				case "buildLeafSkillCatalog":
					got = BuildLeafSkillCatalog(root)
				case "mentionedFolders":
					var folders []string
					for name := range MentionedFolders(input) {
						folders = append(folders, name)
					}
					slices.Sort(folders)
					if !reflect.DeepEqual(folders, c.Folders) {
						t.Errorf("case %d: folders %v want %v", n, folders, c.Folders)
					}
					continue
				default:
					t.Fatalf("unknown op %q", c.Op)
				}
				if got != want {
					t.Errorf("case %d: got %d bytes want %d; prefixes %q vs %q", n, len(got), len(want), got[:min(len(got), 180)], want[:min(len(want), 180)])
				}
			}
		})
	}
}

func TestSpawnInlineGuardOracle(t *testing.T) {
	got := map[string]string{"LEAF_GUARD_BLOCK": LeafGuardBlock, "LEAF_GUARD_BLOCK_COORDINATOR": LeafGuardBlockCoordinator, "V1_SCOPE_BLOCK": V1ScopeBlock, "V1_SCOPE_BLOCK_COORDINATOR": V1ScopeBlockCoordinator}
	for name, c := range spawnInlineTestFixture(t).Guards {
		if got[name] != c.Expected || c.Classification != "intentionally-changed" || c.Reason == "" {
			t.Errorf("guard %s differs from classified oracle", name)
		}
	}
}

func TestSpawnInlineAtomicUTF16Boundary(t *testing.T) {
	root := t.TempDir()
	spawnInlineTestWrite(t, root, "crw-dev", "D")
	spawnInlineTestWrite(t, root, "crw-search", "S")
	for _, body := range []string{"D", "😀"} {
		spawnInlineTestWrite(t, root, "crw-dev", body)
		block := "<skill name=\"crw-dev\">\n" + body + "\n</skill>"
		units := len(block)
		if body == "😀" {
			units -= 2
		}
		input := "$crw-dev" + strings.Repeat("x", 256*1024-units-2-len("$crw-dev"))
		// A separating space avoids extending the mention's folder.
		input = "$crw-dev " + input[len("$crw-dev")+1:]
		if got := InlineSkillBodies(input, root); got != input+"\n\n"+block {
			t.Fatal("exact UTF-16 cap did not attach")
		}
		if got := InlineSkillBodies(input+"x", root); got != input+"x" {
			t.Fatal("one unit over cap partially attached")
		}
	}
	input := "$crw-dev $crw-search " + strings.Repeat("x", 256*1024-70)
	if got := InlineSkillBodies(input, root); got != input {
		t.Fatal("multi-body overflow attached a prefix")
	}
	astral := strings.Repeat("😀", 128*1024) + " $crw-dev"
	if got := InlineSkillBodies(astral, root); got != astral {
		t.Fatal("astral early cap used bytes/runes")
	}
}

func TestSpawnInlineItemBoundaries(t *testing.T) {
	root := spawnInlineTestSkills(t)
	if got := SkillBlocks([]string{"$crw-", "dev"}, root); len(got) != 0 {
		t.Fatal("mention crossed item boundary")
	}
	if got := SkillBlocks([]string{"$crw-search", "$crw-dev $crw-search"}, root); len(got) != 2 || !strings.HasPrefix(got[0], "<skill name=\"crw-dev\">") {
		t.Fatal("global dedupe or sort differs")
	}
	closed := InlineSkillBodies("$crw-dev", root)
	if got := SkillBlocks([]string{closed, "$crw-dev"}, root); len(got) != 0 {
		t.Fatal("closed block did not dedupe globally")
	}
	if got := SkillBlocks([]string{strings.Repeat("x", 256*1024-1), "$crw-search"}, root); len(got) != 0 {
		t.Fatal("aggregate item cap ignored separators")
	}
	spawnInlineTestWrite(t, root, "crw-dev", strings.Repeat("D", 256*1024))
	if got := SkillBlocks([]string{"$crw-dev"}, root); len(got) != 1 || len(got[0]) <= 256*1024 {
		t.Fatal("collector unexpectedly imposed final output cap")
	}
	if got := InlineSkillBodies("$crw-dev", root); got != "$crw-dev" {
		t.Fatal("final single-message output cap missing")
	}
}

func TestSpawnInlineSafeReads(t *testing.T) {
	for _, shape := range []string{"file-link-outside", "file-link-inside", "folder-link-outside", "folder-link-inside", "file-directory", "file-missing", "empty", "not-allowlisted"} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			spawnInlineTestWrite(t, root, "crw-dev", "safe")
			spawnInlineTestWrite(t, outside, "target", "OUTSIDE-SENTINEL")
			target := filepath.Join(outside, "target", "SKILL.md")
			victim := filepath.Join(root, "crw-dev", "SKILL.md")
			switch shape {
			case "file-link-outside", "file-link-inside":
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
				if shape == "file-link-inside" {
					spawnInlineTestWrite(t, root, "target", "OUTSIDE-SENTINEL")
					target = "../target/SKILL.md"
				}
				if err := os.Symlink(target, victim); err != nil {
					t.Fatal(err)
				}
			case "folder-link-outside", "folder-link-inside":
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Dir(victim)); err != nil {
					t.Fatal(err)
				}
				target = filepath.Dir(target)
				if shape == "folder-link-inside" {
					spawnInlineTestWrite(t, root, "target", "OUTSIDE-SENTINEL")
					target = "target"
				}
				if err := os.Symlink(target, filepath.Dir(victim)); err != nil {
					t.Fatal(err)
				}
			case "file-directory", "file-missing":
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
				if shape == "file-directory" {
					if err := os.Mkdir(victim, 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "empty":
				spawnInlineTestWrite(t, root, "crw-dev", "\ufeff \n")
			case "not-allowlisted":
				spawnInlineTestWrite(t, root, "crw-dev-future", "OUTSIDE-SENTINEL")
			}
			input := "$crw-dev"
			if shape == "not-allowlisted" {
				input = "$crw-dev-future"
			}
			if got := InlineSkillBodies(input, root); got != input {
				t.Fatalf("unsafe/missing/empty skill attached: %q", got)
			}
			if shape != "empty" && shape != "not-allowlisted" {
				if got := BuildLeafSkillCatalog(root); got != "" {
					t.Fatalf("unsafe catalog: %q", got)
				}
			}
			if got := InlineSkillBodies("[x](skill://"+target+")", root); strings.Contains(got, "OUTSIDE-SENTINEL") {
				t.Fatal("link target read directly")
			}
		})
	}
	root := t.TempDir()
	if InlineSkillBodies("$crw-dev", "") != "$crw-dev" || BuildLeafSkillCatalog(filepath.Join(root, "absent")) != "" {
		t.Fatal("empty/missing root differs")
	}
	spawnInlineTestWrite(t, root, "crw-dev", "\ufeff D \n")
	if got := InlineSkillBodies("$crw-dev", root); got != "$crw-dev\n\n<skill name=\"crw-dev\">\nD\n</skill>" {
		t.Fatal("JS trim differs")
	}
}

func TestSpawnInlineCatalogQuirks(t *testing.T) {
	for i, c := range spawnInlineTestFixture(t).CatalogCases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			root := t.TempDir()
			spawnInlineTestWrite(t, root, "crw-dev", c.Body)
			want := strings.ReplaceAll(c.Expected, "${SKILLS}", root)
			if got := BuildLeafSkillCatalog(root); got != want {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
	root := t.TempDir()
	spawnInlineTestWrite(t, root, "crw-dev", "name: crw-dev\ndescription: "+strings.Repeat("x", 119)+"😀\n")
	want := "Available skills (self-load from " + root + "/<name>/SKILL.md):\n- crw-dev: " + strings.Repeat("x", 119) + "\xed\xa0\xbd"
	if got := BuildLeafSkillCatalog(root); got != want {
		t.Fatalf("UTF-16 description split differs: %q", got)
	}
	spawnInlineTestWrite(t, root, "crw-dev", strings.Repeat("😀", 512)+"\nname: crw-dev\n")
	if got := BuildLeafSkillCatalog(root); got != "" {
		t.Fatal("catalog head is not 1024 UTF-16 units")
	}
}

func TestSpawnInlineDecodeUTF8(t *testing.T) {
	for i, c := range spawnInlineTestFixture(t).Decoder {
		if got := spawnInlineDecodeUTF8(c.Bytes); got != c.Expected {
			t.Errorf("Node decoder case %d got %q want %q", i, got, c.Expected)
		}
	}
	root := t.TempDir()
	spawnInlineTestWrite(t, root, "crw-dev", string([]byte{0xe2, 0x82}))
	if got := InlineSkillBodies("$crw-dev", root); got != "$crw-dev\n\n<skill name=\"crw-dev\">\n�\n</skill>" {
		t.Fatal("filesystem UTF8 replacement differs")
	}
}

func TestSpawnInlineAllowlistAndMentions(t *testing.T) {
	folders := LeafSafeSkillFolders()
	if len(folders) != 20 {
		t.Fatal("allowlist length differs")
	}
	folders[0] = "mutated"
	if LeafSafeSkillFolders()[0] == "mutated" {
		t.Fatal("shared mutable allowlist")
	}
	root := t.TempDir()
	for _, folder := range LeafSafeSkillFolders() {
		spawnInlineTestWrite(t, root, folder, "body")
	}
	var mentions []string
	for _, folder := range LeafSafeSkillFolders() {
		mentions = append(mentions, "$"+folder)
	}
	if got := SkillBlocks(mentions, root); len(got) != 20 {
		t.Fatalf("got %d allowlisted blocks", len(got))
	}
	cases := map[string][]string{"$CRW:CRW-DEV $crw-SEARCH": {"crw-dev", "crw-search"}, "prefix$crw-dev \\$crw-search": {"crw-dev", "crw-search"}, "skill:///x/crw-dev/SKILL.md": {"crw-dev"}, "skill:///x/other/SKILL.md": {"other"}, "$cxc-dev $codexclaw:cxc-search": {}}
	for input, want := range cases {
		var got []string
		for f := range MentionedFolders(input) {
			got = append(got, f)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%q: %v want %v", input, got, want)
		}
	}
}

func TestSpawnInlineScannerAdversarial(t *testing.T) {
	root := spawnInlineTestSkills(t)
	for _, s := range []string{"<skill name=\"crw-\">$crw-search</skill>", "<skill name=\"crw-dev broken $crw-search", "<skill name=\"crw-dev\"><skill name=\"crw-search broken </skill> $crw-search"} {
		if got := InlineSkillBodies(s, root); got == s {
			t.Fatal("malformed/unbalanced blocks suppressed attachment")
		}
	}
	closed := "<skill name=\"crw-dev\">$crw-search</skill>"
	if got := InlineSkillBodies(closed, root); got != closed {
		t.Fatal("closed interior re-scanned")
	}
	nested := "<skill name=\"crw-dev\">outer <skill name=\"crw-search\">inner</skill> outer tail $crw-dev-testing</skill>"
	if got := InlineSkillBodies(nested, root); got != nested {
		t.Fatal("allowed mention in outer tail escaped nested scanner")
	}
	started := time.Now()
	flood := strings.Repeat("<skill name=\"crw-dev\">", 8000) + strings.Repeat("</skill>", 8000) + " $crw-search"
	if got := InlineSkillBodies(flood, root); !strings.Contains(got, "<skill name=\"crw-search\">") {
		t.Fatal("outside flood mention missing")
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("delimiter flood scan exceeded oracle bound")
	}
}
