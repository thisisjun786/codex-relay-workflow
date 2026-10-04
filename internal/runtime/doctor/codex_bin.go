package doctor

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"strings"
)

// CodexInvocation is the POSIX spawn shape of codex-bin.ts:106-114.
type CodexInvocation struct {
	File    string          `json:"file"`
	Args    []string        `json:"args"`
	Options map[string]bool `json:"options"`
}

// ResolveCodexInvocation applies CODEX_BIN and copies args without executing or
// resolving on PATH. envValue's exact-key preference and case-insensitive scan
// remain on POSIX too. CRW_BIN's directive resolution belongs to host.Invocation.
func ResolveCodexInvocation(command string, args, env []string) CodexInvocation {
	lookup := host.LookupEnv(func(key string) (string, bool) {
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
				return v, true
			}
		}
		for _, e := range env {
			if k, v, ok := strings.Cut(e, "="); ok && strings.EqualFold(k, key) {
				return v, true
			}
		}
		return "", false
	})
	override, _ := lookup("CODEX_BIN")
	if override = text.Trim(override); override != "" {
		command = override
	}
	return CodexInvocation{File: command, Args: append([]string{}, args...), Options: map[string]bool{}}
}
