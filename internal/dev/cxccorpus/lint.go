//go:build dev

package cxccorpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// CoverageFile is contract/schema/cxc/coverage.json: every contract item of the Wave 0 analysis
// with how this corpus holds it.
type CoverageFile struct {
	Description string            `json:"description"`
	Statuses    map[string]string `json:"statuses"`
	// PathLengthDependent are the fixtures holding a byte count or a cut over an expanded
	// case-root or plugin path: a replay compares them only at the recording's path lengths.
	PathLengthDependent []string       `json:"path_length_dependent"`
	Items               []CoverageItem `json:"items"`
}

// CoverageItem is one contract item. Recorded items list the fixtures whose covers name them
// (kept in sync by `crw-dev cxc record`); extracted items name the data file that holds them;
// pending items carry the exact spec to record (scenarios in the specs' grammar, or for an item
// that is not a scenario the extraction method); not-recordable items say why.
type CoverageItem struct {
	ID       string     `json:"id"`
	Contract string     `json:"contract"`
	Source   string     `json:"source,omitempty"`
	Status   string     `json:"status"`
	Fixtures []string   `json:"fixtures,omitempty"`
	Evidence string     `json:"evidence,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	Method   string     `json:"method,omitempty"`
	Spec     []Scenario `json:"spec,omitempty"`
}

var statuses = []string{"recorded", "extracted", "pending", "not-recordable"}

// LoadCoverage reads the coverage index.
func LoadCoverage(root string) (CoverageFile, error) {
	var file CoverageFile
	err := readStrict(filepath.Join(root, Coverage), &file)
	return file, err
}

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

// SubstitutionFile is contract/schema/cxc/name-substitution.json: the ordered CXC -> CRW rename
// rules a Go replayer applies to a fixture's expected text (never to the fixture file itself).
type SubstitutionFile struct {
	Description string             `json:"description"`
	Source      string             `json:"source"`
	Apply       []string           `json:"apply"`
	Rules       []SubstitutionRule `json:"rules"`
	CLI         []CLIRename        `json:"cli"`
	Never       []NeverRule        `json:"never"`
	InputSide   []InputSideRule    `json:"input_side"`
	Env         []EnvRename        `json:"env"`
	Skills      []SkillRename      `json:"skills"`
}

// SubstitutionRule is one ordered rule. Kind "regex" is applied with Go regexp; a "resolver"
// rule is applied the same way to the expectation, and its note says what the replay does to its
// own output; a "rewrite" rule is not textual, and the replayer follows its note. Repeat applies a
// rule until the text stops changing.
type SubstitutionRule struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Regex   string `json:"regex,omitempty"`
	Replace string `json:"replace,omitempty"`
	Repeat  bool   `json:"repeat,omitempty"`
	Note    string `json:"note,omitempty"`
	Site    string `json:"site,omitempty"`
}

// CLIRename maps the argv prefix of a cxc step to the crw command; a null crw is a dropped verb.
type CLIRename struct {
	CXC  []string `json:"cxc"`
	CRW  []string `json:"crw"`
	Note string   `json:"note,omitempty"`
}

// NeverRule is text no rule may change.
type NeverRule struct {
	Text string `json:"text"`
	Why  string `json:"why"`
}

// InputSideRule is a recognizer whose rename changes what is accepted.
type InputSideRule struct {
	Site   string `json:"site"`
	Before string `json:"before"`
	After  string `json:"after"`
	Hazard string `json:"hazard,omitempty"`
}

// EnvRename is one environment variable.
type EnvRename struct {
	CXC  string `json:"cxc"`
	CRW  string `json:"crw"`
	Note string `json:"note,omitempty"`
}

// SkillRename is one skill name.
type SkillRename struct {
	CXC       string `json:"cxc"`
	CRW       string `json:"crw,omitempty"`
	Treatment string `json:"treatment"`
}

// Substituter is the compiled regex rules, in order.
type Substituter struct {
	File  SubstitutionFile
	rules []*regexp.Regexp
}

// LoadSubstitution reads and compiles the table: every regex rule must be RE2.
func LoadSubstitution(root string) (*Substituter, error) {
	var file SubstitutionFile
	if err := readStrict(filepath.Join(root, Substitution), &file); err != nil {
		return nil, err
	}
	s := &Substituter{File: file}
	seen := map[string]bool{}
	for _, r := range file.Rules {
		if seen[r.ID] {
			return nil, fmt.Errorf("rule %s twice", r.ID)
		}
		seen[r.ID] = true
		if r.Kind != "regex" && r.Kind != "resolver" && r.Kind != "rewrite" {
			return nil, fmt.Errorf("rule %s: kind %q", r.ID, r.Kind)
		}
		if r.Kind != "regex" && r.Note == "" {
			return nil, fmt.Errorf("rule %s: a %s rule needs a note", r.ID, r.Kind)
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", r.ID, err)
		}
		if r.Kind == "rewrite" {
			re = nil
		}
		s.rules = append(s.rules, re)
	}
	return s, nil
}

// Apply runs the regex and resolver rules over an expected text in order; rewrite rules are
// skipped (the replayer handles them as their notes say).
func (s *Substituter) Apply(text string) string {
	for i, re := range s.rules {
		if re == nil {
			continue
		}
		rule := s.File.Rules[i]
		for {
			next := re.ReplaceAllString(text, rule.Replace)
			if next == text || !rule.Repeat {
				text = next
				break
			}
			text = next
		}
	}
	return text
}

// MapArgv is the crw argv for a cxc step's argv: the longest matching cli prefix replaced, the
// rest kept. ok is false for a dropped verb or an argv no row covers.
func (s *Substituter) MapArgv(argv []string) (out []string, ok bool) {
	best := -1
	for i, row := range s.File.CLI {
		if len(row.CXC) <= len(argv) && slices.Equal(row.CXC, argv[:len(row.CXC)]) && (best < 0 || len(row.CXC) > len(s.File.CLI[best].CXC)) {
			best = i
		}
	}
	if best < 0 || s.File.CLI[best].CRW == nil {
		return nil, false
	}
	row := s.File.CLI[best]
	return append(slices.Clone(row.CRW), argv[len(row.CXC):]...), true
}
