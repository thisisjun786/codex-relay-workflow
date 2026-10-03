//go:build dev

package skillport

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// entries is the names in root/dir (without suffix), ignoring hidden temp items; a missing
// directory has none.
func entries(root, dir, suffix string) (map[string]bool, error) {
	found := map[string]bool{}
	list, err := os.ReadDir(filepath.Join(root, dir))
	for _, e := range list {
		if name, ok := strings.CutSuffix(e.Name(), suffix); ok && !strings.HasPrefix(e.Name(), ".") {
			found[name] = true
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return found, err
}

// Check verifies every staged skill against its record and returns how many it looked at and the
// problems found, each as "<path>: <what is wrong>". Nothing staged and no record is silent. With a
// source it also checks that tree against the record origin and renders the originals again.
func Check(root string, src *Source) (int, []string) {
	var problems []string
	report := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if err := layout(root); err != nil {
		return 0, []string{err.Error()}
	}
	recorded, err1 := entries(root, RecordDir, ".json")
	staged, err2 := entries(root, StagingRoot, "")
	if err := errors.Join(err1, err2); err != nil {
		return 0, []string{err.Error()}
	}
	set := map[string]bool{}
	maps.Copy(set, recorded)
	maps.Copy(set, staged)
	names := slices.Sorted(maps.Keys(set))
	if len(names) == 0 {
		return 0, nil
	}
	sub, err := newSubstituter(root)
	if err != nil {
		return 0, []string{err.Error()}
	}
	listing := ""
	if src != nil {
		if listing, err = Listing(src.skills()); err != nil {
			report("source: %v", err)
			src = nil
		}
	}
	var origin *Origin
	for _, name := range names {
		dir, rec := StagingRoot+"/"+name, RecordDir+"/"+name+".json"
		if !recorded[name] || !staged[name] {
			report("%s: staged skill %s", dir, map[bool]string{true: "has no record", false: "is missing"}[staged[name]])
			continue
		}
		skill, err := load(root, name)
		if err == nil {
			err = skill.validate(name)
		}
		if err != nil {
			report("%s: %v", rec, err)
			continue
		}
		if origin == nil {
			origin = &skill.Origin
		} else if *origin != skill.Origin {
			report("%s: origin differs from the other records", rec)
		}
		if sub.isStub(skill.From) {
			report("%s: the name table no longer ports %s", rec, skill.From)
		}
		if skill.Table != sub.digest {
			report("%s: name-substitution table changed since this skill was staged; skillport check --source shows which skills it changes, the package comment says how to refresh", rec)
		}
		tree, err := readTree(filepath.Join(root, dir))
		if err != nil {
			report("%s: %v", dir, err)
			continue
		}
		problems = append(problems, compare(dir, skill, tree)...)
		if src != nil {
			problems = append(problems, replay(rec, *src, listing, sub, skill)...)
		}
	}
	return len(names), problems
}

// compare is the differences between a staged tree and what its record accounts for.
func compare(dir string, skill *Skill, tree map[string]file) []string {
	edits := map[string]Edit{}
	names := map[string]bool{}
	for p := range skill.Files {
		names[p] = true
	}
	for p := range tree {
		names[p] = true
	}
	for _, e := range skill.Edits {
		edits[e.File], names[e.File] = e, true
	}
	var out []string
	for _, p := range slices.Sorted(maps.Keys(names)) {
		got, have := tree[p]
		want, inOriginal := skill.Files[p]
		e, edited := edits[p]
		at := dir + "/" + p
		switch {
		case e.Add && !have:
			out = append(out, at+": recorded as added but missing")
		case e.Add:
			if sum(got.data) != e.SHA256 || got.exec != e.Exec {
				out = append(out, at+": differs from the recorded added file")
			}
		case !inOriginal:
			out = append(out, at+": is not in the substituted original and not recorded as an added file")
		case e.Remove:
			if have {
				out = append(out, at+": present although recorded as removed")
			}
		case !have:
			out = append(out, at+": missing from the staged copy and not recorded as removed")
		default:
			data := got.data
			if edited {
				lines, err := revert(splitLines(string(data)), e.Hunks)
				if err != nil {
					out = append(out, at+": "+err.Error())
					continue
				}
				data = []byte(strings.Join(lines, ""))
			}
			if sum(data) != want.Original {
				out = append(out, at+": differs from the substituted original and no recorded edit accounts for it")
			}
			if got.exec != want.Exec {
				out = append(out, at+": executable bit differs from the substituted original")
			}
		}
	}
	return out
}

// replay renders the originals again from the source and compares them with the record.
func replay(rec string, src Source, listing string, sub *substituter, skill *Skill) []string {
	if listing != skill.Origin.SkillsListing || src.Origin != skill.Origin {
		return []string{rec + ": source skills tree does not match the record origin"}
	}
	orig, err := sub.render(filepath.Join(src.skills(), skill.From))
	if err != nil {
		return []string{rec + ": " + err.Error()}
	}
	var out []string
	for p, f := range orig {
		if e, ok := skill.Files[p]; !ok || e.Original != sum(f.data) || e.Exec != f.exec {
			out = append(out, rec+": record is stale against the source: "+p)
		}
	}
	for p := range skill.Files {
		if _, ok := orig[p]; !ok {
			out = append(out, rec+": record is stale against the source: "+p)
		}
	}
	slices.Sort(out)
	return out
}
