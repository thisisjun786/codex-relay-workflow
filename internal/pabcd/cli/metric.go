package cli

// metricCliStub is the red-first stub: the ported signatures with zero answers. The next commit of this
// branch replaces this file with the port of metric-cli.ts, so the final tree never carries the stub.
func RenderMetricHelp() string { return "" }

func RunMetricCLI(argv []string, cwd, stdin string) (CliResult, error) {
	_, _, _ = argv, cwd, stdin
	return CliResult{}, nil
}
