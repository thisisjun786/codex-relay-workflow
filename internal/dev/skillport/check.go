//go:build dev

package skillport

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// entries is the names in root/dir (without suffix), ignoring hidden temp items.
func entries(root, dir, suffix string) map[string]bool {
	found := map[string]bool{}
	list, _ := os.ReadDir(filepath.Join(root, dir))
	for _, e := range list {
		if name, ok := strings.CutSuffix(e.Name(), suffix); ok && !strings.HasPrefix(e.Name(), ".") {
			found[name] = true
		}
	}
	return found
}

// Check verifies every staged skill against its record and returns how many it looked at and the
// problems found, each as "<path>: <what is wrong>". Nothing staged and no record is silent. With a
// source it also checks that tree against the record origin and renders the originals again.
func Check(root string, src *Source) (int, []string) {
	var problems []string
	report := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	recorded, staged := entries(root, RecordDir, ".json"), entries(root, StagingRoot, "")
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

// compare is the differences between a staged tree and the substituted originals the record holds.
func compare(dir string, skill *Skill, tree map[string]file) []string {
	names := map[string]bool{}
	for p := range tree {
		names[p] = true
	}
	for p := range skill.Files {
		names[p] = true
	}
	var out []string
	for _, p := range slices.Sorted(maps.Keys(names)) {
		got, have := tree[p]
		want, inOriginal := skill.Files[p]
		switch at := dir + "/" + p; {
		case !inOriginal:
			out = append(out, at+": is not in the substituted original")
		case !have:
			out = append(out, at+": missing from the staged copy")
		case sum(got.data) != want.Original:
			out = append(out, at+": differs from the substituted original")
		case got.exec != want.Exec:
			out = append(out, at+": executable bit differs from the substituted original")
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
