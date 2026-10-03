package search

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"

// AdapterPreamble is the oracle text after the CRW name substitutions.
const AdapterPreamble = `[crw external skill adapter]
- This is an EXTERNAL skill. crw dev discipline (crw-dev) always wins on conflict.
- Substitute Claude-specific tools with Codex equivalents:
  claude -p / claude CLI -> codex exec; Read/Grep/Glob tools -> shell (cat/rg/fd).
- Resolve path placeholders ({baseDir}, $CODEX_HOME/skills/...) against the skill's
  raw URL directory, not the local filesystem.
- If the skill name collides with a crw built-in (dev-*, search), the built-in
  is authoritative; use this document as supplementary reference only.
`

// SearchFooter resolves the runtime invocation when emitted, never at startup.
// A resolver failure keeps the original fail-open literal-command rung.
func SearchFooter(env host.LookupEnv) string {
	invocation, err := host.Invocation(env)
	if err != nil {
		invocation = "crw"
	}
	return "# external skills: load with `" + invocation + " skill show <id>` (adapter preamble applies; crw-dev wins on conflict)"
}
