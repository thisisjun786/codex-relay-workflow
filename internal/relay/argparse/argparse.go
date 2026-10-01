// Package argparse reads relay command lines. specs.json declares each command's options (its
// flag, whether it takes a value, its type, its choices, whether it is required, its help line),
// the groups of options that exclude one another, and which parsers name a command with their
// first word (the root's and service's). The declarations began as the Python relay's argparse
// parsers; the reading is this package's own (decision R3C-1):
//
//   - an option is its full flag, given as `--flag value` or `--flag=value` (no abbreviation);
//   - -h or --help, as a token of its own, asks for help;
//   - a value that looks like an option (it starts with "-", is not a negative number and holds
//     no space) is not taken as a value: the option is missing its value;
//   - an int is a signed 64-bit decimal integer and a float is what strconv.ParseFloat reads;
//   - help and usage are printed as declared, one line per option, never wrapped.
//
// A line the parser cannot read is a usage error: the caller prints the usage line and the
// message on stderr and exits 2.
package argparse

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

//go:embed specs.json
var data []byte

// The kinds of option. They carry the names of the argparse action classes the relay's parsers
// were declared with, which other packages compare.
const (
	KindValue  = ""                  // takes one value; a later one replaces an earlier one
	KindAppend = "_AppendAction"     // takes one value each time it is given; every value is kept
	KindTrue   = "_StoreTrueAction"  // takes no value; given, it is "true"
	KindFalse  = "_StoreFalseAction" // takes no value; given, it is "false"
	KindHelp   = "_HelpAction"       // -h, --help
)

// Action is one option of a parser.
type Action struct {
	Flags    []string
	Kind     string
	Type     string // "int", "float", or the name of what Check accepts
	Required bool
	Choices  []string
	Help     string
	// Check accepts or refuses a value of a Type other than int or float. Only a parser
	// declared in Go carries one.
	Check func(string) bool
}

// Group is a set of options (indices into Actions) at most one of which may be given; a
// required group needs one.
type Group struct {
	Required bool
	Actions  []int
}

// Spec is one parser: a command's, or the root's ("") or service's, whose first word names a
// command (Commands).
type Spec struct {
	Summary, Description string
	Commands             bool
	Actions              []Action
	Groups               []Group
}

var help = Action{Flags: []string{"-h", "--help"}, Kind: KindHelp, Help: "show this help and exit"}

// Specs are the relay's parsers by name: "" (the root), each root command, and "service <sub>".
var Specs, names = load()

func load() (map[string]Spec, []string) {
	var declared []struct {
		Name, Summary, Description string
		Commands                   bool
		Options                    []struct {
			Flag, Kind, Type, Help string
			Required               bool
			Choices                []string
		}
		Groups []struct {
			Required bool
			Options  []string
		}
	}
	if err := json.Unmarshal(data, &declared); err != nil {
		panic(err)
	}
	kinds := map[string]string{"": KindValue, "append": KindAppend, "true": KindTrue, "false": KindFalse}
	specs := map[string]Spec{}
	var names []string
	for _, d := range declared {
		spec := Spec{Summary: d.Summary, Description: d.Description, Commands: d.Commands, Actions: []Action{help}}
		for _, o := range d.Options {
			kind, ok := kinds[o.Kind]
			if !ok {
				panic("argparse: option " + o.Flag + " of " + d.Name + " has kind " + o.Kind)
			}
			spec.Actions = append(spec.Actions, Action{Flags: []string{o.Flag}, Kind: kind, Type: o.Type, Required: o.Required, Choices: o.Choices, Help: o.Help})
		}
		for _, g := range d.Groups {
			group := Group{Required: g.Required}
			for _, flag := range g.Options {
				at := spec.find(flag)
				if at < 0 {
					panic("argparse: group of " + d.Name + " names " + flag)
				}
				group.Actions = append(group.Actions, at)
			}
			spec.Groups = append(spec.Groups, group)
		}
		specs[d.Name] = spec
		names = append(names, d.Name)
	}
	return specs, names
}

// Commands are the commands a parser's first word may name, in declaration order: the root's
// ("") are the relay's commands, service's are "service <sub>".
func Commands(parent string) []string {
	var commands []string
	for _, name := range names {
		if parent == "" && name != "" && !strings.Contains(name, " ") || parent != "" && strings.HasPrefix(name, parent+" ") {
			commands = append(commands, name)
		}
	}
	return commands
}

// find is the action whose flag is exactly flag, or -1.
func (s Spec) find(flag string) int {
	for i, a := range s.Actions {
		if slices.Contains(a.Flags, flag) {
			return i
		}
	}
	return -1
}

// Key is the name a parsed option's value is kept under: its long flag without the dashes.
func (a Action) Key() string { return strings.TrimPrefix(a.Flags[len(a.Flags)-1], "--") }

func (a Action) label() string { return strings.Join(a.Flags, "/") }

func (a Action) takesValue() bool { return a.Kind == KindValue || a.Kind == KindAppend }

// metavar names an option's value in usage and help: its choices, or its key in capitals.
func (a Action) metavar() string {
	if a.Choices != nil {
		return "{" + strings.Join(a.Choices, ",") + "}"
	}
	return strings.ToUpper(strings.ReplaceAll(a.Key(), "-", "_"))
}

var negativeNumber = regexp.MustCompile(`^-([0-9]+|[0-9]*\.[0-9]+)$`)

// Optional is whether a token looks like an option, so it is not taken as an option's value:
// it starts with "-", is longer than that, is not a negative number and holds no space.
func Optional(token string) bool {
	return len(token) > 1 && token[0] == '-' && !negativeNumber.MatchString(token) && !strings.Contains(token, " ")
}

// Result is a parsed command line.
type Result struct {
	// Values are each given option's values by key: the text given (an int or float in its
	// canonical spelling), "true" or "false" for an option that takes none.
	Values map[string][]string
	// Numbers are the int (*big.Int, always within int64) and float (float64) options' values.
	Numbers map[string]any
	Given   map[string]bool
	// Message is why the line cannot be read; Help, that it asked for help.
	Message string
	Help    bool
	// Remaining is, for a parser whose first word names a command, that word and the rest.
	Remaining []string
}

// Parse reads argv against the named parser.
func Parse(command string, argv []string) Result { return ParseSpec(Specs[command], argv) }

// ParseSpec reads argv against spec. Options are read in order: the first that cannot be read
// is the answer, unless -h or --help came first. Words the parser does not know are collected
// and reported once the line is read, before a missing required option. In a parser that names commands, "--" ends the options.
func ParseSpec(spec Spec, argv []string) Result {
	r := Result{Values: map[string][]string{}, Numbers: map[string]any{}, Given: map[string]bool{}}
	var unknown []string
	for i := 0; i < len(argv); i++ {
		token := argv[i]
		if token == "--" && spec.Commands {
			// The end of options: the next word names the command, whatever it looks like. A
			// command's parser reads no words, so there "--" is refused with what follows it.
			if i+1 < len(argv) {
				r.Remaining = argv[i+1:]
			}
			break
		}
		name, value, inline := strings.Cut(token, "=")
		at := spec.find(name)
		if at < 0 {
			if spec.Commands && !Optional(token) {
				r.Remaining = argv[i:]
				break
			}
			unknown = append(unknown, token)
			continue
		}
		a := spec.Actions[at]
		if !a.takesValue() && inline {
			r.Message = "argument " + a.label() + ": takes no value, but was given " + strconv.Quote(value)
			return r
		}
		switch a.Kind {
		case KindHelp:
			r.Help = true
			return r
		case KindTrue, KindFalse:
			value = strconv.FormatBool(a.Kind == KindTrue)
		default:
			if !inline {
				if i+1 >= len(argv) || Optional(argv[i+1]) {
					r.Message = "argument " + a.label() + ": expected one argument"
					return r
				}
				i++
				value = argv[i]
			}
			if message := r.convert(a, value); message != "" {
				r.Message = message
				return r
			}
		}
		for _, g := range spec.Groups {
			if !slices.Contains(g.Actions, at) {
				continue
			}
			for _, j := range g.Actions {
				if j != at && r.Given[spec.Actions[j].Key()] {
					r.Message = "argument " + a.label() + ": not allowed with argument " + spec.Actions[j].label()
					return r
				}
			}
		}
		key := a.Key()
		if number, ok := r.Numbers[key]; ok {
			value = NumberText(number)
		}
		r.Given[key] = true
		if a.Kind == KindAppend {
			r.Values[key] = append(r.Values[key], value)
		} else {
			r.Values[key] = []string{value}
		}
	}
	if len(unknown) > 0 {
		r.Message = "unrecognized arguments: " + strings.Join(unknown, " ")
		return r
	}
	var missing []string
	for _, a := range spec.Actions {
		if a.Required && !r.Given[a.Key()] {
			missing = append(missing, a.label())
		}
	}
	if spec.Commands && r.Remaining == nil {
		missing = append(missing, "command")
	}
	if len(missing) > 0 {
		r.Message = "the following arguments are required: " + strings.Join(missing, ", ")
		return r
	}
	for _, g := range spec.Groups {
		if !g.Required || slices.ContainsFunc(g.Actions, func(j int) bool { return r.Given[spec.Actions[j].Key()] }) {
			continue
		}
		var flags []string
		for _, j := range g.Actions {
			flags = append(flags, spec.Actions[j].label())
		}
		r.Message = "one of the arguments " + strings.Join(flags, " ") + " is required"
		return r
	}
	return r
}

// convert checks a value against its option's type and choices, keeping a number in Numbers;
// it answers why the value is refused, or "".
func (r *Result) convert(a Action, value string) string {
	valid := true
	switch {
	case a.Type == "int":
		n, err := strconv.ParseInt(value, 10, 64)
		valid = err == nil
		r.Numbers[a.Key()] = big.NewInt(n)
	case a.Type == "float":
		f, err := strconv.ParseFloat(value, 64)
		valid = err == nil
		r.Numbers[a.Key()] = f
	case a.Check != nil:
		valid = a.Check(value)
	}
	if !valid {
		delete(r.Numbers, a.Key())
		return fmt.Sprintf("argument %s: invalid %s value: %q", a.label(), a.Type, value)
	}
	if a.Choices != nil && !slices.Contains(a.Choices, value) {
		return fmt.Sprintf("argument %s: invalid choice: %q (choose from %s)", a.label(), value, strings.Join(a.Choices, ", "))
	}
	return ""
}

// program is how a parser is invoked: prog, then the command for a command's parser.
func program(prog, command string) string {
	if command != "" {
		return prog + " " + command
	}
	return prog
}

// Usage is a parser's usage line: every option, a required one without brackets.
func Usage(prog, command string) string {
	spec := Specs[command]
	parts := []string{"usage: " + program(prog, command)}
	for _, a := range spec.Actions {
		part := a.Flags[0]
		if a.takesValue() {
			part += " " + a.metavar()
		}
		if !a.Required {
			part = "[" + part + "]"
		}
		parts = append(parts, part)
	}
	if spec.Commands {
		parts = append(parts, "<command> ...")
	}
	return strings.Join(parts, " ")
}

// Help is a parser's help: its usage line, what it does, its options with their help lines
// and, for a parser that names a command, the commands.
func Help(prog, command string) string {
	spec := Specs[command]
	var b strings.Builder
	b.WriteString(Usage(prog, command) + "\n")
	for _, text := range []string{spec.Description, spec.Summary} {
		if text != "" {
			b.WriteString("\n" + text + "\n")
			break
		}
	}
	table := func(title string, rows [][2]string) {
		b.WriteString("\n" + title + ":\n")
		column := 0
		for _, row := range rows {
			column = max(column, len(row[0]))
		}
		column = min(column, 30)
		for _, row := range rows {
			line := "  " + row[0]
			if row[1] != "" && len(row[0]) > column {
				line += "\n" + strings.Repeat(" ", column+4)
			} else if row[1] != "" {
				line += strings.Repeat(" ", column-len(row[0])+2)
			}
			b.WriteString(line + row[1] + "\n")
		}
	}
	var options [][2]string
	for _, a := range spec.Actions {
		header := strings.Join(a.Flags, ", ")
		if a.takesValue() {
			header += " " + a.metavar()
		}
		options = append(options, [2]string{header, a.Help})
	}
	table("options", options)
	if spec.Commands {
		var commands [][2]string
		for _, name := range Commands(command) {
			commands = append(commands, [2]string{strings.TrimPrefix(name, command+" "), Specs[name].Summary})
		}
		table("commands", commands)
	}
	return b.String()
}

// Error is the usage error's text on stderr: the usage line, then the message.
func (r Result) Error(prog, command string) string {
	return fmt.Sprintf("%s\n%s: error: %s\n", Usage(prog, command), program(prog, command), r.Message)
}
