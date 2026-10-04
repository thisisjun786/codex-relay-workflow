package role

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"

const AliasMapDate = "2026-09-24"

type ModelAlias string
type ResolvedAlias struct {
	ID       string `json:"id"`
	Verified bool   `json:"verified"`
	MapDate  string `json:"mapDate"`
}

func ModelAliases() map[ModelAlias]string                                { return nil }
func ResolveDispatchAlias(name string, env host.LookupEnv) ResolvedAlias { return ResolvedAlias{} }
func RenderDispatchCard(host.LookupEnv) (string, error)                  { return "", nil }
