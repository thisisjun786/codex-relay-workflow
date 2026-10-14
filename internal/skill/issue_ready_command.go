package skill

import (
	"fmt"
	"regexp"
	"strings"
)

// The end condition's commands as the release gate reads them (CRW-1328): statically, never run. go test passes having run no test
// when the package does not build without a tag its files require (every file under internal/dev and cmd/crw-dev starts with
// //go:build dev) or when -run matches no test, so either is a warning of the report. A warning never holds an issue: the decision,
// its reasons and the exit code are the gate's as before. The one real run of the command (rc 0 and at least one test run) is the
// design-stage explorer's step (crw-add-issue), warned and recorded, never this check's.

// devPackage is a package path whose files build only with -tags dev.
var devPackage = regexp.MustCompile(`^(\./)?(internal/dev|cmd/crw-dev)(/|$)`)

// goIdentifier is a name a -run pattern may name literally: a test, a benchmark, a subtest.
var goIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// commandLines are the commands a section states: its backticked spans that can be commands and the lines of its fenced blocks that
// are not output lines.
func (s section) commandLines() []string {
	var out []string
	for i, line := range s.lines {
		if s.fenced[i] {
			if mark, _, _ := fenceRun(line); mark != 0 {
				continue
			}
			if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$ ")); t != "" && !outputLine.MatchString(strings.ToLower(t)) {
				out = append(out, t)
			}
			continue
		}
		for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
			if commandText(m[1]) {
				out = append(out, strings.TrimSpace(m[1]))
			}
		}
	}
	return out
}

// shellWords splits a command line the way a shell would for the reading here: words separated by blanks, quotes kept together and
// removed.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	quote, in := rune(0), false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// fixedRunPattern says whether a -run pattern names its tests literally: names joined by |, at each level of a subtest path, with
// an optional ^ and $ and parentheses around the alternatives. A placeholder, a shell variable or a regular expression wildcard is
// not one.
func fixedRunPattern(p string) bool {
	if p == "" {
		return false
	}
	for _, level := range strings.Split(p, "/") {
		level = strings.TrimSuffix(strings.TrimPrefix(level, "^"), "$")
		if strings.HasPrefix(level, "(") && strings.HasSuffix(level, ")") {
			level = level[1 : len(level)-1]
		}
		for _, alt := range strings.Split(level, "|") {
			if !goIdentifier.MatchString(strings.TrimSuffix(strings.TrimPrefix(alt, "^"), "$")) {
				return false
			}
		}
	}
	return true
}

// goTestWarnings are the warnings of one command: none when it is no go test.
func goTestWarnings(command string) []string {
	words := shellWords(command)
	start := -1
	for i := 0; i+1 < len(words); i++ {
		if (words[i] == "go" || strings.HasSuffix(words[i], "/go")) && words[i+1] == "test" {
			start = i + 2
			break
		}
	}
	if start < 0 {
		return nil
	}
	var tags []string
	var packages []string
	run, hasRun := "", false
	for i := start; i < len(words); i++ {
		w := words[i]
		if w == "&&" || w == "||" || w == "|" || w == ";" || strings.HasPrefix(w, ">") || strings.HasPrefix(w, "2>") {
			break
		}
		if !strings.HasPrefix(w, "-") {
			packages = append(packages, w)
			continue
		}
		name, value, given := strings.Cut(strings.TrimPrefix(w, "-"), "=")
		name = strings.TrimPrefix(name, "-")
		if name != "tags" && name != "run" {
			continue
		}
		if !given && i+1 < len(words) {
			i++
			value = words[i]
		}
		if name == "tags" {
			tags = append(tags, strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' })...)
		} else {
			run, hasRun = value, true
		}
	}
	var out []string
	tagged := false
	for _, t := range tags {
		if t == "dev" {
			tagged = true
		}
	}
	if !tagged {
		var dev []string
		for _, p := range packages {
			if devPackage.MatchString(p) {
				dev = append(dev, p)
			}
		}
		if len(dev) > 0 {
			out = append(out, fmt.Sprintf("end condition: `%s` tests %s without -tags dev: every file under internal/dev and cmd/crw-dev starts with //go:build dev, so without the tag the package does not build or tests nothing; add -tags dev", command, strings.Join(dev, ", ")))
		}
	}
	if hasRun && !fixedRunPattern(run) {
		out = append(out, fmt.Sprintf("end condition: `%s` has -run %q, which does not name its tests literally: go test exits 0 when -run matches no test, so the command can pass having run none; name the tests (for example -run '^(TestA|TestB)$')", command, run))
	}
	return out
}

// endConditionWarnings are the warnings of every go test command of the done condition sections.
func endConditionWarnings(sections []section) []string {
	out := []string{}
	for _, s := range sections {
		if s.kind != itemDone {
			continue
		}
		for _, c := range s.commandLines() {
			out = append(out, goTestWarnings(c)...)
		}
	}
	return out
}
