//go:build dev

package laneparity

import (
	"fmt"
	"path/filepath"
	"strings"
)

// CommandExecutable is the file a declared command line starts first, as the replay case resolves
// it: an absolute path, or crw / $CRW_BIN, which the case's PATH and environment point at the build
// under test (crw). False for a command whose first word names no file the harness can identify
// (another variable, a relative path, a shell builtin such as exit).
func CommandExecutable(command, crw string) (string, bool) {
	word, ok := firstWord(command)
	if !ok {
		return "", false
	}
	switch word {
	case "crw", "$CRW_BIN", "${CRW_BIN}":
		return crw, true
	case "$HOME/" + runtimeBin, "${HOME}/" + runtimeBin:
		// The installed runtime the shipped plugin starts: the harness links it to the build under test
		// in the HOME of every step it fires (seedPlan), so that build is what the command starts.
		return crw, true
	}
	if strings.Contains(word, "$") || strings.ContainsAny(word, "`\\") || !filepath.IsAbs(word) {
		return "", false
	}
	return word, true
}

// firstWord is the first word of a command line, as the shell reads it: the text in double quotes
// or up to the first blank or operator.
func firstWord(command string) (string, bool) {
	s := strings.TrimLeft(command, " \t")
	var word string
	if strings.HasPrefix(s, `"`) {
		end := strings.Index(s[1:], `"`)
		if end < 0 {
			return "", false
		}
		word = s[1 : 1+end]
	} else {
		word = s
		if i := strings.IndexAny(word, " \t;&|"); i >= 0 {
			word = word[:i]
		}
	}
	return word, true
}

// CommandBuild is the sha256 of the executable a declared command starts, or empty and why not:
// it names no identifiable file, or the file cannot be read.
func CommandBuild(command, crw string) (digest, problem string) {
	path, ok := CommandExecutable(command, crw)
	if !ok {
		return "", fmt.Sprintf("the declared command %q starts no executable the harness can identify", command)
	}
	digest, err := FileDigest(path)
	if err != nil {
		return "", fmt.Sprintf("the declared command %q: %v", command, err)
	}
	return digest, ""
}
