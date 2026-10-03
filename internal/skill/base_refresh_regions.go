package skill

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The edit regions a node declared (dag-region-declare), read for the mechanical-resolution check
// (CRW-412). The check asks one thing of them: which rule settles the conflict on a path. The answer is
// a rule only when every declaration given agrees on it and nothing weakens it, because a conflict is
// settled by a rule only where the scheduler would have judged the overlap mechanical.

const maxRegionBytes = 1 << 20

// wireRegion is one region of the declaration document, as dag-region-declare reads it.
type wireRegion struct {
	Repository string "json:\"repository\""
	Path       string "json:\"path\""
	Kind       string "json:\"kind\""
	Key        string "json:\"key\""
	Change     string "json:\"change\""
	Exclusive  bool   "json:\"exclusive\""
	Grade      string "json:\"grade\""
	Rule       string "json:\"rule\""
}

// regionSet is the regions of one declaration that name the repository, folded like a declaration is
// (a rename, a delete and a hotspot are exclusive whatever grade was written).
type regionSet struct{ regions []dagsched.Region }

// coverage is the declarations given to one check: one set for each node that has a say in the place.
type coverage []regionSet

func regionControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// mechanicalRule is whether a rule is one a mechanical region may name: union, renumber, or
// regenerate: followed by a command.
func mechanicalRule(rule string) bool {
	if len(rule) > dagsched.MaxRuleBytes {
		return false
	}
	if rule == dagsched.RuleUnion || rule == dagsched.RuleRenumber {
		return true
	}
	command, ok := strings.CutPrefix(rule, dagsched.RuleRegeneratePref)
	return ok && command != "" && command == strings.TrimSpace(command) && !regionControl(command)
}

func normalizeRegion(w wireRegion) (dagsched.Region, error) {
	if w.Path != strings.TrimSpace(w.Path) || regionControl(w.Path) {
		return dagsched.Region{}, fmt.Errorf("the path %q has whitespace or a control character at its ends or inside it", w.Path)
	}
	clean := path.Clean(w.Path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") {
		return dagsched.Region{}, fmt.Errorf("the path %q is not a clean path inside the repository", w.Path)
	}
	change := w.Change
	if change == "" {
		change = "edit"
	}
	switch {
	case w.Kind != "tree" && w.Kind != "file" && w.Kind != "symbol":
		return dagsched.Region{}, fmt.Errorf("the kind %q is tree, file or symbol", w.Kind)
	case change != "edit" && change != "rename" && change != "delete":
		return dagsched.Region{}, fmt.Errorf("the change %q is edit, rename or delete", change)
	case w.Kind == "symbol" && w.Key == "":
		return dagsched.Region{}, fmt.Errorf("a symbol region names its symbol")
	case w.Kind != "symbol" && w.Key != "":
		return dagsched.Region{}, fmt.Errorf("only a symbol region has a key")
	}
	exclusive, _ := dagsched.Classify(clean, change)
	r := dagsched.Region{Repository: w.Repository, Path: clean, Kind: w.Kind, Key: w.Key, Change: change, Exclusive: w.Exclusive || exclusive, Grade: w.Grade, Rule: w.Rule}
	switch w.Grade {
	case "", dagsched.GradeIndependent, dagsched.GradeLocal, dagsched.GradeExclusive:
		if w.Rule != "" {
			return dagsched.Region{}, fmt.Errorf("the rule %q is on a region graded %q: only a mechanical region names a rule", w.Rule, w.Grade)
		}
	case dagsched.GradeMechanical:
		if !mechanicalRule(w.Rule) {
			return dagsched.Region{}, fmt.Errorf("a mechanical region names its rule: %s, %s or %s<command>, not %q", dagsched.RuleUnion, dagsched.RuleRenumber, dagsched.RuleRegeneratePref, w.Rule)
		}
	default:
		return dagsched.Region{}, fmt.Errorf("the grade %q is %s, %s, %s or %s", w.Grade, dagsched.GradeIndependent, dagsched.GradeMechanical, dagsched.GradeLocal, dagsched.GradeExclusive)
	}
	return r, nil
}

func readRegionSet(file, repository string) (regionSet, error) {
	f, err := os.Open(file)
	if err != nil {
		return regionSet{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRegionBytes+1))
	if err != nil {
		return regionSet{}, err
	}
	if len(data) > maxRegionBytes {
		return regionSet{}, fmt.Errorf("the declaration is longer than %d bytes", maxRegionBytes)
	}
	var wire []wireRegion
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return regionSet{}, fmt.Errorf("the regions are a JSON list of {repository, path, kind, key, change, exclusive, grade, rule}: %w", err)
	}
	if decoder.More() {
		return regionSet{}, fmt.Errorf("the regions are one JSON list")
	}
	var set regionSet
	for i, w := range wire {
		if w.Repository != repository {
			continue // another repository's region says nothing about a place in this one
		}
		r, err := normalizeRegion(w)
		if err != nil {
			return regionSet{}, fmt.Errorf("region %d: %w", i+1, err)
		}
		set.regions = append(set.regions, r)
	}
	return set, nil
}

// readRegionSets reads the declarations a check is given.
func readRegionSets(files []string, repository string) (coverage, error) {
	sets := make(coverage, 0, len(files))
	for _, file := range files {
		set, err := readRegionSet(file, repository)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		sets = append(sets, set)
	}
	return sets, nil
}

// touching are the regions that say something about a path: a file or symbol region on it, and a tree
// region above it.
func (s regionSet) touching(p string) []dagsched.Region {
	var out []dagsched.Region
	for _, r := range s.regions {
		if (r.Kind == "tree" && within(p, r.Path)) || (r.Kind != "tree" && r.Path == p) {
			out = append(out, r)
		}
	}
	return out
}

// ruleFor is the rule that settles a conflict on a path, and whether there is one. There is one only
// when, in every declaration given, a region touches the path and every region that touches it is
// mechanical and names that rule: a region of any other grade (an independent claim the overlap
// contradicts, a local or exclusive one), a symbol (a part of a file is not a file), a disagreement
// between regions or between declarations, and no region at all leave the path uncovered. A shared
// contract surface is never covered, and between the regions of two declarations the scheduler's own
// judgement of the overlap has to be mechanical, which it is not for two trees that hold a shared
// surface. A single declaration is taken as it stands: it cannot show what the other node declared.
func (c coverage) ruleFor(p string) (string, bool) {
	if len(c) == 0 || dagsched.SharedSurface(p) {
		return "", false
	}
	rule := ""
	perSet := make([][]dagsched.Region, 0, len(c))
	for _, set := range c {
		touching := set.touching(p)
		if len(touching) == 0 {
			return "", false
		}
		for _, r := range touching {
			if r.Kind == "symbol" || dagsched.EffectiveGrade(r) != dagsched.GradeMechanical {
				return "", false
			}
			if rule == "" {
				rule = r.Rule
			} else if r.Rule != rule {
				return "", false
			}
		}
		perSet = append(perSet, touching)
	}
	for i := range perSet {
		for j := i + 1; j < len(perSet); j++ {
			for _, a := range perSet[i] {
				for _, b := range perSet[j] {
					if dagsched.PairGrade(a, b) != dagsched.GradeMechanical {
						return "", false
					}
				}
			}
		}
	}
	return rule, rule != ""
}

// regenerateCommands are the commands of every regenerate rule the declarations name, sorted: the
// most runs a check can make is two of each.
func (c coverage) regenerateCommands() []string {
	seen := map[string]bool{}
	for _, set := range c {
		for _, r := range set.regions {
			if command, ok := strings.CutPrefix(r.Rule, dagsched.RuleRegeneratePref); ok && dagsched.EffectiveGrade(r) == dagsched.GradeMechanical {
				seen[command] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for command := range seen {
		out = append(out, command)
	}
	sort.Strings(out)
	return out
}
