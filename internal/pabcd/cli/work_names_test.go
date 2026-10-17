package cli

import "strings"

// The upstream recordings stay immutable. Only their public command spellings
// and help titles change under the CRW-1527 name map; all other bytes still compare.
func workOracleNames(s string) string {
	return strings.NewReplacer(
		"crw pabcd orchestrate", "crw work state",
		"crw pabcd loop", "crw work continue",
		"crw pabcd plan", "crw work outline",
		"crw pabcd review-round", "crw work assessment",
		"crw pabcd", "crw work",
		"multi-cycle PABCD loop", "multi-cycle task work",
	).Replace(s)
}
