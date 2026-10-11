// Package agents embeds the native role definitions (architect, executor, explorer, reviewer) as the files stand in
// this directory. internal/role registers two of them as native Codex roles; the flow engine (internal/flow/roles)
// reads all four as the base prompt of an engine thread. Both read the same bytes.
package agents

import "embed"

// FS holds the role definitions, <role>.toml.
//
//go:embed *.toml
var FS embed.FS
