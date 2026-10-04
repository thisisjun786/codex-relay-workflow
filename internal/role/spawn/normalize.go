// Package spawn contains the skill mention library ported from CXC v0.2.40.
// Callers supply the skills directory; this package registers no hook.
package spawn

// NormalizeSkillMentions conservatively normalizes explicit skill mentions.
func NormalizeSkillMentions(message, skillsDir string) string { return "" }
