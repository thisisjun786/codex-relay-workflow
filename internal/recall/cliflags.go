package recall

type ParsedFlags struct {
	Values      map[string]any `json:"values"`
	Positionals []string       `json:"positionals"`
}

func Usage() string                                { return "" }
func WantsHelp([]string) bool                      { return false }
func ParseFlags([]string) (ParsedFlags, error)     { return ParsedFlags{}, nil }
func ReadFlags([]string) (ParsedFlags, error)      { return ParsedFlags{}, nil }
func FlagLikePathError(map[string]any) error       { return nil }
func NumFlag(map[string]any, string) *float64      { return nil }
func ExplicitHome(map[string]any) (*string, error) { return nil, nil }
