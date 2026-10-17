package cli

import "strings"

// workDiagnostic updates command hints inherited from the unchanged state/gate
// packages at the presentation boundary. Never apply it to stored records or to
// a receipt child process's stdout/stderr.
func workDiagnostic(s string) string {
	return strings.NewReplacer(
		"crw pabcd orchestrate", "crw work state",
		"crw pabcd loop", "crw work continue",
		"crw pabcd plan", "crw work outline",
		"crw pabcd review-round", "crw work assessment",
		"crw pabcd", "crw work",
		"IPABCD", "task work",
		"PABCD", "task work",
	).Replace(s)
}
