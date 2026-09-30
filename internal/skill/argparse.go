package skill

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type pythonOption struct {
	name      string
	valueName string
}

type pythonCommand struct {
	help     string
	position string
	minArgs  int
	maxArgs  int
	options  []pythonOption
}

type pythonFamily struct {
	program  string
	usage    string
	help     string
	required string
	order    []string
	commands map[string]pythonCommand
}

var pythonNegativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

func pythonHelpArgument(arg string) (bool, string) {
	name, value, explicit := strings.Cut(arg, "=")
	if strings.HasPrefix(name, "--") && len(name) > 2 && strings.HasPrefix("--help", name) || strings.HasPrefix(name, "-h") && !strings.HasPrefix(name, "--") {
		if explicit {
			return false, "argument -h/--help: ignored explicit argument " + argvRepr(value)
		}
		return true, ""
	}
	return false, ""
}

func precheckPythonArgs(args []string, stdout, stderr io.Writer) ([]string, int, bool) {
	family, ok := pythonArgparseFamilies[args[0]]
	if !ok {
		return args, 0, false
	}
	parsed, code, handled := family.parse(args[1:], stdout, stderr)
	if handled {
		return nil, code, true
	}
	return append([]string{args[0]}, parsed...), 0, false
}

func (family pythonFamily) parse(args []string, stdout, stderr io.Writer) ([]string, int, bool) {
	positionalOnly := false
	var extras []string
	for len(args) > 0 {
		if args[0] == "--" {
			args, positionalOnly = args[1:], true
			break
		}
		if help, problem := pythonHelpArgument(args[0]); help {
			_, _ = io.WriteString(stdout, family.help)
			return nil, 0, true
		} else if problem != "" {
			return nil, family.error(stderr, family.usage, problem), true
		}
		if !strings.HasPrefix(args[0], "-") {
			break
		}
		extras, args = append(extras, args[0]), args[1:]
	}
	if len(args) == 0 {
		return nil, family.error(stderr, family.usage, "the following arguments are required: "+family.required), true
	}
	command, ok := family.commands[args[0]]
	if !ok {
		if strings.HasPrefix(args[0], "-") && !positionalOnly {
			return nil, family.error(stderr, family.usage, "the following arguments are required: "+family.required), true
		}
		choices := make([]string, len(family.order))
		for i, choice := range family.order {
			choices[i] = "'" + choice + "'"
		}
		message := fmt.Sprintf("argument %s: invalid choice: %s (choose from %s)", family.required, argvRepr(args[0]), strings.Join(choices, ", "))
		return nil, family.error(stderr, family.usage, message), true
	}
	parsed, message, usage, help := family.parseCommand(args[0], args[1:], command)
	if help {
		_, _ = io.WriteString(stdout, command.help)
		return nil, 0, true
	}
	if message != "" {
		if strings.HasPrefix(message, "unrecognized arguments:") {
			return nil, family.error(stderr, usage, message), true
		}
		return nil, family.commandError(stderr, args[0], usage, message), true
	}
	if len(extras) > 0 {
		return nil, family.error(stderr, family.usage, "unrecognized arguments: "+strings.Join(extras, " ")), true
	}
	return append([]string{args[0]}, parsed...), 0, false
}

func (family pythonFamily) parseCommand(name string, args []string, command pythonCommand) ([]string, string, string, bool) {
	usage := strings.SplitN(command.help, "\n\n", 2)[0] + "\n"
	parsed := make([]string, 0, len(args))
	positionals := make([]string, 0, command.maxArgs)
	extras := make([]string, 0)
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !positionalOnly {
			if help, problem := pythonHelpArgument(arg); help {
				return nil, "", "", true
			} else if problem != "" {
				return nil, problem, usage, false
			}
		}
		if !positionalOnly && arg == "--" {
			positionalOnly = true
			if command.maxArgs == 0 {
				extras = append(extras, arg)
			}
			continue
		}
		if !positionalOnly && strings.HasPrefix(arg, "-") {
			option, inline, found := command.matchOption(arg)
			if !found {
				extras = append(extras, arg)
				continue
			}
			parsed = append(parsed, option.name)
			if option.valueName == "" {
				if inline != nil {
					return nil, "argument " + option.name + ": ignored explicit argument " + argvRepr(*inline), usage, false
				}
				continue
			}
			if inline != nil {
				parsed = append(parsed, *inline)
				continue
			}
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") && !pythonNegativeNumber.MatchString(args[i+1]) {
				return nil, "argument " + option.name + ": expected one argument", usage, false
			}
			i++
			parsed = append(parsed, args[i])
			continue
		}
		if len(positionals) < command.maxArgs {
			positionals = append(positionals, arg)
			parsed = append(parsed, arg)
		} else {
			extras = append(extras, arg)
		}
	}
	if len(positionals) < command.minArgs {
		return nil, "the following arguments are required: " + command.position, usage, false
	}
	if len(extras) > 0 {
		return nil, "unrecognized arguments: " + strings.Join(extras, " "), family.usage, false
	}
	return parsed, "", "", false
}

func (command pythonCommand) matchOption(arg string) (pythonOption, *string, bool) {
	name, value, hasValue := strings.Cut(arg, "=")
	matches := make([]pythonOption, 0, 1)
	for _, option := range command.options {
		if name == option.name || strings.HasPrefix(option.name, name) {
			matches = append(matches, option)
		}
	}
	if len(matches) != 1 {
		return pythonOption{}, nil, false
	}
	if hasValue {
		return matches[0], &value, true
	}
	return matches[0], nil, true
}

// error and commandError print what parser.error prints. A message quotes the arguments as
// sys.argv holds them (os.fsdecode: each byte outside well-formed UTF-8 is a lone surrogate),
// and sys.stderr writes each lone surrogate as its \udcXX escape. A repr() in the message is
// well-formed UTF-8 with its surrogates escaped already, so decoding the whole message changes
// only the arguments it prints as they are.
func (family pythonFamily) error(stderr io.Writer, usage, message string) int {
	_, _ = io.WriteString(stderr, usage)
	fmt.Fprintf(stderr, "%s: error: %s\n", family.program, stderrText(store.FSDecode(message)))
	return 2
}

func (family pythonFamily) commandError(stderr io.Writer, command, usage, message string) int {
	_, _ = io.WriteString(stderr, usage)
	fmt.Fprintf(stderr, "%s %s: error: %s\n", family.program, command, stderrText(store.FSDecode(message)))
	return 2
}

var pythonArgparseFamilies = map[string]pythonFamily{
	"hook-probe": {
		program: "hook_probe.py", usage: "usage: hook_probe.py [-h] {observe,decide,replay} ...\n", help: hookProbeRootHelp, required: "command",
		order: []string{"observe", "decide", "replay"},
		commands: map[string]pythonCommand{
			"observe": {help: hookProbeObserveHelp, maxArgs: 0, options: []pythonOption{{"--binary", "BINARY"}, {"--codex-home", "CODEX_HOME"}, {"--sanitize", ""}}},
			"decide":  {help: hookProbeDecideHelp, position: "observation", minArgs: 1, maxArgs: 1},
			"replay":  {help: hookProbeReplayHelp, maxArgs: 0, options: []pythonOption{{"--fixtures", "FIXTURES"}, {"--contract", "CONTRACT"}, {"--host-fixtures", "HOST_FIXTURES"}, {"--allow-unreached", ""}}},
		},
	},
	"parent-title": {
		program: "parent_title.py", usage: "usage: parent_title.py [-h] {decide,readback,replay} ...\n", help: parentTitleRootHelp, required: "command",
		order: []string{"decide", "readback", "replay"},
		commands: map[string]pythonCommand{
			"decide":   {help: parentTitleDecideHelp, maxArgs: 0},
			"readback": {help: parentTitleReadbackHelp, maxArgs: 0},
			"replay":   {help: parentTitleReplayHelp, maxArgs: 0, options: []pythonOption{{"--fixtures", "FIXTURES"}, {"--allow-unreached", ""}}},
		},
	},
	"start-policy": {
		program: "start_policy.py", usage: "usage: start_policy.py [-h] {vocabulary,check,selftest} ...\n", help: startPolicyRootHelp, required: "mode",
		order: []string{"vocabulary", "check", "selftest"},
		commands: map[string]pythonCommand{
			"vocabulary": {help: startPolicyVocabularyHelp, maxArgs: 0},
			"check":      {help: startPolicyCheckHelp, position: "record", maxArgs: 1},
			"selftest":   {help: startPolicySelftestHelp, maxArgs: 0},
		},
	},
}
