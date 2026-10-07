package cli

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ScanAction is the parser's record or help action.
type ScanAction string

const (
	ScanActionRecord ScanAction = "record"
	ScanActionHelp   ScanAction = "help"
)

// ScanDimensionText preserves an explicitly supplied fact or gap.
type ScanDimensionText struct {
	Dimension interview.Dimension `json:"dimension"`
	Text      string              `json:"text"`
}

// ScanCliArgs ports scan-cli.ts:44-58 (CXC v0.2.40, 3c1459ac).
// Counts retain the oracle's float64 range and signed zero. Optional fields
// are omitted when unused. Parsing does not read or write session state.
type ScanCliArgs struct {
	Action                 ScanAction                                       `json:"action"`
	SessionID              string                                           `json:"sessionId"`
	ContradictionCount     float64                                          `json:"contradictionCount"`
	HighContradictionCount float64                                          `json:"highContradictionCount"`
	Cwd                    string                                           `json:"cwd"`
	Derive                 bool                                             `json:"derive,omitempty"`
	Map                    map[string]interview.Dimension                   `json:"map,omitempty"`
	MapOrder               []string                                         `json:"-"`
	Dims                   map[interview.Dimension]interview.DimensionLevel `json:"dims,omitempty"`
	Known                  []ScanDimensionText                              `json:"known,omitempty"`
	Unknown                []ScanDimensionText                              `json:"unknown,omitempty"`
	Confidence             map[interview.Dimension]float64                  `json:"confidence,omitempty"`
}

// ScanCliParsed holds exactly one of Args and a non-empty Error.
type ScanCliParsed struct {
	Args  *ScanCliArgs
	Error string
}

// ParseScanCliArgs ports scan-cli.ts:79-207. argv excludes the scan token.
// Help is recognized only as the action. Value flags consume the next token
// even when it is a flag; repeated scalar/map values overwrite, facts append.
func ParseScanCliArgs(argv []string, cwd string) ScanCliParsed {
	a := &ScanCliArgs{Cwd: cwd}
	if len(argv) > 0 {
		a.Action = ScanAction(argv[0])
	}
	if a.Action == "help" || a.Action == "--help" || a.Action == "-h" {
		a.Action = ScanActionHelp
		return ScanCliParsed{Args: a}
	}
	if a.Action != ScanActionRecord {
		return ScanCliParsed{Error: fmt.Sprintf("unknown scan action '%s'; run crw pabcd scan --help", a.Action)}
	}
	for i := 1; i < len(argv); i++ {
		flag := argv[i]
		next := func(fallback string) string {
			i++
			if i < len(argv) {
				return argv[i]
			}
			return fallback
		}
		switch flag {
		case "--session":
			a.SessionID = next("")
		case "--cwd":
			a.Cwd = next(cwd)
		case "--contradictions":
			a.ContradictionCount = scanParseInt(next(""))
		case "--high":
			a.HighContradictionCount = scanParseInt(next(""))
		case "--derive":
			a.Derive = true
		case "--map", "--dim", "--known", "--unknown", "--confidence":
			if err := scanPairFlag(a, flag, next("")); err != "" {
				return ScanCliParsed{Error: err}
			}
		default:
			return ScanCliParsed{Error: fmt.Sprintf("unknown argument '%s'", flag)}
		}
	}
	if a.SessionID == "" {
		return ScanCliParsed{Error: "scan record: --session <id> is required (mutating command, no latest-session fallback)"}
	}
	// CRW-871: a non-canonical id is refused here, before the runner reads, locks or writes: the state
	// library sanitises the key, so a raw id would rewrite a DIFFERENT session's file.
	if !state.IsCanonicalSessionID(a.SessionID) {
		return ScanCliParsed{Error: "scan record: " + sessionAliasRefusalText}
	}
	if !scanFinite(a.ContradictionCount) || a.ContradictionCount < 0 {
		return ScanCliParsed{Error: "scan record: --contradictions must be a non-negative integer"}
	}
	if !scanFinite(a.HighContradictionCount) || a.HighContradictionCount < 0 {
		return ScanCliParsed{Error: "scan record: --high must be a non-negative integer"}
	}
	return ScanCliParsed{Args: a}
}

func scanIsDimension(raw string) bool {
	dims := interview.DimensionOrder()
	return slices.Contains(dims[:], interview.Dimension(raw))
}

func scanIsLevel(raw string) bool {
	levels := interview.DimensionLevels()
	return slices.Contains(levels[:], interview.DimensionLevel(raw))
}

func scanSplitPair(raw string) (key, value string, ok bool) {
	at := strings.IndexByte(raw, '=')
	if at <= 0 {
		return "", "", false
	}
	return raw[:at], raw[at+1:], true
}

func scanDimensionError(raw string) string {
	var names []string
	for _, dim := range interview.DimensionOrder() {
		names = append(names, string(dim))
	}
	return fmt.Sprintf("scan record: unknown dimension '%s' (expected %s)", raw, strings.Join(names, "|"))
}

func scanPairFlag(a *ScanCliArgs, flag, raw string) string {
	key, value, ok := scanSplitPair(raw)
	if !ok {
		shape := "<dimension>=<text>"
		switch flag {
		case "--map":
			shape = "<questionId>=<dimension>"
		case "--dim":
			shape = "<dimension>=<level>"
		case "--confidence":
			shape = "<dimension>=<0..1>"
		}
		return fmt.Sprintf("scan record: %s expects %s", flag, shape)
	}
	name := key
	if flag == "--map" {
		name = value
	}
	if !scanIsDimension(name) {
		return scanDimensionError(name)
	}
	dim := interview.Dimension(key)
	switch flag {
	case "--map":
		if a.Map == nil {
			a.Map = make(map[string]interview.Dimension)
		}
		if _, present := a.Map[key]; !present {
			a.MapOrder = append(a.MapOrder, key)
		}
		a.Map[key] = interview.Dimension(value)
	case "--dim":
		if !scanIsLevel(value) {
			var names []string
			for _, level := range interview.DimensionLevels() {
				names = append(names, string(level))
			}
			return fmt.Sprintf("scan record: invalid level '%s' (expected %s)", value, strings.Join(names, "|"))
		}
		if value == string(interview.LevelMax) {
			return scanMaxLevelError
		}
		if a.Dims == nil {
			a.Dims = make(map[interview.Dimension]interview.DimensionLevel)
		}
		a.Dims[dim] = interview.DimensionLevel(value)
	case "--known", "--unknown":
		if value == "" {
			return fmt.Sprintf("scan record: %s text must not be empty", flag)
		}
		fact := ScanDimensionText{Dimension: dim, Text: value}
		if flag == "--known" {
			a.Known = append(a.Known, fact)
		} else {
			a.Unknown = append(a.Unknown, fact)
		}
	case "--confidence":
		n := scanNumber(value)
		if !scanFinite(n) || n < 0 || n > 1 {
			return fmt.Sprintf("scan record: --confidence must be within [0,1], got '%s'", value)
		}
		if a.Confidence == nil {
			a.Confidence = make(map[interview.Dimension]float64)
		}
		a.Confidence[dim] = n
	}
	return ""
}

func scanFinite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

// Decimal parseInt accepts the longest digit prefix and preserves negative zero.
func scanParseInt(raw string) float64 {
	s := text.Trim(raw)
	i := 0
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return math.NaN()
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil && !math.IsInf(n, 0) {
		return math.NaN()
	}
	return n
}

// Number syntax differs from Go's ParseFloat (no underscores or hex floats).
// Unlike planPhaseNumber, confidence permits fractions and signed underflow.
func scanNumber(raw string) float64 {
	s := text.Trim(raw)
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, err := strconv.ParseUint(s[2:], base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(n)
		}
	}
	valid, _ := regexp.MatchString(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`, s)
	if !valid {
		return math.NaN()
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(n, 0) {
		return math.NaN()
	}
	return n
}

const scanMaxLevelError = "scan record: --dim cannot set 'max'. To make a dimension count for I->P, ask a " +
	"question, record the answer, and attribute it: " +
	"`crw pabcd scan record --session <id> --derive --map <questionId>=<dimension>`. " +
	"The gate re-reads the interview ledger, so a level with no answered question " +
	"behind it does not open P. If the interview genuinely is NOT complete, bypass it " +
	"deliberately: write {\"from\":\"I\",\"to\":\"P\",\"did\":\"<reason>\",\"override\":true} to a " +
	"file and run `crw pabcd orchestrate P --session <id> --attest-file <path>` (the file flag " +
	"is required on Windows) so the bypass is attested and recorded."
