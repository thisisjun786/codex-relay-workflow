package workdirs

import "testing"

func TestIsRoot(t *testing.T) {
	for rel, want := range map[string]bool{
		"flow/run-1/land-wt":       true,
		"flow/run-1/node-wt":       true,
		"flow/run-1/wt":            true,
		"flow/run-1/calls/c1/wt":   true,
		"flow/run-1/calls/c1/wt-1": true,
		"flow/run-1/calls/c1/wt-9": true,

		"flow/run-1/calls/c1/wt-0":   false,
		"flow/run-1/calls/c1/wt-old": false,
		"flow/run-1/calls/c1/wt-01":  false,
		"flow/run-1/calls/c1":        false,
		"flow/run-1/calls":           false,
		"flow/run-1":                 false,
		"flow/run-1/records/wt":      false,
		"flow/run-1/land-wt/x":       false,
		"flow/run-1/run.json":        false,
		"flow":                       false,
		"other/run-1/land-wt":        false,
		"repo/node-wt":               false,
		"flow//run-1/wt":             false,
		"flow/run-1/../run-1/wt":     false,
		"./flow/run-1/wt":            false,
	} {
		if got := IsRoot(rel); got != want {
			t.Errorf("IsRoot(%q) = %v, want %v", rel, got, want)
		}
	}
}
