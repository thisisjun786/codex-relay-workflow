//go:build dev

package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const rebrandHeading1523 = "## Task work names (CRW-1523)"

// The map is a proposal, not an executable alias registry. Unknown prose names
// warn; missing required coverage is an error in the proposal itself.
func rebrandMap1523(text string) (map[string]string, []string) {
	names := map[string]string{}
	var problems []string
	for _, table := range markdownTables(headingSection(text, rebrandHeading1523)) {
		if len(table) == 0 || len(table[0]) != 5 || table[0][0] != "Legacy name" {
			continue
		}
		for _, row := range table[1:] {
			if len(row) != 5 {
				problems = append(problems, "a name row needs all five columns")
				continue
			}
			key := strings.ToLower(strings.Trim(row[0], "`"))
			recommendation := strings.TrimSpace(strings.Trim(row[1], "`"))
			if key == "" || recommendation == "" || row[2] == "" || row[3] == "" ||
				!regexp.MustCompile(`^[0-9][0-9,]* / [0-9][0-9,]*$`).MatchString(row[4]) {
				problems = append(problems, "incomplete name row: "+row[0])
			}
			if _, duplicate := names[key]; duplicate {
				problems = append(problems, "duplicate legacy name: "+key)
			}
			names[key] = recommendation
		}
	}
	return names, problems
}

func requiredNames1523(registry string) []string {
	names := []string{
		"pabcd", "ipabcd", "pabcd-state", "crw-pabcd", "crw-loop",
		".crw/sessions", "ledger.jsonl", "i", "p", "a", "b", "c", "d", "idle",
		"crw pabcd",
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*\{Name: "([^"]+)"`).FindAllStringSubmatch(registry, -1) {
		names = append(names, "crw pabcd "+m[1])
	}
	return names
}

func missingNames1523(mapped map[string]string, required []string) []string {
	var missing []string
	for _, name := range required {
		if strings.TrimSpace(mapped[name]) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

func nameByte1523(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '_' || b == '-'
}

// A spelling inside a package path, filename, field or Go identifier is not a
// public label. A sentence-final period is still punctuation.
func publicBoundary1523(line string, start, end int) bool {
	if start > 0 {
		b := line[start-1]
		if nameByte1523(b) || b == '/' || b == '\\' || b == '.' {
			return false
		}
	}
	if end < len(line) {
		b := line[end]
		if nameByte1523(b) || b == '/' || b == '\\' ||
			b == '.' && end+1 < len(line) && nameByte1523(line[end+1]) {
			return false
		}
	}
	return true
}

// publicNames1523 scans visible Markdown labels and shell examples. It is
// deliberately an advisory spelling scanner, not a Markdown parser or a count
// of runtime behaviors. Line numbers survive the excluded source-code fences.
func publicNames1523(text string) map[string]int {
	found := map[string]int{}
	word := regexp.MustCompile(`(?i)(?:crw:)?(?:crw-)?(?:ipabcd|pabcd)(?:-[a-z0-9-]+)?|crw-loop`)
	command := regexp.MustCompile(`(?i)\bcrw[ \t]+pabcd(?:[ \t]+([a-z][a-z0-9-]*))?`)
	phase := regexp.MustCompile(`(?i)\b(?:phase|crw[ \t]+pabcd[ \t]+orchestrate)[ \t]+(IDLE|I|P|A|B|C|D)\b`)
	destination := regexp.MustCompile(`\]\([^)\n]*\)`)
	fence, source := "", false
	for i, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			if fence == "" {
				fence = trim[:3]
				lang := strings.ToLower(strings.TrimSpace(trim[3:]))
				source = slices.Contains([]string{"go", "js", "javascript", "ts", "typescript", "json", "python"}, lang)
			} else if strings.HasPrefix(trim, fence) {
				fence, source = "", false
			}
			continue
		}
		if source {
			continue
		}
		line = destination.ReplaceAllString(line, "]")
		add := func(name string) {
			name = strings.TrimPrefix(strings.ToLower(name), "crw:")
			if _, exists := found[name]; !exists {
				found[name] = i + 1
			}
		}
		for _, loc := range word.FindAllStringIndex(line, -1) {
			if publicBoundary1523(line, loc[0], loc[1]) {
				add(line[loc[0]:loc[1]])
			}
		}
		for _, m := range command.FindAllStringSubmatch(line, -1) {
			name := "crw pabcd"
			if m[1] != "" {
				name += " " + strings.ToLower(m[1])
			}
			add(name)
		}
		for _, m := range phase.FindAllStringSubmatch(line, -1) {
			add(m[1])
		}
		for _, path := range []string{".crw/sessions", "ledger.jsonl"} {
			if strings.Contains(line, path) {
				add(path)
			}
		}
	}
	return found
}

func TestCRW1523PublicNamesHaveRecommendations(t *testing.T) {
	mapped, problems := rebrandMap1523(readRepoText(t, namingFile))
	for _, problem := range problems {
		t.Error(problem)
	}
	registry := readRepoText(t, "internal/harness/pabcd.go")
	required := requiredNames1523(registry)
	if len(required) <= 15 {
		t.Fatal("the current command registry yielded no verbs")
	}
	for _, name := range missingNames1523(mapped, required) {
		t.Errorf("missing required name recommendation: %s", name)
	}
	cmd := exec.Command("git", "ls-files", "-z", "--", "*.md")
	cmd.Dir = repoRoot()
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	warnings := map[string]string{}
	for _, path := range strings.Split(string(output), "\x00") {
		if path == "" || path == namingFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoRoot(), path))
		if err != nil {
			t.Fatal(err)
		}
		for name, line := range publicNames1523(string(data)) {
			if mapped[name] == "" {
				if _, seen := warnings[name]; !seen {
					warnings[name] = fmt.Sprintf("%s:%d: unmapped public name %q", path, line, name)
				}
			}
		}
	}
	var names []string
	for name := range warnings {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Log("WARNING " + warnings[name])
	}
}

func TestCRW1523NameScanner(t *testing.T) {
	public := publicNames1523("Use PABCD. [crw-pabcd](internal/pabcd/source.go)\n" +
		"```sh\ncrw pabcd orchestrate I --session example\n```\n" +
		"phase D, .crw/sessions/example.json and .crw/ledger.jsonl; crw:crw-pabcd and crw-loop.")
	for _, want := range []string{"pabcd", "crw-pabcd", "crw pabcd orchestrate", "i", "d", ".crw/sessions", "ledger.jsonl", "crw-loop"} {
		if public[want] == 0 {
			t.Errorf("public spelling %q was lost: %v", want, public)
		}
	}
	identifiers := "internal/pabcd/source pabcd-state/src PABCDState pabcd_state obj.pabcd pabcd.go\n" +
		"[source](plugins/crw/skills/crw-pabcd/SKILL.md)\n```go\nvar pabcd = \"PABCD\"\n```\n" +
		"~~~json\n{\"name\":\"pabcd\"}\n~~~\n"
	if got := publicNames1523(identifiers); len(got) != 0 {
		t.Errorf("code identifiers or destinations became public names: %v", got)
	}
	found := publicNames1523("pabcd-widget and crw pabcd surprise")
	if found["pabcd-widget"] != 1 || found["crw pabcd surprise"] != 1 {
		t.Errorf("unknown names must remain visible to the warning check: %v", found)
	}
	if got := publicNames1523("I P A B C D are letters here."); len(got) != 0 {
		t.Errorf("bare letters were treated as phases: %v", got)
	}
}

func TestCRW1523MapValidation(t *testing.T) {
	header := rebrandHeading1523 + "\n\n| Legacy name | Canonical recommendation | Meaning / boundary | Other candidate (why not) | Uses (files / matches) |\n|---|---|---|---|---|\n"
	row := "| `PABCD` | `task work` | One task. | workflow: too broad. | 1 / 2 |\n"
	mapped, problems := rebrandMap1523(header + row)
	if len(problems) != 0 || len(missingNames1523(mapped, []string{"pabcd"})) != 0 {
		t.Fatalf("a mapped public name must pass: %v %v", mapped, problems)
	}
	_, problems = rebrandMap1523(header + row + row)
	if len(problems) == 0 {
		t.Error("a duplicate mapping was accepted")
	}
	_, problems = rebrandMap1523(header + strings.Replace(row, "`task work`", "", 1))
	if len(problems) == 0 {
		t.Error("an empty recommendation was accepted")
	}
	required := requiredNames1523("{Name: \"surprise\", Run: run},\n")
	if !slices.Contains(missingNames1523(mapped, required), "crw pabcd surprise") {
		t.Error("a newly registered command escaped required coverage")
	}
}
