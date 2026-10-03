package bridge

import (
	"slices"
	"strings"
)

func sortedNames(names []string) []string { slices.Sort(names); return names }
func equalNames(a, b []string) bool {
	return slices.Equal(sortedNames(slices.Clone(a)), sortedNames(slices.Clone(b)))
}
func indexOf(items []string, want string) int { return slices.Index(items, want) }
func contains(text, part string) bool         { return strings.Contains(text, part) }
