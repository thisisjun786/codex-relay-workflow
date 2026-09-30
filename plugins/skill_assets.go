// Package plugins supplies the canonical skill data to standalone CRW binaries.
package plugins

import "embed"

// SkillFiles holds the contracts and the replay corpus the skill commands read.
// This declaration stays outside the installed plugin root; the data is not copied.
//
//go:embed crw/skills/crw-run/references/start-policy.md crw/skills/crw-run/references/hook-contract.md crw/skills/crw-run/scripts/fixtures/decisions/*.json crw/skills/crw-run/scripts/fixtures/titles/*.json crw/skills/crw-run/scripts/fixtures/host/*.json
var SkillFiles embed.FS
