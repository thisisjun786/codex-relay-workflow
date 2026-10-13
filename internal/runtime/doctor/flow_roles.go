package doctor

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// flowRolesCLI is `crw doctor flow-roles [--json]` (CRW-1330, CRW-1316 design section 8): the helper role store a flow run reads
// its six pairs from (its path, whether a file is there, the canonical digest of the six pairs, each role's pair and where it came
// from) and every warning: the snapshot's, the execution policy loader's, and one when the execution policy still declares
// agent-roles, which the flow no longer reads. It prints one JSON document, reads the store and the policy file only, and exits 0
// whatever it warns about (each warning is a reversible setting, warned and not refused); a policy file that does not load is
// exit 1 with the reason, as it stops the bridge and the flow alike.
func flowRolesCLI(args []string, stdout, stderr io.Writer, env host.LookupEnv) int {
	flags := flag.NewFlagSet("crw doctor flow-roles", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Bool("json", true, "print the report as JSON (the only form)")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageExit
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "crw doctor flow-roles: unknown argument %q\n", flags.Arg(0))
		return usageExit
	}
	snap := role.ReadFlowSnapshot(env)
	roles := make([]any, 0, len(snap.Roles))
	for _, r := range snap.Roles {
		roles = append(roles, contract.OrderedObject{{Key: "role", Value: string(r.Role)}, {Key: "model", Value: nullable(r.Model)}, {Key: "reasoningEffort", Value: nullable(r.Effort)}, {Key: "state", Value: string(r.State)}})
	}
	warnings := append([]string{}, snap.Warnings...)
	vars := map[string]string{}
	for _, name := range []string{execution.EnvPolicy, execution.EnvDigest} {
		if v, ok := env(name); ok {
			vars[name] = v
		}
	}
	source := strings.TrimSpace(vars[execution.EnvPolicy])
	exit := 0
	var policyDetail any
	policy, err := execution.FromEnvironment(vars)
	if err != nil {
		policyDetail, exit = err.Error(), 1
	} else {
		warnings = append(warnings, policy.Warnings()...)
		if policy.DeclaresAgentRoles() {
			warnings = append(warnings, fmt.Sprintf("the execution policy %s declares agent-roles: the flow no longer reads it (its pairs come from the helper role store %s); compare its pairs with crw flow roles, then remove the section", source, snap.Path))
		}
	}
	list := make([]any, len(warnings))
	for i, w := range warnings {
		list[i] = w
	}
	report := contract.OrderedObject{
		{Key: "store", Value: nullable(snap.Path)}, {Key: "present", Value: snap.Present}, {Key: "digest", Value: snap.Digest()},
		{Key: "roles", Value: roles}, {Key: "policy", Value: nullable(source)}, {Key: "policyError", Value: policyDetail}, {Key: "warnings", Value: list},
	}
	if err := contract.Emit(stdout, report); err != nil {
		return 1
	}
	return exit
}

// flowRolesEnv is the process environment the subcommand reads.
func flowRolesEnv() host.LookupEnv { return host.LookupEnv(os.LookupEnv) }
