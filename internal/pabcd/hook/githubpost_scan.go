package hook

import "github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"

// Shell command shape and JSON request expansion stay in the hook. Content
// checks are shared with the native review publisher (CRW-1105).
func githubPostSecretLine(content string) (int, bool) { return publishpolicy.SecretLine(content) }
