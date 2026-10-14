package pluginwiring

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"

// CodexHomeNamed is the one CODEX_HOME rule of the SessionStart hook, the bridge launcher, register-mcp and doctor (CRW-1377): the
// variable's value trimmed of the whitespace around it (text.Trim, as the installer's features command and the CXC oracle's
// resolveCodexHome trim it). "" means the variable names no Codex home, and each caller falls back to its own default, so a padded
// CODEX_HOME makes all four read and write one directory.
func CodexHomeNamed(value string) string { return text.Trim(value) }
