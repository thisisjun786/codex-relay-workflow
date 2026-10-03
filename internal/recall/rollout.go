package recall

type ChatEntry struct {
	TS string `json:"ts"`
	Role string `json:"role"`
	Text string `json:"text"`
	MatchField string `json:"matchField"`
	Synthetic bool `json:"synthetic"`
}

func ParseRollout(content string, includeTools bool) ([]ChatEntry, error) {
	return []ChatEntry{}, nil
}
