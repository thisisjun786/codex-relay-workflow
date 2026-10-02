// The committed schema files of the corpus that a replay reads: the rename table, the coverage
// index, the hook declarations. They carry no build tag, like the rest of the engine.
package cxccorpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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

// LoadCoverage reads the coverage index.
func LoadCoverage(root string) (CoverageFile, error) {
	var file CoverageFile
	err := readStrict(filepath.Join(root, Coverage), &file)
	return file, err
}

// DeclarationFile is contract/schema/cxc/hook-declarations.json (contract K1): every hook
// registration plugin.json lists, in manifest order, with its command split into the entry file
// and arguments the recorder runs.
type DeclarationFile struct {
	Description string        `json:"description"`
	Oracle      string        `json:"oracle"`
	Legs        []Declaration `json:"legs"`
}

// Declaration is one registered hook leg (hook-declarations.json).
type Declaration struct {
	Leg     string   `json:"leg"`
	File    string   `json:"file"`
	Event   string   `json:"event"`
	Matcher string   `json:"matcher,omitempty"`
	Command string   `json:"command"`
	Entry   string   `json:"entry"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
	Status  string   `json:"statusMessage"`
}

// LoadDeclarations reads the committed declarations, keyed by leg.
func LoadDeclarations(root string) (DeclarationFile, map[string]Declaration, error) {
	var file DeclarationFile
	if err := readStrict(filepath.Join(root, Declarations), &file); err != nil {
		return file, nil, err
	}
	byLeg := map[string]Declaration{}
	for _, d := range file.Legs {
		if _, dup := byLeg[d.Leg]; dup {
			return file, nil, fmt.Errorf("%s: leg %q twice", Declarations, d.Leg)
		}
		byLeg[d.Leg] = d
	}
	return file, byLeg, nil
}

// Kebab is an event name in kebab case: SessionStart becomes session-start.
func Kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
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

// Substituter is the compiled regex rules, in order, and the cli table's verbs.
type Substituter struct {
	File     SubstitutionFile
	rules    []*regexp.Regexp
	verbs    *regexp.Regexp
	verbRows map[string]CLIRename
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
	return s, s.compileVerbs()
}

// compileVerbs builds the verb pass of Expected: every row's cxc words, longest first, after
// {CRW} or a bare cxc and before a word boundary.
func (s *Substituter) compileVerbs() error {
	rows := slices.Clone(s.File.CLI)
	slices.SortStableFunc(rows, func(a, b CLIRename) int { return len(b.CXC) - len(a.CXC) })
	s.verbRows = map[string]CLIRename{}
	var verbs []string
	for _, row := range rows {
		verb := strings.Join(row.CXC, " ")
		s.verbRows[verb] = row
		verbs = append(verbs, regexp.QuoteMeta(verb))
	}
	re, err := regexp.Compile(`(\{CRW\}|\bcxc) (` + strings.Join(verbs, "|") + `)\b`)
	s.verbs = re
	return err
}

// Apply runs the regex and resolver rules over an expected text in order; rewrite rules are
// skipped (the replayer handles them as their notes say).
func (s *Substituter) Apply(text string) string { return s.run(text, -1) }

// run applies the rules in order and, after rule number verbsAfter, the cli verb pass.
func (s *Substituter) run(text string, verbsAfter int) string {
	for i, re := range s.rules {
		if re != nil {
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
		if i == verbsAfter {
			text = s.verbPass(text)
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

// Expected is the expected side of a replay: Apply, then the cli table's verbs for the words that
// follow the oracle's resolved invocation and a bare cxc (the R27 and R33 notes: {CRW} orchestrate
// becomes {CRW} pabcd orchestrate). The verb pass runs right after the last resolver rule, while
// the text still says cxc, so a marker such as [codexclaw bg] (renamed [crw bg] by R23, never a
// command) is left alone.
func (s *Substituter) Expected(text string) string {
	last := -1
	for i, rule := range s.File.Rules {
		if rule.Kind == "resolver" {
			last = i
		}
	}
	text = s.run(text, last)
	if last < 0 {
		text = s.verbPass(text)
	}
	return text
}

// verbPass is one pass over the table's rows, longest first, so no row chains into another; a
// dropped row (a null crw) and an identical one leave the words as they are.
func (s *Substituter) verbPass(text string) string {
	return s.verbs.ReplaceAllStringFunc(text, func(match string) string {
		prefix, verb, _ := strings.Cut(match, " ")
		row := s.verbRows[verb]
		if row.CRW == nil {
			return match
		}
		return prefix + " " + strings.Join(row.CRW, " ")
	})
}

// EnvName is the crw name of an oracle environment variable and whether crw keeps it: the table
// renames some, drops one, and leaves a name it does not list (CODEX_HOME) as it is.
func (s *Substituter) EnvName(name string) (string, bool) {
	for _, row := range s.File.Env {
		if row.CXC == name {
			return row.CRW, row.CRW != ""
		}
	}
	return name, true
}

// Renamed is the normaliser a Go replay uses: every text and alias rule whose pattern or
// replacement names the oracle (the spawn grant store is codexclaw-subspawn-<uid>, and crw-subspawn-<uid>
// in crw) runs through the name substitution, so what a Go build prints normalises to what the
// oracle's output became after the same substitution. The other rules are kept as they are.
func (n *Normaliser) Renamed(sub *Substituter) (*Normaliser, error) {
	rename := func(s string) string {
		if lower := strings.ToLower(s); strings.Contains(lower, "codexclaw") || strings.Contains(lower, "cxc") {
			return sub.Apply(s)
		}
		return s
	}
	rules := n.Rules
	rules.Text = slices.Clone(rules.Text)
	for i, r := range rules.Text {
		rules.Text[i].Regex, rules.Text[i].Replace = rename(r.Regex), rename(r.Replace)
	}
	rules.Alias = slices.Clone(rules.Alias)
	for i, r := range rules.Alias {
		rules.Alias[i].Regex = rename(r.Regex)
	}
	return Compile(rules)
}

// StdoutBytes rebuilds the step's stdout from its form: a compact or two-space indented document
// with the final newline the form names, one compact document per line for jsonl, the text as is.
func (r StepResult) StdoutBytes() string {
	var text string
	if r.Stdout != nil {
		text = *r.Stdout
	}
	return rebuild(r.StdoutForm, r.StdoutJSON, r.StdoutJSONL, text)
}

// Content rebuilds a text file's content from its form; false for a binary or sidecar entry,
// which holds a digest or nothing.
func (e Entry) Content() (string, bool) {
	switch e.Form {
	case "binary", "sqlite-sidecar":
		return "", false
	}
	var text string
	if e.Text != nil {
		text = *e.Text
	}
	return rebuild(e.Form, e.JSON, e.JSONL, text), true
}

func rebuild(form string, doc json.RawMessage, lines []json.RawMessage, text string) string {
	compact := func(raw json.RawMessage) string {
		var buf bytes.Buffer
		if json.Compact(&buf, raw) != nil {
			return string(raw)
		}
		return buf.String()
	}
	switch form {
	case "text", "sqlite-text":
		return text
	case "json", "sqlite":
		return compact(doc)
	case "json-line":
		return compact(doc) + "\n"
	case "json-pretty-nonl":
		return string(doc)
	case "json-pretty":
		return string(doc) + "\n"
	case "jsonl":
		var out strings.Builder
		for _, line := range lines {
			out.WriteString(compact(line) + "\n")
		}
		return out.String()
	}
	return ""
}
