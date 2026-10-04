package cli

// metric.go ports CXC v0.2.40 pabcd-state/src/metric-cli.ts (commit 3c1459ac) as a library over
// internal/pabcd/metric. The verb row that reaches it is added by the follow-on wiring issue; this file
// registers nothing and does no work at program start (no init, no package-level variable initializers).

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// metricCliUsage is usage() of metric-cli.ts:17-28, the answer to an unknown verb. It names no binary, so
// no name substitution applies to it.
const metricCliUsage = `metric: expected one of:
  metric record --session <id> --name <metric> --value <number> [--source operator-entered|evaluate.sh] [--work-phase <id>] [--json]
  metric ingest --session <id> [--source evaluate.sh] [--work-phase <id>] [--json]  # reads METRIC name=value lines from stdin
  metric show --session <id> [--json]
  metric kind --session <id> [satisfy|maximize] [--json]`

// metricCliHelp is renderMetricHelp() of metric-cli.ts:72-90 under the settled CRW command name
// (contract/schema/cxc/name-substitution.json, the cli table's metric row and R11's loop-skill name).
const metricCliHelp = `crw pabcd metric — session-scoped objective metrics for maximize-goal loops

Usage:
  crw pabcd metric record --session <id> --name <metric> --value <number> [--source operator-entered|evaluate.sh] [--work-phase <id>] [--json]
  crw pabcd metric ingest --session <id> --source evaluate.sh [--json]   (reads stdin)
  crw pabcd metric show --session <id> [--json]
  crw pabcd metric kind --session <id> [--set satisfy|maximize] [--json]
  crw pabcd metric parse-line --session <id>                             (reads stdin)
  crw pabcd metric --help

Notes:
  Two non-improving rows on the same metric switch the Stop block to
  "step back and re-plan with divergence" (crw-loop objective plateau).
  --source records HOW the number was obtained; an operator-entered value and
  an evaluate.sh value are not interchangeable evidence.`

// RenderMetricHelp is renderMetricHelp(): the usage text of the metric command, which the verb answers for
// help, --help and -h with exit status 0.
func RenderMetricHelp() string { return metricCliHelp }

// metricCliFlag is readFlag of metric-cli.ts:30-34: the first occurrence's next token, and false when the
// flag is absent or is the last token (the oracle's argv[idx + 1] ?? null).
func metricCliFlag(argv []string, name string) (string, bool) {
	if value := readFlagValue(argv, name); value != nil {
		return *value, true
	}
	return "", false
}

// metricCliSource is readSource of metric-cli.ts:59-62: the --source value, or the fallback when the flag
// is absent or last; a value that is neither of the two sources is refused.
func metricCliSource(argv []string, fallback metric.ObjectiveMetricSource) (metric.ObjectiveMetricSource, bool) {
	raw := fallback
	if value := readFlagValue(argv, "--source"); value != nil {
		raw = metric.ObjectiveMetricSource(*value)
	}
	return raw, raw == metric.OperatorEntered || raw == metric.EvaluateSh
}

// metricCliWorkPhase is readFlag(argv, "--work-phase") ?? undefined: an absent or last flag is the library's
// default work phase, and an empty value is kept as written.
func metricCliWorkPhase(argv []string) *string { return readFlagValue(argv, "--work-phase") }

// metricCliPositionals is positionalArgs of metric-cli.ts:40-53: the tokens that are neither a value flag nor
// a flag-shaped token. --set is not a value flag, so "kind --set maximize" reaches it as the positional
// "maximize" (a defect of the oracle, recorded in docs/port-cxc/known-defects.md).
func metricCliPositionals(argv []string) []string {
	out := []string{}
	values := []string{"--session", "-s", "--name", "--value", "--source", "--work-phase"}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if slices.Contains(values, arg) {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// metricCliKind is parseObjectiveKind of metric-cli.ts:64-66.
func metricCliKind(raw string) *metric.ObjectiveKind {
	if raw == string(metric.Satisfy) || raw == string(metric.Maximize) {
		kind := metric.ObjectiveKind(raw)
		return &kind
	}
	return nil
}

// metricCliDigit reports whether c is a digit of base (2, 8 or 16).
func metricCliDigit(c byte, base int) bool {
	switch {
	case c >= '0' && c <= '9':
		return int(c-'0') < base
	case c >= 'a' && c <= 'f':
		return int(c-'a')+10 < base
	case c >= 'A' && c <= 'F':
		return int(c-'A')+10 < base
	}
	return false
}

// metricCliNumber is Number(rawValue) of metric-cli.ts:110 for the spellings the guard lets through:
// JavaScript whitespace is trimmed, "" and a whitespace-only value are 0, a 0x/0o/0b form is read at any
// length (so it is not capped at 64 bits, unlike scanNumber), Infinity keeps its sign, a decimal that
// overflows is infinite, one that underflows is 0, and everything else is NaN. The caller refuses a
// non-finite result, as the oracle's !Number.isFinite does.
func metricCliNumber(raw string) float64 {
	s := text.Trim(raw)
	if s == "" {
		return 0
	}
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
			for i := 2; i < len(s); i++ {
				if !metricCliDigit(s[i], base) {
					return math.NaN()
				}
			}
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64() // rounds to nearest even; infinite past the range
			return f
		}
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	decimal := regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	if !decimal.MatchString(s) {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(s, 64) // a range error already carries the value Number() gives
	return f
}

// metricCliNumberText is String(number) of the oracle's template literals. json.Marshal spells finite
// numbers as ES6 does (1e21 is 1e+21, 1e-7 is 1e-7); a zero is "0" because String(-0) is "0".
func metricCliNumberText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	b, _ := json.Marshal(f)
	return string(b)
}

// metricCliQuote is JSON.stringify of one string, over the package's encoder (SetEscapeHTML(false) plus the
// U+2028/U+2029 pass). Encoding one string cannot fail.
func metricCliQuote(s string) string {
	out, _ := statusJSON(s)
	return out
}

// metricCliRowList is JSON.stringify of an array of rows, each spelled by the metric package.
func metricCliRowList(records []metric.Record) string {
	rows := make([]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, metric.Encode(r))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// RunMetricCLI is runMetricCli of metric-cli.ts:92-151: argv without the metric token, and stdin already
// read by the caller (the verb's 64 KiB bound belongs to the wiring). A failure of the metric library is
// returned as an error with a zero result, the shape the harness prints as the oracle's dispatcher prints
// an uncaught throw (crw cli failed: <message>, exit 1).
func RunMetricCLI(argv []string, cwd, stdin string) (CliResult, error) {
	verb := ""
	if len(argv) > 0 {
		verb = argv[0]
	}
	json := slices.Contains(argv, "--json")
	// 260825 wp1: --help used to be rejected with "--session <id> is required", so the usage text was
	// unreachable from the documented entry point. Help is only the first token; "record --help" is a
	// record that misses its session, as the recorded fixtures show.
	if len(argv) == 0 || isHelpToken(verb) {
		return CliResult{Code: 0, Output: RenderMetricHelp()}, nil
	}
	sessionID, haveSession := metricCliFlag(argv, "--session")
	if !haveSession {
		sessionID, haveSession = metricCliFlag(argv, "-s")
	}
	if !haveSession || sessionID == "" {
		return CliResult{Code: 1, Output: "metric: --session <id> is required"}, nil
	}

	switch verb {
	case "record":
		name, haveName := metricCliFlag(argv, "--name")
		if !haveName || name == "" {
			return CliResult{Code: 1, Output: "metric record: --name <metric> is required"}, nil
		}
		source, sourceOK := metricCliSource(argv, metric.OperatorEntered)
		if !sourceOK {
			return CliResult{Code: 1, Output: "metric record: --source must be operator-entered or evaluate.sh"}, nil
		}
		rawValue, haveValue := metricCliFlag(argv, "--value")
		value := metricCliNumber(rawValue)
		if !haveValue || rawValue == "" || math.IsNaN(value) || math.IsInf(value, 0) {
			return CliResult{Code: 1, Output: "metric record: --value <number> is required"}, nil
		}
		record, err := metric.RecordObjectiveMetric(cwd, metric.RecordInput{
			SessionID: sessionID, MetricName: name, Value: value, Source: source, WorkPhaseID: metricCliWorkPhase(argv),
		})
		if err != nil {
			return CliResult{}, err
		}
		if json {
			return CliResult{Code: 0, Output: metric.Encode(record)}, nil
		}
		return CliResult{Code: 0, Output: fmt.Sprintf("metric record: %s=%s best=%s source=%s",
			record.MetricName, metricCliNumberText(record.Value), metricCliNumberText(record.Best), record.Source)}, nil

	case "ingest":
		source, sourceOK := metricCliSource(argv, metric.EvaluateSh)
		if !sourceOK {
			return CliResult{Code: 1, Output: "metric ingest: --source must be operator-entered or evaluate.sh"}, nil
		}
		records, err := metric.RecordMetricsFromText(cwd, metric.TextInput{
			SessionID: sessionID, Text: stdin, Source: source, WorkPhaseID: metricCliWorkPhase(argv),
		})
		if err != nil {
			return CliResult{}, err
		}
		if json {
			return CliResult{Code: 0, Output: `{"records":` + metricCliRowList(records) + "}"}, nil
		}
		return CliResult{Code: 0, Output: fmt.Sprintf("metric ingest: recorded %d METRIC line(s)", len(records))}, nil

	case "show":
		records := metric.ReadObjectiveMetrics(cwd, sessionID)
		if json {
			return CliResult{Code: 0, Output: `{"sessionId":` + metricCliQuote(sessionID) + `,"records":` + metricCliRowList(records) + "}"}, nil
		}
		if len(records) == 0 {
			return CliResult{Code: 0, Output: "metric show: no records for session " + sessionID}, nil
		}
		lines := make([]string, 0, len(records))
		for _, r := range records {
			lines = append(lines, fmt.Sprintf("%s=%s best=%s source=%s phase=%s",
				r.MetricName, metricCliNumberText(r.Value), metricCliNumberText(r.Best), r.Source, r.WorkPhaseID))
		}
		return CliResult{Code: 0, Output: strings.Join(lines, "\n")}, nil

	case "kind":
		var requested *metric.ObjectiveKind
		if positionals := metricCliPositionals(argv[1:]); len(positionals) > 0 {
			requested = metricCliKind(positionals[0])
		}
		if requested != nil {
			if err := metric.WriteObjectiveKind(cwd, sessionID, *requested); err != nil {
				return CliResult{}, err
			}
		}
		kind := metric.ReadObjectiveKind(cwd, sessionID)
		if json {
			explicit := "null"
			if requested != nil {
				explicit = metricCliQuote(string(*requested))
			}
			return CliResult{Code: 0, Output: `{"sessionId":` + metricCliQuote(sessionID) + `,"kind":` + metricCliQuote(string(kind)) + `,"explicit":` + explicit + "}"}, nil
		}
		output := "metric kind: " + string(kind)
		if requested != nil {
			output += " (explicit)"
		}
		return CliResult{Code: 0, Output: output}, nil

	case "parse-line":
		// The oracle's help says it reads stdin, but v0.2.40 joins the argv after the verb, so the ordinary
		// forms answer null with exit 1 (fixture cli__metric__record_show_kind_parse_line).
		name, value, ok := metric.ParseMetricLine(strings.Join(argv[1:], " "))
		if !ok {
			return CliResult{Code: 1, Output: "null"}, nil
		}
		return CliResult{Code: 0, Output: `{"metricName":` + metricCliQuote(name) + `,"value":` + metricCliNumberText(value) + "}"}, nil
	}
	return CliResult{Code: 1, Output: metricCliUsage}, nil
}
