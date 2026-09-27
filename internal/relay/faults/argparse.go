package faults

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

type argOption struct {
	name     string
	required bool
	boolean  bool
	integer  bool
	number   bool
	choices  []string
}

type argCommand struct {
	name    string
	options []argOption
}

func opt(name string) argOption     { return argOption{name: name} }
func req(name string) argOption     { return argOption{name: name, required: true} }
func boolean(name string) argOption { return argOption{name: name, boolean: true} }
func intOpt(name string) argOption  { return argOption{name: name, integer: true} }
func number(name string) argOption  { return argOption{name: name, number: true} }
func choices(name string, required bool, values ...string) argOption {
	return argOption{name: name, required: required, choices: values}
}

var faultArgCommands = []argCommand{
	{"fault-target", []argOption{req("product"), opt("workspace"), opt("project"), req("team"), opt("project-ref")}},
	{"fault-observe", []argOption{req("observation"), opt("adopt")}},
	{"fault-sweep", []argOption{opt("product"), opt("project"), opt("readings"), intOpt("readings-after")}},
	{"fault-show", []argOption{opt("fault"), opt("publication"), opt("product"), opt("fault-class"), opt("scope"), choices("fault-state", false, "observed", "open", "fix_pending", "resolved", "withdrawn"), intOpt("limit"), intOpt("after")}},
	{"fault-fix", []argOption{req("fault"), req("ref"), opt("detail")}},
	{"fault-reverify", []argOption{req("fault"), choices("method", true, "suite", "command", "observation"), req("ref"), choices("outcome", true, "passed", "absent", "failed"), opt("detail")}},
	{"fault-resolve", []argOption{req("fault")}},
	{"fault-next", []argOption{intOpt("limit")}},
	{"fault-claim", []argOption{req("publication"), req("owner"), boolean("takeover")}},
	{"fault-operation", []argOption{req("publication"), req("claim-token")}},
	{"fault-reconcile", []argOption{req("publication"), opt("observed"), opt("observed-fields"), boolean("prior-ended"), opt("reason"), boolean("searched")}},
	{"fault-complete", []argOption{req("publication"), opt("claim-token"), opt("readback"), opt("external-ref"), opt("project-ref"), opt("observed-fields")}},
	{"fault-fail", []argOption{req("publication"), req("claim-token"), req("error"), boolean("ended")}},
	{"fault-retry", []argOption{req("publication")}},
	{"fault-prune", []argOption{req("fault"), intOpt("keep")}},
	{"fault-adopt", []argOption{req("fault"), req("external-ref"), req("scope")}},
	{"fault-move", []argOption{req("fault"), req("scope")}},
	{"fault-queue", []argOption{req("fault"), req("kind"), req("trigger"), opt("payload")}},
	{"fault-update", []argOption{req("fault"), choices("op", true, "set_project", "reopen", "add_relation", "add_label"), opt("value")}},
	{"fault-cancel", []argOption{req("publication"), req("reason")}},
	{"fault-stage", []argOption{req("fault"), choices("stage", true, "accepted", "assigned", "merged", "installed"), req("ref"), opt("detail")}},
	{"fault-policy", []argOption{req("product"), opt("fault-class"), choices("severity", false, "notice", "degraded", "broken"), intOpt("threshold"), number("window"), opt("reason"), intOpt("limit"), opt("after")}},
	{"fault-limit", []argOption{req("product"), opt("kind"), intOpt("max-count"), number("window"), intOpt("limit"), opt("after")}},
	{"fault-attention", nil},
	{"fault-relink", []argOption{intOpt("limit")}},
	{"fault-notifications", []argOption{opt("notification-state"), intOpt("limit"), intOpt("after")}},
	{"fault-notification-raise", []argOption{req("fault"), req("reason"), opt("ref")}},
	{"fault-notification-reserve", []argOption{req("owner"), intOpt("limit")}},
	{"fault-notification-ack", []argOption{req("notification"), req("token"), req("ref")}},
	{"fault-notification-fail", []argOption{req("notification"), req("token"), req("error")}},
	{"fault-notification-reconcile", []argOption{req("notification"), choices("delivered", true, "yes", "no"), req("ref")}},
}

type faultArgError struct {
	usage, message string
	help           bool
}

func (e *faultArgError) Error() string { return e.message }

func faultCommand(name string) *argCommand {
	for i := range faultArgCommands {
		if faultArgCommands[i].name == name {
			return &faultArgCommands[i]
		}
	}
	return nil
}

func (c *argCommand) parse(argv []string) (map[string]string, error) {
	values := map[string]string{}
	set := map[string]bool{}
	var unknown []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-h" || (strings.HasPrefix(arg, "--") && strings.HasPrefix("help", strings.TrimPrefix(arg, "--"))) {
			return nil, &faultArgError{usage: faultHelp[c.name], help: true}
		}
		if !strings.HasPrefix(arg, "--") {
			unknown = append(unknown, arg)
			continue
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		matches := make([]*argOption, 0, 2)
		for j := range c.options {
			if c.options[j].name == name {
				matches = []*argOption{&c.options[j]}
				break
			}
			if strings.HasPrefix(c.options[j].name, name) {
				matches = append(matches, &c.options[j])
			}
		}
		if len(matches) > 1 {
			options := make([]string, len(matches))
			for j, match := range matches {
				options[j] = "--" + match.name
			}
			return nil, &faultArgError{usage: faultUsage[c.name], message: "ambiguous option: --" + name + " could match " + strings.Join(options, ", ")}
		}
		if len(matches) == 0 {
			unknown = append(unknown, arg)
			continue
		}
		o := matches[0]
		if o.boolean {
			if inline {
				unknown = append(unknown, arg)
				continue
			}
			values["--"+o.name], set[o.name] = "true", true
			continue
		}
		if !inline {
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
				return nil, &faultArgError{usage: faultUsage[c.name], message: "argument --" + o.name + ": expected one argument"}
			}
			i++
			value = argv[i]
		}
		if len(o.choices) > 0 && !slices.Contains(o.choices, value) {
			quoted := make([]string, len(o.choices))
			for j, one := range o.choices {
				quoted[j] = "'" + one + "'"
			}
			return nil, &faultArgError{usage: faultUsage[c.name], message: "argument --" + o.name + ": invalid choice: '" + value + "' (choose from " + strings.Join(quoted, ", ") + ")"}
		}
		if o.integer {
			if _, err := strconv.Atoi(value); err != nil {
				return nil, &faultArgError{usage: faultUsage[c.name], message: "argument --" + o.name + ": invalid int value: '" + value + "'"}
			}
		}
		if o.number {
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				return nil, &faultArgError{usage: faultUsage[c.name], message: "argument --" + o.name + ": invalid float value: '" + value + "'"}
			}
		}
		values["--"+o.name], set[o.name] = value, true
	}
	var missing []string
	for _, o := range c.options {
		if o.required && !set[o.name] {
			missing = append(missing, "--"+o.name)
		}
	}
	if len(missing) > 0 {
		return nil, &faultArgError{usage: faultUsage[c.name], message: "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	if len(unknown) > 0 {
		return nil, &faultArgError{usage: faultGlobalUsage, message: "unrecognized arguments: " + strings.Join(unknown, " ")}
	}
	if c.name == "fault-show" && set["fault"] && set["publication"] {
		return nil, &faultArgError{usage: faultUsage[c.name], message: "argument --publication: not allowed with argument --fault"}
	}
	return values, nil
}

func faultParse(prog string, command string, argv []string, stdout, stderr io.Writer) (map[string]string, int, bool) {
	parsed, err := faultCommand(command).parse(argv)
	if err == nil {
		return parsed, 0, false
	}
	var bad *faultArgError
	if !errors.As(err, &bad) {
		return nil, 0, false
	}
	usage := strings.ReplaceAll(bad.usage, "codex-session-relay", prog)
	if bad.help {
		fmt.Fprint(stdout, usage)
		return nil, contract.ExitOk, true
	}
	fmt.Fprintln(stderr, usage)
	who := prog + " " + command
	if bad.usage == faultGlobalUsage {
		who = prog
	}
	fmt.Fprintf(stderr, "%s: error: %s\n", who, bad.message)
	return nil, 2, true
}
