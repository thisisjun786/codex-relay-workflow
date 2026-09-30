// Package argparse preserves the relay's CPython 3.13 argument and help contract.
// specs.json was generated from the Python relay's build_parser (generate_spec.py), without
// width-dependent rendered text; since todo 44 removed the Python relay it is the frozen source.
package argparse

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

//go:embed specs.json
var data []byte

type Action struct {
	Flags                    []string
	Dest, Kind, Type, Header string
	Required, Suppressed     bool
	Choices                  []string
	Help                     []string
	Children                 []Action
	// Check is a type= callable other than int or float, named by Type: false is the ValueError
	// argparse reports as "invalid <Type> value". Only a parser declared in Go carries one.
	Check func(string) bool `json:"-"`
}
type Section struct {
	Title   string
	Actions []int
}
type Group struct {
	Required bool
	Actions  []int
}
type Spec struct {
	Parts               []string
	Split               int
	Description, Epilog []string
	Actions             []Action
	Sections            []Section
	Groups              []Group
}

var Specs = func() map[string]Spec {
	var specs map[string]Spec
	if err := json.Unmarshal(data, &specs); err != nil {
		panic(err)
	}
	return specs
}()

func width() int {
	columns, _ := strconv.Atoi(os.Getenv("COLUMNS"))
	if columns <= 0 {
		columns = 80
		if size, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && size.Col > 0 {
			columns = int(size.Col)
		}
	}
	return columns - 2
}
func length(s string) int { return utf8.RuneCountInString(s) }
func program(prog, command string) string {
	if command != "" {
		return prog + " " + command
	}
	return prog
}

// Usage wraps complete action parts, including mutually exclusive separators,
// exactly as HelpFormatter._format_usage (not textwrap) does.
func Usage(prog, command string) string {
	spec := Specs[command]
	prog = program(prog, command)
	prefix := "usage: "
	textWidth := width()
	usage := strings.Join(append([]string{prog}, spec.Parts...), " ")
	if length(prefix+usage) <= textWidth {
		return prefix + usage
	}
	linesFor := func(parts []string, indent, first string) []string {
		var lines, line []string
		lineLen := length(indent) - 1
		if first != "" {
			lineLen = length(first) - 1
		}
		for _, part := range parts {
			if lineLen+1+length(part) > textWidth && len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
				line = nil
				lineLen = length(indent) - 1
			}
			line = append(line, part)
			lineLen += length(part) + 1
		}
		if len(line) > 0 {
			lines = append(lines, indent+strings.Join(line, " "))
		}
		if first != "" && len(lines) > 0 {
			lines[0] = strings.TrimPrefix(lines[0], indent)
		}
		return lines
	}
	opts, pos := spec.Parts[:spec.Split], spec.Parts[spec.Split:]
	var lines []string
	if float64(length(prefix+prog)) <= 0.75*float64(textWidth) {
		indent := strings.Repeat(" ", length(prefix+prog)+1)
		if len(opts) > 0 {
			lines = linesFor(append([]string{prog}, opts...), indent, prefix)
			lines = append(lines, linesFor(pos, indent, "")...)
		} else {
			lines = linesFor(append([]string{prog}, pos...), indent, prefix)
		}
	} else {
		indent := strings.Repeat(" ", length(prefix))
		lines = linesFor(spec.Parts, indent, "")
		if len(lines) > 1 {
			lines = linesFor(opts, indent, "")
			lines = append(lines, linesFor(pos, indent, "")...)
		}
		lines = append([]string{prog}, lines...)
	}
	return prefix + strings.Join(lines, "\n")
}

// wrap uses textwrap's pre-tokenized chunks. Tokenization is independent of width
// and is recorded by the spec generator, including Python's hyphen boundaries.
func wrap(chunks []string, width int) []string {
	chunks = slices.Clone(chunks)
	var lines []string
	for len(chunks) > 0 {
		if chunks[0] == " " && len(lines) > 0 {
			chunks = chunks[1:]
			if len(chunks) == 0 {
				break
			}
		}
		var line []string
		used := 0
		for len(chunks) > 0 && used+length(chunks[0]) <= width {
			line = append(line, chunks[0])
			used += length(chunks[0])
			chunks = chunks[1:]
		}
		if len(chunks) > 0 && length(chunks[0]) > width {
			space := max(1, width-used)
			if used < width {
				word := []rune(chunks[0])
				end := min(space, len(word))
				if len(word) > space {
					hyphen := strings.LastIndex(string(word[:end]), "-")
					if hyphen > 0 && strings.Trim(string(word[:hyphen]), "-") != "" {
						end = length(string(word[:hyphen+1]))
					}
				}
				line = append(line, string(word[:end]))
				chunks[0] = string(word[end:])
				if chunks[0] == "" {
					chunks = chunks[1:]
				}
			}
		}
		text := strings.TrimRight(strings.Join(line, ""), " ")
		if text != "" {
			lines = append(lines, text)
		}
	}
	return lines
}

func Help(prog, command string) string {
	spec := Specs[command]
	w := width()
	var b strings.Builder
	b.WriteString(Usage(prog, command) + "\n\n")
	if len(spec.Description) > 0 {
		b.WriteString(strings.Join(wrap(spec.Description, w), "\n") + "\n\n")
	}
	actionMax := 0
	for _, a := range spec.Actions {
		if !a.Suppressed {
			actionMax = max(actionMax, length(a.Header)+2)
			for _, child := range a.Children {
				actionMax = max(actionMax, length(child.Header)+2)
			}
		}
	}
	helpPos := min(actionMax+2, min(24, max(w-20, 4)))
	helpWidth := max(w-helpPos, 11)
	var renderAction func(*strings.Builder, Action, int)
	renderAction = func(body *strings.Builder, a Action, currentIndent int) {
		if a.Suppressed {
			return
		}
		actionWidth := helpPos - currentIndent - 2
		body.WriteString(strings.Repeat(" ", currentIndent) + a.Header)
		if len(a.Help) == 0 {
			body.WriteByte('\n')
		} else {
			firstIndent := 0
			if length(a.Header) <= actionWidth {
				body.WriteString(strings.Repeat(" ", actionWidth-length(a.Header)+2))
			} else {
				body.WriteByte('\n')
				firstIndent = helpPos
			}
			for i, line := range wrap(a.Help, helpWidth) {
				indent := helpPos
				if i == 0 {
					indent = firstIndent
				}
				body.WriteString(strings.Repeat(" ", indent) + line + "\n")
			}
		}
		for _, child := range a.Children {
			renderAction(body, child, currentIndent+2)
		}
	}
	for _, section := range spec.Sections {
		var body strings.Builder
		for _, index := range section.Actions {
			renderAction(&body, spec.Actions[index], 2)
		}
		if body.Len() > 0 {
			b.WriteString(section.Title + ":\n" + body.String() + "\n")
		}
	}
	if len(spec.Epilog) > 0 {
		b.WriteString(strings.Join(wrap(spec.Epilog, w), "\n") + "\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

var negativeNumber = regexp.MustCompile(`^-(?:\p{Nd}+|\p{Nd}*\.\p{Nd}+)$`)

// Optional is argparse's option-looking-token rule for parsers without negative
// number option names (all relay parsers). A single dash is an ordinary value.
func Optional(value string) bool {
	return len(value) > 1 && strings.HasPrefix(value, "-") && !negativeNumber.MatchString(strings.TrimSuffix(value, "\n")) && !strings.Contains(value, " ")
}

type Result struct {
	Values       map[string][]string
	Numbers      map[string]any // *big.Int or float64, converted by the declared action type
	Given        map[string]bool
	Message      string
	Global, Help bool
	// Remaining starts with the selected subcommand. Unknown root options are
	// reported after the child parses, so its help and required errors win.
	Remaining, Unknown []string
}

// Parse classifies tokens before consuming actions. Like CPython 3.13, ambiguity
// is reported when the action is consumed; an earlier help action still exits.
func Parse(command string, argv []string) Result { return ParseSpec(Specs[command], argv) }

// ParseSpec is Parse for a parser declared in Go rather than generated from build_parser. Its
// first action must be -h/--help, which a -h cluster resolves to.
func ParseSpec(spec Spec, argv []string) Result {
	r := Result{Values: map[string][]string{}, Numbers: map[string]any{}, Given: map[string]bool{}}
	type token struct {
		action                int
		value, problem        string
		inline, optional, end bool
	}
	tokens := make([]token, len(argv))
	ended := false
	for i, arg := range argv {
		t := token{action: -1}
		tokens[i] = t
		if ended {
			continue
		}
		if arg == "--" {
			tokens[i].end = true
			ended = true
			continue
		}
		if len(arg) < 2 || arg[0] != '-' {
			continue
		}
		name, value, inline := strings.Cut(arg, "=")
		var matches []int
		for j, a := range spec.Actions {
			if slices.Contains(a.Flags, name) {
				matches = []int{j}
				break
			}
			for _, f := range a.Flags {
				if strings.HasPrefix(name, "--") && strings.HasPrefix(f, name) {
					matches = append(matches, j)
					break
				}
			}
		}
		if len(matches) == 0 && strings.HasPrefix(arg, "-h") && !strings.HasPrefix(arg, "--") {
			matches = []int{0}
			value = ""
			inline = false // -hx executes -h before the remaining short-option cluster.
		}
		// _parse_optional resolves exact/abbreviated options (including '=')
		// before treating negative numbers or tokens containing spaces as values.
		if len(matches) == 0 && !Optional(arg) {
			continue
		}
		tokens[i].optional = true
		if len(matches) > 1 {
			var names []string
			for _, j := range matches {
				for _, f := range spec.Actions[j].Flags {
					if strings.HasPrefix(f, name) {
						names = append(names, f)
					}
				}
			}
			tokens[i].problem = "ambiguous option: " + arg + " could match " + strings.Join(names, ", ")
		}
		if len(matches) == 1 {
			tokens[i].action = matches[0]
			tokens[i].value = value
			tokens[i].inline = inline
		}
	}
	var subparser *Action
	for i := range spec.Actions {
		if spec.Actions[i].Kind == "_SubParsersAction" {
			subparser = &spec.Actions[i]
			break
		}
	}
	var unknown []string
	for i := 0; i < len(argv); i++ {
		t := tokens[i]
		if t.problem != "" {
			r.Message = t.problem
			return r
		}
		if subparser != nil && t.end {
			continue
		}
		if subparser != nil && !t.optional {
			if !slices.Contains(subparser.Choices, argv[i]) {
				var choices []string
				for _, name := range subparser.Choices {
					choices = append(choices, store.PythonRepr(name))
				}
				r.Message = "argument " + subparser.Dest + ": invalid choice: " + store.PythonRepr(argv[i]) + " (choose from " + strings.Join(choices, ", ") + ")"
				return r
			}
			r.Remaining = argv[i:]
			r.Given[subparser.Dest] = true
			break
		}
		if t.end {
			unknown = append(unknown, argv[i:]...)
			break
		}
		if t.action < 0 {
			unknown = append(unknown, argv[i])
			continue
		}
		a := spec.Actions[t.action]
		name := strings.TrimPrefix(a.Flags[len(a.Flags)-1], "--")
		label := strings.Join(a.Flags, "/")
		value := t.value
		boolean := a.Kind == "_StoreTrueAction" || a.Kind == "_StoreFalseAction" || a.Kind == "_HelpAction"
		if boolean {
			if t.inline {
				r.Message = "argument " + label + ": ignored explicit argument " + store.PythonRepr(value)
				return r
			}
			if a.Kind == "_HelpAction" {
				r.Help = true
				return r
			}
			value = strconv.FormatBool(a.Kind == "_StoreTrueAction")
		} else if !t.inline {
			if i+1 >= len(argv) || tokens[i+1].optional || tokens[i+1].end {
				r.Message = "argument " + label + ": expected one argument"
				return r
			}
			i++
			value = argv[i]
		}
		valid := true
		switch {
		case a.Type == "int":
			r.Numbers[name], valid = ParseInt(value)
		case a.Type == "float":
			r.Numbers[name], valid = ParseFloat(value)
		case a.Check != nil:
			valid = a.Check(value)
		}
		if !valid {
			r.Message = "argument " + label + ": invalid " + a.Type + " value: " + store.PythonRepr(value)
			return r
		}
		if a.Choices != nil && !slices.Contains(a.Choices, value) {
			var choices []string
			for _, v := range a.Choices {
				choices = append(choices, store.PythonRepr(v))
			}
			r.Message = "argument " + label + ": invalid choice: " + store.PythonRepr(value) + " (choose from " + strings.Join(choices, ", ") + ")"
			return r
		}
		for _, g := range spec.Groups {
			if slices.Contains(g.Actions, t.action) {
				for _, j := range g.Actions {
					other := spec.Actions[j]
					key := strings.TrimPrefix(other.Flags[len(other.Flags)-1], "--")
					if j != t.action && r.Given[key] {
						r.Message = "argument " + label + ": not allowed with argument " + strings.Join(other.Flags, "/")
						return r
					}
				}
			}
		}
		if number, ok := r.Numbers[name]; ok {
			value = NumberText(number)
		}
		r.Given[name] = true
		if a.Kind == "_AppendAction" {
			r.Values[name] = append(r.Values[name], value)
		} else {
			r.Values[name] = []string{value}
		}
	}
	var missing []string
	for _, a := range spec.Actions {
		if a.Kind == "_SubParsersAction" && a.Required && !r.Given[a.Dest] {
			missing = append(missing, a.Dest)
		}
		if len(a.Flags) > 0 && a.Required && !r.Given[strings.TrimPrefix(a.Flags[len(a.Flags)-1], "--")] {
			missing = append(missing, strings.Join(a.Flags, "/"))
		}
	}
	if len(missing) > 0 {
		r.Message = "the following arguments are required: " + strings.Join(missing, ", ")
		return r
	}
	for _, g := range spec.Groups {
		if !g.Required {
			continue
		}
		given := false
		var names []string
		for _, j := range g.Actions {
			a := spec.Actions[j]
			names = append(names, strings.Join(a.Flags, "/"))
			given = given || r.Given[strings.TrimPrefix(a.Flags[len(a.Flags)-1], "--")]
		}
		if !given {
			r.Message = "one of the arguments " + strings.Join(names, " ") + " is required"
			return r
		}
	}
	if subparser != nil {
		r.Unknown = unknown
		return r
	}
	if len(unknown) > 0 {
		r.Message = "unrecognized arguments: " + strings.Join(unknown, " ")
		r.Global = true
	}
	return r
}

// RootArgs binds accepted globals for the command-family runners, which retain
// their own handler interfaces. Abbreviations never reach a second parser.
func (r Result) RootArgs() []string {
	var argv []string
	for _, action := range Specs[""].Actions {
		if len(action.Flags) == 0 {
			continue
		}
		flag := action.Flags[len(action.Flags)-1]
		for _, value := range r.Values[strings.TrimPrefix(flag, "--")] {
			if action.Kind == "_StoreTrueAction" {
				argv = append(argv, flag)
			} else {
				argv = append(argv, flag+"="+value)
			}
		}
	}
	return append(argv, r.Remaining...)
}

func (r Result) Error(prog, command string) string {
	who := program(prog, command)
	if r.Global {
		command = ""
		who = prog
	}
	return fmt.Sprintf("%s\n%s: error: %s\n", Usage(prog, command), who, r.Message)
}
