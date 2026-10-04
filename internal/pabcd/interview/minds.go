package interview

type Mind string

const (
	MindContrarian        Mind = "contrarian"
	MindSocratic          Mind = "socratic"
	MindOntologist        Mind = "ontologist"
	MindEvaluator         Mind = "evaluator"
	MindSimplifier        Mind = "simplifier"
	MindConcurrencyCap         = 3
	MindDispatchDirective      = ""
)

func Minds() [5]Mind {
	return [5]Mind{MindContrarian, MindSocratic, MindOntologist, MindEvaluator, MindSimplifier}
}
func MindRolePrompt(mind Mind) string { return "" }

type MindContradiction struct {
	Mind          Mind                  `json:"mind"`
	CorrelationID string                `json:"correlationId"`
	Dimension     Dimension             `json:"dimension"`
	Contradiction string                `json:"contradiction"`
	Severity      ContradictionSeverity `json:"severity"`
	Evidence      string                `json:"evidence"`
}

func NormalizeMindOutput(mind Mind, raw any, roundID ...any) []MindContradiction { return nil }
func SelectMinds(tracker any, count ...float64) []Mind                           { return nil }
