//go:build dev

package cxccorpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

var statuses = []string{"recorded", "extracted", "pending", "not-recordable"}

// FixtureCovers maps each fixture ID to its covers, from the fixture files.
func FixtureCovers(root string) (map[string][]string, error) {
	paths, err := filepath.Glob(filepath.Join(root, FixtureDir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, path := range paths {
		f, err := LoadFixture(path)
		if err != nil {
			return nil, err
		}
		out[strings.TrimSuffix(filepath.Base(path), ".json")] = f.Covers
	}
	return out, nil
}

// SyncCoverage fills each recorded item's fixture list from the fixtures' covers.
func SyncCoverage(cov *CoverageFile, covers map[string][]string) {
	by := map[string][]string{}
	for id, items := range covers {
		for _, item := range items {
			by[item] = append(by[item], id)
		}
	}
	for i := range cov.Items {
		list := by[cov.Items[i].ID]
		sort.Strings(list)
		cov.Items[i].Fixtures = list
	}
}

// Lint checks the corpus without an oracle: specs, fixtures, declarations, rules, the
// substitution table and the coverage index agree. It returns one line per problem.
func Lint(root string) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if _, err := LoadRules(root); err != nil {
		add("%s: %v", Normalise, err)
	}
	if _, err := LoadSubstitution(root); err != nil {
		add("%s: %v", Substitution, err)
	}
	decls, byLeg, err := LoadDeclarations(root)
	if err != nil {
		add("%s: %v", Declarations, err)
	}
	if err == nil && len(decls.Legs) != 32 {
		add("%s: %d legs, the v0.2.40 manifest registers 32", Declarations, len(decls.Legs))
	}
	specs, err := LoadSpecs(root)
	if err != nil {
		add("%s: %v", SpecDir, err)
	}
	specByID := map[string]Scenario{}
	for _, spec := range specs {
		for _, s := range spec.Scenarios {
			specByID[s.ID] = s
			if err := validID(s.ID); err != nil {
				add("scenario %q: id %v", s.ID, err)
			}
			if len(s.Covers) == 0 {
				add("scenario %s covers nothing", s.ID)
			}
			if len(s.Steps) == 0 {
				add("scenario %s has no steps", s.ID)
			}
			for i, step := range s.Steps {
				if step.Hook != "" && byLeg != nil {
					if _, ok := byLeg[step.Hook]; !ok {
						add("scenario %s step %d: hook leg %q is not declared", s.ID, i, step.Hook)
					}
				}
			}
		}
	}
	covers, err := FixtureCovers(root)
	if err != nil {
		add("%s: %v", FixtureDir, err)
	}
	for id := range covers {
		spec, ok := specByID[id]
		if !ok {
			add("fixture %s has no spec in %s", id, SpecDir)
			continue
		}
		f, err := LoadFixture(filepath.Join(root, FixtureDir, id+".json"))
		if err != nil {
			add("%v", err)
			continue
		}
		if f.Run.Kind != RunKind {
			add("fixture %s: run.kind %q, want %q", id, f.Run.Kind, RunKind)
		}
		if f.Oracle != OracleTag+" "+OracleCommit {
			add("fixture %s: oracle %q is not %s %s", id, f.Oracle, OracleTag, OracleCommit)
		}
		if !sameJSON(f.Given, spec.Given) || !sameJSON(f.Run.Steps, spec.Steps) || !slices.Equal(f.Covers, spec.Covers) || !slices.Equal(f.Run.Observe, spec.Observe) {
			add("fixture %s does not hold its spec's given, steps, observe and covers: record it again", id)
		}
		if len(f.Expect.Steps) != len(spec.Steps) {
			add("fixture %s: %d step results for %d steps", id, len(f.Expect.Steps), len(spec.Steps))
		}
		raw, _ := os.ReadFile(filepath.Join(root, FixtureDir, id+".json"))
		if canonical, err := Marshal(f); err == nil && !bytes.Equal(canonical, raw) {
			add("fixture %s is not in the corpus's JSON spelling: record it again", id)
		}
	}
	for id := range specByID {
		if _, ok := covers[id]; !ok {
			add("scenario %s has no recorded fixture in %s", id, FixtureDir)
		}
	}
	cov, err := LoadCoverage(root)
	if err != nil {
		add("%s: %v", Coverage, err)
		return problems
	}
	for _, id := range cov.PathLengthDependent {
		if _, ok := covers[id]; !ok {
			add("path_length_dependent names %s, which is not a fixture", id)
		}
	}
	items := map[string]CoverageItem{}
	for _, item := range cov.Items {
		if _, dup := items[item.ID]; dup {
			add("coverage item %s twice", item.ID)
		}
		items[item.ID] = item
		if !slices.Contains(statuses, item.Status) {
			add("coverage item %s: status %q is not one of %v", item.ID, item.Status, statuses)
		}
		if _, ok := cov.Statuses[item.Status]; !ok {
			add("coverage item %s: status %q is not described in statuses", item.ID, item.Status)
		}
		switch item.Status {
		case "recorded":
			if len(item.Fixtures) == 0 {
				add("coverage item %s is recorded but no fixture covers it", item.ID)
			}
		case "extracted":
			if item.Evidence == "" {
				add("coverage item %s is extracted but names no evidence file", item.ID)
			} else if _, err := os.Stat(filepath.Join(root, item.Evidence)); err != nil {
				add("coverage item %s: evidence %v", item.ID, err)
			}
		case "pending":
			if len(item.Spec) == 0 && item.Method == "" {
				add("coverage item %s is pending without the exact spec (or extraction method) to record it", item.ID)
			}
			for _, s := range item.Spec {
				if err := validID(s.ID); err != nil {
					add("coverage item %s: pending spec id %q %v", item.ID, s.ID, err)
				}
				if len(s.Steps) == 0 {
					add("coverage item %s: pending spec %s has no steps", item.ID, s.ID)
				}
				for _, step := range s.Steps {
					if step.Hook != "" && byLeg != nil {
						if _, ok := byLeg[step.Hook]; !ok {
							add("coverage item %s: pending spec %s names undeclared leg %q", item.ID, s.ID, step.Hook)
						}
					}
				}
			}
		case "not-recordable":
			if item.Reason == "" {
				add("coverage item %s is not recordable without a reason", item.ID)
			}
		}
		if item.Status != "recorded" && len(item.Fixtures) > 0 {
			add("coverage item %s is %s but fixtures %v cover it", item.ID, item.Status, item.Fixtures)
		}
	}
	synced := cov
	synced.Items = slices.Clone(cov.Items)
	SyncCoverage(&synced, covers)
	for i := range synced.Items {
		if !slices.Equal(synced.Items[i].Fixtures, cov.Items[i].Fixtures) {
			add("coverage item %s lists fixtures %v, the fixtures' covers say %v: record again or sync", cov.Items[i].ID, cov.Items[i].Fixtures, synced.Items[i].Fixtures)
		}
	}
	for id, list := range covers {
		for _, item := range list {
			if _, ok := items[item]; !ok {
				add("fixture %s covers %q, which is not a coverage item", id, item)
			}
		}
	}
	// Every registered leg is a coverage item of its own.
	for leg := range byLeg {
		if _, ok := items["K3/"+leg]; !ok {
			add("hook leg %s has no coverage item K3/%s", leg, leg)
		}
	}
	sort.Strings(problems)
	return problems
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// validID is the fixture filename rule: lowercase words joined by - _ and the __ separator.
func validID(id string) error {
	if id == "" || len(id) > 160 {
		return errors.New("empty or longer than 160 bytes")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return fmt.Errorf("character %q is outside [a-z0-9._-]", r)
		}
	}
	if !strings.Contains(id, "__") {
		return errors.New("has no __ separating its surface from its case")
	}
	return nil
}
