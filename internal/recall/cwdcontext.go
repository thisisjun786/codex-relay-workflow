package recall

type CwdSession struct {
	Path     string  `json:"path"`
	ThreadID *string `json:"threadId"`
	Date     string  `json:"date"`
	Excerpt  string  `json:"excerpt"`
}

type CwdSessionOptions struct {
	IndexPath     string
	Home          string
	ExcerptChars  *int
	ReadOriginUrl ReadOriginUrl
}

type SummaryEntry struct {
	Relpath string `json:"relpath"`
	Title   string `json:"title"`
}

func ListCwdSessions(string, int, ...CwdSessionOptions) []CwdSession { return nil }
func LoadSummaryIndex(...string) map[string]SummaryEntry             { return map[string]SummaryEntry{} }
