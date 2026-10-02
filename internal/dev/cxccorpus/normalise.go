//go:build dev

package cxccorpus

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Rules is contract/schema/cxc/normalisation.json: how a raw recording becomes the stored
// fixture, and how a Go replay's output must be brought to the same form before it is compared.
type Rules struct {
	Description  string        `json:"description"`
	Placeholders []Placeholder `json:"placeholders"`
	Text         []TextRule    `json:"text"`
	Alias        []AliasRule   `json:"alias"`
	DropStderr   []NamedRegex  `json:"drop_stderr_lines"`
}

// Placeholder names one absolute path the recorder binds per case root.
type Placeholder struct {
	Placeholder string `json:"placeholder"`
	Is          string `json:"is"`
}

// TextRule replaces every match of Regex with Replace (Go regexp template syntax, ${1}).
type TextRule struct {
	Name    string `json:"name"`
	Regex   string `json:"regex"`
	Replace string `json:"replace"`
	Why     string `json:"why"`
}

// AliasRule replaces each distinct match with <Alias_n>, numbered by first appearance across the
// whole scenario (steps in order, then the tree in path order), so equality across steps holds.
// With Group set, only that capture group is replaced and the rest of the match is kept (RE2 has
// no lookaround to say "a nonce after GRANT:").
type AliasRule struct {
	Name  string `json:"name"`
	Regex string `json:"regex"`
	Group int    `json:"group,omitempty"`
	Alias string `json:"alias"`
	Why   string `json:"why"`
}

// NamedRegex is a line filter.
type NamedRegex struct {
	Name  string `json:"name"`
	Regex string `json:"regex"`
	Why   string `json:"why"`
}

// PlaceholderNames are the placeholders the recorder binds, in the order they are substituted
// (longest path first is guaranteed by the binding, not by this order).
var PlaceholderNames = []string{"${PLUGIN_ROOT}", "${CXC_ROOT}", "${WS}", "${CODEX_HOME}", "${CXC_HOME}", "${HOME}", "${TMP}", "${STUBS}", "${BIN}", "${REC}", "${ROOT}", "${NODE}"}

// LoadRules reads and compiles the normalisation rules.
func LoadRules(root string) (*Normaliser, error) {
	var rules Rules
	if err := readStrict(filepath.Join(root, Normalise), &rules); err != nil {
		return nil, err
	}
	return Compile(rules)
}

// Normaliser is the compiled rules.
type Normaliser struct {
	Rules  Rules
	text   []*regexp.Regexp
	alias  []*regexp.Regexp
	stderr []*regexp.Regexp
}

// Compile checks every regex is RE2 (Go) syntax.
func Compile(rules Rules) (*Normaliser, error) {
	n := &Normaliser{Rules: rules}
	for _, r := range rules.Text {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("text rule %s: %w", r.Name, err)
		}
		n.text = append(n.text, re)
	}
	for _, r := range rules.Alias {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("alias rule %s: %w", r.Name, err)
		}
		if r.Group < 0 || r.Group > re.NumSubexp() {
			return nil, fmt.Errorf("alias rule %s: group %d of %d", r.Name, r.Group, re.NumSubexp())
		}
		n.alias = append(n.alias, re)
	}
	for _, r := range rules.DropStderr {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("stderr rule %s: %w", r.Name, err)
		}
		n.stderr = append(n.stderr, re)
	}
	declared := map[string]bool{}
	for _, p := range rules.Placeholders {
		declared[p.Placeholder] = true
	}
	for _, name := range PlaceholderNames {
		if !declared[name] {
			return nil, fmt.Errorf("placeholder %s is bound by the recorder but not declared", name)
		}
		delete(declared, name)
	}
	for name := range declared {
		return nil, fmt.Errorf("placeholder %s is declared but the recorder binds no path to it", name)
	}
	return n, nil
}

// Binding is one placeholder's absolute path in one case.
type Binding struct {
	Placeholder, Path string
}

// Session normalises one scenario: the path bindings of its case root and the alias table that
// spans all of its text.
type Session struct {
	n        *Normaliser
	bindings []Binding
	aliases  []map[string]string
}

// NewSession binds the placeholders. Each path is also bound in its symlink-resolved spelling;
// longer paths are replaced first so a root never eats its child's placeholder.
func (n *Normaliser) NewSession(bindings []Binding) *Session {
	var all []Binding
	seen := map[string]bool{}
	for _, b := range bindings {
		for _, p := range []string{b.Path, realpath(b.Path)} {
			if p != "" && p != "/" && !seen[p] {
				seen[p] = true
				all = append(all, Binding{b.Placeholder, p})
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return len(all[i].Path) > len(all[j].Path) })
	s := &Session{n: n, bindings: all}
	for range n.alias {
		s.aliases = append(s.aliases, map[string]string{})
	}
	return s
}

func realpath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// Text normalises one text: paths, then the text rules, then the aliases.
func (s *Session) Text(text string) string {
	for _, b := range s.bindings {
		text = strings.ReplaceAll(text, b.Path, b.Placeholder)
	}
	for i, re := range s.n.text {
		text = re.ReplaceAllString(text, s.n.Rules.Text[i].Replace)
	}
	for i, re := range s.n.alias {
		table := s.aliases[i]
		rule := s.n.Rules.Alias[i]
		alias := func(match string) string {
			if name, ok := table[match]; ok {
				return name
			}
			name := fmt.Sprintf("<%s_%d>", rule.Alias, len(table)+1)
			table[match] = name
			return name
		}
		var out strings.Builder
		last := 0
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[2*rule.Group], m[2*rule.Group+1]
			if start < 0 {
				continue
			}
			out.WriteString(text[last:start])
			out.WriteString(alias(text[start:end]))
			last = end
		}
		out.WriteString(text[last:])
		text = out.String()
	}
	return text
}

// Stderr drops the Node runtime's own warning lines, then normalises the rest.
func (s *Session) Stderr(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.SplitAfter(text, "\n")
	var kept []string
	for _, line := range lines {
		bare := strings.TrimRight(line, "\n")
		drop := false
		for _, re := range s.n.stderr {
			if re.MatchString(bare) {
				drop = true
				break
			}
		}
		if !drop && line != "" {
			kept = append(kept, line)
		}
	}
	return s.Text(strings.Join(kept, ""))
}

// osGetenv is os.Getenv; a seam for the safety checks' tests.
var osGetenv = os.Getenv
