package recall

type ClippedChatResult struct {
	ChatSearchResult
	Clipped bool `json:"clipped"`
}

func FormatChatResult(ChatSearchResult) string                 { return "" }
func FormatMemoryResult(MemorySearchResult, ...float64) string { return "" }
func AgeDays(*string, float64) *float64                        { return nil }
func NewerRelpath(MemoryHit, []MemoryHit) *string              { return nil }
func ClipChatResultForJson(ChatSearchResult) ClippedChatResult { return ClippedChatResult{} }
func clip(string, int) string                                  { return "" }
func topicTokens(MemoryHit) []string                           { return nil }
func chatHitHeader(ChatHit) string                             { return "" }
