package role

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// HelperArgs ports ParsedSubagentsArgs (CXC v0.2.40 cli.ts:21-111).
// Scope records the redundant trailing --global selector: the store has only
// one global layer. Project trust-token and dispatch are outside this port.
type HelperArgs struct {
	Action string
	Role   RoleName
	Scope  ConfigScope
	Patch  RolePatch
	Err    string
}
type HelperResult struct {
	Code   int
	Output string
}

// HelperCommand owns its output and status, receiving arguments after its name.
// The dispatch port adds its stdin-driven callback to HelperCommands.
type HelperCommand struct {
	Name string
	Run  func([]string, io.Reader, io.Writer, host.LookupEnv) int
}

func ParseHelperArgs(args []string) HelperArgs {
	if helperCLIArg(args, 0) == "register" {
		if len(args) == 2 && (args[1] == "executor" || args[1] == "architect") {
			return HelperArgs{Action: "register", Role: RoleName(args[1])}
		}
		return HelperArgs{Action: "register", Err: "usage: subagents register executor|architect"}
	}
	// Strip a selector, never a model or prompt whose literal value is --global.
	if len(args) > 0 && args[len(args)-1] == "--global" && !slices.Contains([]string{"--prompt", "--model", "--fallback-model"}, helperCLIArg(args, len(args)-2)) {
		parsed := helperCLIProjectArgs(args[:len(args)-1])
		parsed.Scope = ScopeGlobal
		return parsed
	}
	return helperCLIProjectArgs(args)
}

// This is parseProjectArgs with its project-only trust-token removed. The name
// identifies the oracle unit; all operations still use the global store.
func helperCLIProjectArgs(args []string) HelperArgs {
	sub := helperCLIArg(args, 0)
	if len(args) == 0 || sub == "list" {
		return HelperArgs{Action: "list"}
	}
	if sub == "help" || sub == "--help" || sub == "-h" {
		return HelperArgs{Action: "help"}
	}
	parsed := HelperArgs{Action: sub}
	role := RoleName(helperCLIArg(args, 1))
	switch sub {
	case "reset":
		if !validRole(role) || len(args) != 2 {
			parsed.Err = "reset requires exactly one valid role"
		} else {
			parsed.Role = role
		}
	case "get", "set":
		if !validRole(role) {
			parsed.Err = fmt.Sprintf("unknown role '%s' (expected %s)", role, helperCLIRoles("|"))
			return parsed
		}
		parsed.Role = role
		if sub == "set" {
			parsed.Patch, parsed.Err = helperCLISetArgs(args)
		}
	default:
		return HelperArgs{Action: "help", Err: fmt.Sprintf("unknown subcommand '%s'", sub)}
	}
	return parsed
}

func helperCLISetArgs(args []string) (RolePatch, string) {
	var patch RolePatch
	clearFallback, setFallback, changed := false, false, false
	for i := 2; i < len(args); i++ {
		flag := args[i]
		changed = true
		switch flag {
		case "--mode":
			i++
			v := RoleMode(helperCLIArg(args, i))
			if v != ModeDefault && v != ModeModel {
				return RolePatch{}, fmt.Sprintf("--mode must be default|model (got '%s')", v)
			}
			patch.Mode = Some(v)
		case "--model":
			i++
			patch.Model = Some(helperCLIArg(args, i))
		case "--fallback-model", "--fallback-effort":
			i++
			value := helperCLIArg(args, i)
			fallback := FallbackPatch{}
			if patch.Fallback.Value != nil {
				fallback = *patch.Fallback.Value
			}
			if flag == "--fallback-model" {
				if text.Trim(value) == "" {
					return RolePatch{}, "--fallback-model requires a model id"
				}
				fallback.Model = Some(value)
			} else {
				if value != "inherit" && !validEffort(EffortName(value)) {
					return RolePatch{}, "invalid --fallback-effort"
				}
				fallback.Effort = Null[EffortName]()
				if value != "inherit" {
					fallback.Effort = Some(EffortName(value))
				}
			}
			patch.Fallback, setFallback = Some(fallback), true
		case "--clear-fallback":
			patch.Fallback, clearFallback = Null[FallbackPatch](), true
		case "--effort":
			i++
			v := EffortName(helperCLIArg(args, i))
			if !validEffort(v) {
				return RolePatch{}, fmt.Sprintf("--effort must be %s (got '%s')", helperCLIEfforts("|"), v)
			}
			patch.Effort = Some(v)
		case "--clear-effort":
			patch.Effort = Null[EffortName]()
		case "--prompt":
			i++
			patch.PromptOverride = Some(helperCLIArg(args, i))
		case "--clear-prompt":
			patch.PromptOverride = Null[string]()
		default:
			return RolePatch{}, fmt.Sprintf("unknown flag '%s'", flag)
		}
	}
	if clearFallback && setFallback {
		return RolePatch{}, "--clear-fallback cannot be combined with fallback settings"
	}
	if !changed {
		return RolePatch{}, "set requires at least one of --mode/--model/--effort/--clear-effort/--prompt/--clear-prompt/--fallback-model/--fallback-effort/--clear-fallback"
	}
	return patch, ""
}

func helperCLIArg(args []string, i int) string {
	if i >= 0 && i < len(args) {
		return args[i]
	}
	return ""
}
func helperCLIRoles(separator string) string {
	var names []string
	for _, r := range Roles() {
		names = append(names, string(r))
	}
	return strings.Join(names, separator)
}
func helperCLIEfforts(separator string) string {
	var names []string
	for _, e := range Efforts() {
		names = append(names, string(e))
	}
	return strings.Join(names, separator)
}

// HelperHelp is HELP with CRW command names and the global-only store contract.
func HelperHelp() string {
	return strings.Join([]string{
		"crw role helper — per-role subagent model/prompt config (.crw/subagents.json)", "",
		"  crw role helper               list all role configs",
		"  crw role helper get <role>    show one role config",
		"  crw role helper set <role> --mode default|model [--model <id>] [--effort <level>|--clear-effort] [--prompt <text>|--clear-prompt]",
		"  --fallback-model <id> [--fallback-effort low|medium|high|xhigh|inherit] | --clear-fallback",
		"  crw role helper register executor|architect   register or update managed role; restart Codex afterward",
		"  crw role helper reset <role>  remove the role override and inherit the session",
		"  Settings use the global store; trailing --global is accepted for compatibility.", "",
		"  roles: " + helperCLIRoles(", "),
		"  efforts: " + helperCLIEfforts(", ") + " (unset = inherit the parent session's effort)",
	}, "\n")
}

// RunHelper is runSubagents over the existing Go store and registration API.
// Native home is optional, including the distinction between absent and blank.
func RunHelper(parsed HelperArgs, env host.LookupEnv, nativeHome ...string) HelperResult {
	if parsed.Err != "" {
		return helperCLIFailure(parsed.Err)
	}
	if parsed.Action == "help" {
		return HelperResult{Output: HelperHelp()}
	}
	if parsed.Action == "register" {
		return helperCLIRegister(parsed.Role, env, nativeHome)
	}
	var cfg Config
	var err error
	switch parsed.Action {
	case "list", "get":
		cfg, err = ReadConfig(env)
	case "set":
		cfg, err = SetRole(env, parsed.Role, parsed.Patch)
	case "reset":
		cfg, err = ResetRole(env, parsed.Role)
	default:
		return helperCLIFailure(fmt.Sprintf("unknown subcommand '%s'", parsed.Action))
	}
	if err != nil {
		return helperCLIFailure(err.Error())
	}
	var value any = cfg
	if parsed.Action != "list" {
		value = cfg.Roles[parsed.Role]
	}
	encoded, err := Stringify(value, "  ")
	if err != nil {
		return helperCLIFailure(err.Error())
	}
	return HelperResult{Output: string(encoded)}
}

func helperCLIRegister(role RoleName, env host.LookupEnv, homes []string) HelperResult {
	if len(homes) == 0 {
		home, err := host.Home(env)
		if err != nil {
			return helperCLIFailure(err.Error())
		}
		homes = []string{ResolveNativeRoleHome(env, home)}
	}
	result, err := RegisterRole(role, homes...)
	if err != nil {
		return helperCLIFailure(err.Error())
	}
	prefix := "Already registered"
	if result.Created {
		prefix = "Registered"
	} else if result.Updated {
		prefix = "Updated"
	}
	return HelperResult{Output: fmt.Sprintf("%s: %s\nStart a new Codex session and verify %s appears in the live spawn schema.", prefix, result.Path, role)}
}
func helperCLIFailure(message string) HelperResult {
	return HelperResult{Code: 1, Output: "subagents: " + message}
}

func HelperCommands() []HelperCommand {
	return []HelperCommand{
		{Name: "dispatch", Run: DispatchCommand},
		helperCLIRow("list"), helperCLIRow("get"), helperCLIRow("set"), helperCLIRow("reset"),
		helperCLIRow("register"), helperCLIRow("help"), helperCLIRow("--help"), helperCLIRow("-h"),
	}
}
func helperCLIRow(name string) HelperCommand {
	return HelperCommand{Name: name, Run: func(args []string, _ io.Reader, stdout io.Writer, env host.LookupEnv) int {
		return helperCLIRun(append([]string{name}, args...), stdout, env)
	}}
}

// CLI is the role mode's explicit entry. Go main/mode dispatch replaces Node's
// isDirect realpath check (cli.ts:179-205); importing role runs nothing.
func CLI(args []string, in io.Reader, stdout, stderr io.Writer, env host.LookupEnv) int {
	const usage = "usage: crw role [-h] {helper} ..."
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) == 0 || args[0] != "helper" {
		fmt.Fprintln(stderr, usage)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "crw role: error: a command is required")
		} else {
			fmt.Fprintf(stderr, "crw role: error: invalid command %q (choose from helper)\n", args[0])
		}
		return 2
	}
	args = args[1:]
	if len(args) > 0 && args[0] == "dispatch-lock-clear" { // not a HelperCommands row: TestHelperCLI pins that table's names
		return DispatchLockClearCommand(args[1:], in, stdout, env)
	}
	for _, command := range HelperCommands() {
		if len(args) > 0 && args[0] == command.Name {
			return command.Run(args[1:], in, stdout, env)
		}
	}
	return helperCLIRun(args, stdout, env)
}
func helperCLIRun(args []string, stdout io.Writer, env host.LookupEnv) int {
	result := RunHelper(ParseHelperArgs(args), env)
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}
