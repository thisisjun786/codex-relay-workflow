package hook

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// nativeRegistration recognizes the shipped direct command, not a substring or a
// shell wrapper we cannot interpret. Python registrations retain their old parser.
// The optional '; exit 0' is the documented host fail-open suffix (decision 12).
var nativeExitSuffix = regexp.MustCompile(`;\s*exit\s+0\s*$`)

func nativeRegistration(command string) (target, settings string, ok bool) {
	command = strings.TrimSpace(command)
	command = nativeExitSuffix.ReplaceAllString(command, "")
	words, parsed := shellSplit(command)
	if !parsed || len(words) < 2 || len(words) > 3 {
		return "", "", false
	}
	if filepath.Base(words[0]) != "crw" || words[1] != "hook" {
		return "", "", false
	}
	target = words[0]
	// Expand the direct command's HOME prefixes without evaluating shell code.
	// Single-quoted and escaped dollars, and quoted tildes, remain literal.
	if strings.HasPrefix(command, `"$HOME/`) || strings.HasPrefix(command, `"${HOME}/`) || strings.HasPrefix(command, "$HOME/") || strings.HasPrefix(command, "${HOME}/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
		target = strings.Replace(target, "${HOME}", home, 1)
		target = strings.Replace(target, "$HOME", home, 1)
	}
	if strings.HasPrefix(command, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
		target = filepath.Join(home, strings.TrimPrefix(target, "~/"))
	}
	// `--plugin-launch` (decision 26) names no settings: that run reads the CODEX_HOME settings.
	if len(words) == 3 && words[2] != PluginLaunch {
		settings = words[2]
	}
	return target, settings, true
}
