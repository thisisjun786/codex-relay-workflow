package spawn

const InlineSkillOpen = "<skill name=\"crw-"
const InlineSkillClose = "</skill>"

func InlineSkillBodies(message, skillsDir string) string    { return message }
func SkillBlocks(texts []string, skillsDir string) []string { return nil }
func MentionedFolders(message string) map[string]bool       { return map[string]bool{} }
func LeafSafeSkillFolders() []string                        { return nil }
func spawnInlineDecodeUTF8(data []byte) string              { return string(data) }
