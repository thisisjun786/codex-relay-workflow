//go:build dev

package skillport

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f fixture) edited(t *testing.T) string {
	t.Helper()
	text := strings.Replace(slurp(t, f.path("SKILL.md")), "# Demo", "# Demo (edited)", 1)
	text = strings.Replace(text, "Last line", "Last line, edited", 1)
	put(t, f.path("SKILL.md"), text, 0o644)
	put(t, f.path("scripts/extra.sh"), "#!/bin/sh\n", 0o755)
	must(t, os.Remove(f.path("references/a.md")))
	return text
}

func TestRecordedEditsAreTheOnlyAllowedDifference(t *testing.T) {
	f := newFixture(t)
	f.stage(t)
	if n, err := RecordEdits(f.root, f.src, "crw-kwrite", "unused"); err != nil || n != 0 {
		t.Fatalf("RecordEdits on a clean skill = %d, %v", n, err)
	}
	edited := f.edited(t)
	expectProblem(t, "before recording", f.problems(nil), "SKILL.md")
	if _, err := RecordEdits(f.root, f.src, "crw-kwrite", ""); err == nil {
		t.Error("new edits were recorded without a reason")
	}
	if n, err := RecordEdits(f.root, f.src, "crw-kwrite", "boundary wording"); err != nil || n != 3 {
		t.Fatalf("RecordEdits = %d, %v", n, err)
	}
	f.expectClean(t, "recorded", nil)
	f.expectClean(t, "recorded, with the source", &f.src)
	skill, err := load(f.root, "crw-kwrite")
	must(t, err)
	byFile := map[string]Edit{}
	for _, e := range skill.Edits {
		byFile[e.File] = e
	}
	if len(byFile["SKILL.md"].Hunks) != 2 || !byFile["scripts/extra.sh"].Add || !byFile["references/a.md"].Remove {
		t.Errorf("recorded edits: %+v", skill.Edits)
	}
	if n, err := RecordEdits(f.root, f.src, "crw-kwrite", "another reason"); err != nil || n != 3 {
		t.Fatalf("second RecordEdits = %d, %v", n, err)
	}
	if skill, _ = load(f.root, "crw-kwrite"); skill.Edits[0].Reason != "boundary wording" || skill.Edits[2].Reason != "boundary wording" {
		t.Errorf("an unchanged edit lost its reason: %+v", skill.Edits)
	}
	put(t, f.path("SKILL.md"), strings.Replace(edited, "(edited)", "(edited again)", 1), 0o644)
	expectProblem(t, "inside a recorded region", f.problems(nil), "SKILL.md")
}

func TestTamperedRecordsFail(t *testing.T) {
	for _, row := range []struct {
		name   string
		tamper func(s *Skill)
	}{
		{"hunk text", func(s *Skill) { s.Edits[0].Hunks[0].New[0] += "x" }},
		{"edit dropped", func(s *Skill) { s.Edits = nil }},
		{"empty hunk", func(s *Skill) { s.Edits[0].Hunks = append(s.Edits[0].Hunks, Hunk{Line: 99}) }},
		{"two edits for one file", func(s *Skill) { s.Edits = append(s.Edits, s.Edits[0]) }},
		{"file digest", func(s *Skill) { s.Files["SKILL.md"] = FileEntry{Original: "00"} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.stage(t)
			put(t, f.path("SKILL.md"), strings.Replace(slurp(t, f.path("SKILL.md")), "# Demo", "# Demo (edited)", 1), 0o644)
			if n, err := RecordEdits(f.root, f.src, "crw-kwrite", "edit"); err != nil || n != 1 {
				t.Fatalf("RecordEdits = %d, %v", n, err)
			}
			skill, err := load(f.root, "crw-kwrite")
			must(t, err)
			row.tamper(skill)
			must(t, save(f.root, "crw-kwrite", skill))
			if f.problems(nil) == "" {
				t.Error("the tampered record passed")
			}
		})
	}
}

func TestRecordEditsRefusals(t *testing.T) {
	for _, row := range []struct {
		name, want string
		change     func(t *testing.T, f fixture)
	}{
		{"binary content", "binary", func(t *testing.T, f fixture) { put(t, f.path("assets/blob.bin"), "\x00other", 0o644) }},
		{"executable bit only", "executable bit", func(t *testing.T, f fixture) { must(t, os.Chmod(f.path("scripts/run.sh"), 0o644)) }},
		{"stale originals", "stale", func(t *testing.T, f fixture) {
			skill, err := load(f.root, "crw-kwrite")
			must(t, err)
			skill.Files["SKILL.md"] = FileEntry{Original: "00"}
			must(t, save(f.root, "crw-kwrite", skill))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.stage(t)
			row.change(t, f)
			if _, err := RecordEdits(f.root, f.src, "crw-kwrite", "why"); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Errorf("RecordEdits error = %v, want %q", err, row.want)
			}
		})
	}
}

func TestLineDiffRoundTrips(t *testing.T) {
	for _, row := range [][2]string{
		{"", "a\n"}, {"a\n", ""}, {"a\nb\nc\n", "a\nc\n"}, {"a\nb\n", "a\nx\nb\n"}, {"a\nb", "a\nb\n"},
		{"a\r\nb\n", "a\nb\r\n"}, {"same\n", "same\n"}, {"1\n2\n3\n4\n5\n6\n", "1\nX\n3\n4\n5\nY\n6\n"},
	} {
		hunks := diffLines(splitLines(row[0]), splitLines(row[1]))
		if got, err := revert(splitLines(row[1]), hunks); err != nil || strings.Join(got, "") != row[0] {
			t.Errorf("%q -> %q: hunks %+v restore %q, %v", row[0], row[1], hunks, got, err)
		}
	}
	if n := len(diffLines(splitLines("1\n2\n3\n4\n5\n6\n"), splitLines("1\nX\n3\n4\n5\nY\n6\n"))); n != 2 {
		t.Errorf("two separate changes made %d hunks", n)
	}
	staged := splitLines("x\nb\n")
	for _, row := range []struct {
		name  string
		hunks []Hunk
	}{
		{"new differs from the staged lines", []Hunk{{Line: 1, Old: []string{"a\n"}, New: []string{"q\n"}}}},
		{"empty", []Hunk{{Line: 1}}},
		{"no-op", []Hunk{{Line: 1, Old: []string{"x\n"}, New: []string{"x\n"}}}},
		{"out of range", []Hunk{{Line: 9, Old: []string{"a\n"}}}},
		{"unordered", []Hunk{{Line: 2, Old: []string{"b!\n"}, New: []string{"b\n"}}, {Line: 1, Old: []string{"a\n"}, New: []string{"x\n"}}}},
		{"old is not a whole line", []Hunk{{Line: 1, Old: []string{"a"}, New: []string{"x\n"}}}},
	} {
		if got, err := revert(staged, row.hunks); err == nil {
			t.Errorf("%s: restored %q", row.name, got)
		} else if row.name == "unordered" && !strings.Contains(err.Error(), "before the end") {
			t.Errorf("unordered hunks reached the wrong refusal: %v", err)
		}
	}
}

func TestDefaultOriginMatchesTheOracle(t *testing.T) {
	oracle := os.Getenv("CRW_SKILLPORT_ORACLE")
	if oracle == "" {
		t.Skip("CRW_SKILLPORT_ORACLE is not set")
	}
	oracle = filepath.Join(oracle, "plugins/codexclaw/skills")
	if _, err := os.Stat(oracle); err != nil {
		t.Skip("the extracted CXC v0.2.40 tree is not on this host")
	}
	if got, err := Listing(oracle); err != nil || got != DefaultOrigin().SkillsListing {
		t.Errorf("Listing(oracle) = %q, %v; DefaultOrigin has %q", got, err, DefaultOrigin().SkillsListing)
	}
}

func TestRun(t *testing.T) {
	f := newFixture(t)
	do := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(args, &out, &errb, f.src.Origin)
		return code, out.String(), errb.String()
	}
	for _, row := range [][]string{{}, {"nope"}, {"stage", "--root", f.root, "kwrite"}, {"check", "extra"}} {
		if code, _, _ := do(row...); code != 2 {
			t.Errorf("run(%q) exit %d, want 2", row, code)
		}
	}
	if code, out, errs := do("stage", "--root", f.root, "--source", f.src.Dir, "kwrite"); code != 0 || !strings.Contains(out, "staged crw-kwrite") {
		t.Fatalf("stage: %d %q %q", code, out, errs)
	}
	if code, out, _ := do("check", "--root", f.root, "--source", f.src.Dir); code != 0 || !strings.Contains(out, "Checked 1 staged skills") || !strings.Contains(out, "fidelity only") {
		t.Errorf("check: %d %q", code, out)
	}
	put(t, f.path("SKILL.md"), "changed\n", 0o644)
	if code, _, errs := do("check", "--root", f.root); code != 1 || !strings.Contains(errs, "SKILL.md: differs") {
		t.Errorf("check after a change: %d %q", code, errs)
	}
	if code, _, _ := do("edits", "--root", f.root, "--source", f.src.Dir, "crw-kwrite"); code != 1 {
		t.Errorf("edits without a reason exit %d", code)
	}
	if code, out, errs := do("edits", "--root", f.root, "--source", f.src.Dir, "--reason", "why", "crw-kwrite"); code != 0 || !strings.Contains(out, "crw-kwrite: 1 recorded edits") {
		t.Errorf("edits: %d %q %q", code, out, errs)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"stage", "--root", f.root, "--source", f.src.Dir, "kwrite"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "does not match the pinned origin") {
		t.Errorf("Run with a synthetic tree: %d %q", code, errb.String())
	}
}

func TestEditsOutsideTheRecordFail(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(*testing.T, fixture)
	}{
		{"unrecorded line", func(t *testing.T, f fixture) {
			put(t, f.path("SKILL.md"), slurp(t, f.path("SKILL.md"))+"Outside the hunks\n", 0o644)
		}},
		{"added content", func(t *testing.T, f fixture) { put(t, f.path("scripts/extra.sh"), "changed\n", 0o755) }},
		{"added mode", func(t *testing.T, f fixture) { must(t, os.Chmod(f.path("scripts/extra.sh"), 0o644)) }},
		{"added missing", func(t *testing.T, f fixture) { must(t, os.Remove(f.path("scripts/extra.sh"))) }},
		{"removed present", func(t *testing.T, f fixture) { put(t, f.path("references/a.md"), "back\n", 0o644) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.stage(t)
			f.edited(t)
			_, err := RecordEdits(f.root, f.src, "crw-kwrite", "test")
			must(t, err)
			row.change(t, f)
			if f.problems(nil) == "" {
				t.Fatal("unrecorded change passed")
			}
		})
	}
}

func TestMalformedEditsAreRejected(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(*Skill)
	}{
		{"old text", func(s *Skill) { s.Edits[0].Hunks[0].Old[0] += "x" }},
		{"negative position", func(s *Skill) { s.Edits[0].Hunks[0].Line = -1 }},
		{"huge position", func(s *Skill) { s.Edits[0].Hunks[0].Line = int(^uint(0) >> 1) }},
		{"blank reason", func(s *Skill) { s.Edits[0].Reason = " \t" }},
		{"irrelevant digest", func(s *Skill) { s.Edits[0].SHA256 = "00" }},
		{"irrelevant mode", func(s *Skill) { s.Edits[0].Exec = true }},
		{"both kinds", func(s *Skill) { s.Edits[0].Remove = true }},
		{"unsafe edit path", func(s *Skill) { s.Edits[0].File = "../outside" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.stage(t)
			put(t, f.path("SKILL.md"), "edited\n", 0o644)
			_, err := RecordEdits(f.root, f.src, "crw-kwrite", "why")
			must(t, err)
			f.rewrite(t, row.change)
			if f.problems(nil) == "" {
				t.Fatal("malformed record passed")
			}
		})
	}
}

func TestRecordingRefusalsPreserveTheRecord(t *testing.T) {
	for _, row := range []struct {
		name, want string
		change     func(*testing.T, fixture)
	}{
		{"changed table", "name-substitution", func(t *testing.T, f fixture) { f.rewrite(t, func(s *Skill) { s.Table = "00" }) }},
		{"origin mismatch", "origin", func(t *testing.T, f fixture) {
			put(t, filepath.Join(f.src.skills(), "kwrite/SKILL.md"), "different source\n", 0o644)
		}},
		{"source mode", "origin", func(t *testing.T, f fixture) {
			must(t, os.Chmod(filepath.Join(f.src.skills(), "kwrite/scripts/run.sh"), 0o644))
		}},
		{"no reason", "reason", func(t *testing.T, f fixture) {}},
		{"binary", "binary", func(t *testing.T, f fixture) { put(t, f.path("assets/blob.bin"), "\x00changed", 0o644) }},
		{"mode", "executable bit", func(t *testing.T, f fixture) { must(t, os.Chmod(f.path("scripts/run.sh"), 0o644)) }},
		{"stale file set", "stale", func(t *testing.T, f fixture) { f.rewrite(t, func(s *Skill) { delete(s.Files, "references/a.md") }) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.stage(t)
			put(t, f.path("SKILL.md"), "edited\n", 0o644)
			row.change(t, f)
			p := recordPath(f.root, "crw-kwrite")
			before := slurp(t, p)
			reason := "why"
			if row.name == "no reason" {
				reason = " \t"
			}
			if _, err := RecordEdits(f.root, f.src, "crw-kwrite", reason); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("got %v, want %s", err, row.want)
			}
			if slurp(t, p) != before {
				t.Fatal("refusal changed record")
			}
		})
	}
	for _, name := range []string{"../outside", "crw-../outside", "crw-", "kwrite"} {
		f := newFixture(t)
		f.stage(t)
		if _, err := RecordEdits(f.root, f.src, name, "why"); err == nil || !strings.Contains(err.Error(), "name") {
			t.Errorf("unsafe name %q: %v", name, err)
		}
	}
	f := newFixture(t)
	f.stage(t)
	table := filepath.Join(f.root, "contract/schema/cxc/name-substitution.json")
	put(t, table, strings.Replace(slurp(t, table), `"crw-kwrite"`, `""`, 1), 0o644)
	if _, err := RecordEdits(f.root, f.src, "crw-kwrite", "why"); err == nil || !strings.Contains(err.Error(), "no longer ports") {
		t.Errorf("stub: %v", err)
	}
}

func TestRecordEditsRefusesLinkedLayout(t *testing.T) {
	for _, rel := range []string{StagingRoot, RecordDir} {
		t.Run(rel, func(t *testing.T) {
			f := newFixture(t)
			f.stage(t)
			p := filepath.Join(f.root, rel)
			moved := p + "-moved"
			must(t, os.Rename(p, moved))
			must(t, os.Symlink(moved, p))
			before := slurp(t, recordPath(f.root, "crw-kwrite"))
			if _, err := RecordEdits(f.root, f.src, "crw-kwrite", "why"); err == nil || !strings.Contains(err.Error(), "plain directory") {
				t.Fatalf("linked layout: %v", err)
			}
			if slurp(t, recordPath(f.root, "crw-kwrite")) != before {
				t.Fatal("refusal changed record")
			}
		})
	}
}

func TestSaveRefusalPreservesRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	f := newFixture(t)
	f.stage(t)
	put(t, f.path("SKILL.md"), "edited\n", 0o644)
	p := recordPath(f.root, "crw-kwrite")
	before := slurp(t, p)
	dir := filepath.Dir(p)
	must(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if _, err := RecordEdits(f.root, f.src, "crw-kwrite", "why"); err == nil {
		t.Fatal("read-only record directory accepted")
	}
	if slurp(t, p) != before {
		t.Fatal("write refusal changed record")
	}
}

func TestLineDiffFallbackAndOverflow(t *testing.T) {
	a, b := splitLines(strings.Repeat("a\n", 2050)), splitLines(strings.Repeat("b\n", 2050))
	hunks := diffLines(a, b)
	if len(hunks) != 1 {
		t.Fatalf("fallback hunks: %d", len(hunks))
	}
	if got, err := revert(b, hunks); err != nil || strings.Join(got, "") != strings.Join(a, "") {
		t.Fatalf("fallback: %v", err)
	}
	huge := int(^uint(0) >> 1)
	for _, hs := range [][]Hunk{{{Line: huge, New: []string{"x\n", "y\n"}}}, {{Line: 1, New: []string{"x\n"}}, {Line: huge, Old: []string{"z\n"}}}, {{Line: -1, Old: []string{"a\n"}}}} {
		if _, err := revert([]string{"x\n"}, hs); err == nil {
			t.Fatal("invalid position accepted")
		}
	}
}

func TestOverlappingHunksReachOrderingGuard(t *testing.T) {
	staged := splitLines("x\nb\n")
	hunks := []Hunk{{Line: 1, Old: []string{"a\n", "removed\n"}, New: []string{"x\n"}}, {Line: 2, Old: []string{"old\n"}, New: []string{"b\n"}}}
	if _, err := revert(staged, hunks); err == nil || !strings.Contains(err.Error(), "before the end") {
		t.Fatalf("overlap must reach ordering guard: %v", err)
	}
}
