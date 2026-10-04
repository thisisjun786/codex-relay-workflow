package role

import (
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

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
type HelperCommand struct {
	Name string
	Run  func([]string, io.Reader, io.Writer, host.LookupEnv) int
}

func ParseHelperArgs(args []string) HelperArgs { return HelperArgs{} }
func RunHelper(parsed HelperArgs, env host.LookupEnv, nativeHome ...string) HelperResult {
	return HelperResult{}
}
func HelperHelp() string                                                                { return "" }
func HelperCommands() []HelperCommand                                                   { return nil }
func CLI(args []string, in io.Reader, stdout, stderr io.Writer, env host.LookupEnv) int { return 0 }
