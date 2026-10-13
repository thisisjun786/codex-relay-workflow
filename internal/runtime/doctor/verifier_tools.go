package doctor

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// VerifierToolsReport is the verifierTools entry of the doctor report (CRW-1319): the verifier's tools as a run with env reads them
// (the configuration's verifier section, else PATH), the go it runs, where that go came from and its version, whether crw-dev is
// runnable and, with no Go root configured, the Go toolchain roots found on the host (crwconfig.ProbeVerifierTools, the probe the
// preflight fact verifier_prerequisites reads too, with the seams of opts). Everything wrong is a warning; the entry never makes
// the report a fault.
func VerifierToolsReport(ctx context.Context, env scope.Env, opts crwconfig.ProbeOptions) Object {
	p := crwconfig.ProbeVerifierToolsWith(ctx, env, "", opts)
	warnings := []any{}
	for _, w := range append(append([]string{}, p.Problems...), p.Notes...) {
		warnings = append(warnings, w)
	}
	var config, command any
	if p.Tools.Config != "" {
		config = p.Tools.Config
	}
	if p.Command != nil {
		argv := []any{}
		for _, a := range p.Command {
			argv = append(argv, a)
		}
		command = argv
	}
	candidates := []any{}
	for _, c := range p.Candidates {
		candidates = append(candidates, Object{{Key: "root", Value: c.Root}, {Key: "version", Value: c.Version}})
	}
	return Object{
		{Key: "config", Value: config},
		{Key: "goRoot", Value: nullable(p.Tools.GoRoot)},
		{Key: "go", Value: nullable(p.Go)},
		{Key: "goSource", Value: nullable(p.GoSource)},
		{Key: "goVersion", Value: nullable(p.GoVersion)},
		{Key: "goRootCandidates", Value: candidates},
		{Key: "crwDev", Value: Object{{Key: "mode", Value: p.Mode}, {Key: "command", Value: command}, {Key: "runnable", Value: p.Runnable}}},
		{Key: "warnings", Value: warnings},
	}
}
