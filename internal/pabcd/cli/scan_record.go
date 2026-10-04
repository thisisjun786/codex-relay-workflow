package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// RunScanCli ports scan-cli.ts:211-417 (CXC v0.2.40, 3c1459ac).
// args comes from ParseScanCliArgs, including MapOrder. It registers no verb.
// Read/append/write share the state lock; unreadable or lossy state is refused
// before append. A failed state write keeps the scan row already appended.
func RunScanCli(args ScanCliArgs) CliResult {
	return scanRecordRun(args, state.AppendInterviewEvent)
}

func scanRecordRun(a ScanCliArgs, appendEvent func(string, state.InterviewEvent) error) CliResult {
	if a.Action == ScanActionHelp {
		return CliResult{Output: scanRecordHelp}
	}
	var round float64
	derivedCount := 0
	err := state.WithSessionLock(a.Cwd, a.SessionID, func() error {
		s, unreadable := state.ReadStateStrict(a.Cwd, a.SessionID)
		if unreadable {
			return errors.New("session state is unreadable; refusing to overwrite it")
		}
		if !cliVerdictsIntact(a.Cwd, a.SessionID, len(s.UnverifiedSubagents)) {
			return errors.New("session state holds unreadable unverified records; refusing to rewrite it")
		}
		tracker := s.Interview
		if tracker == nil {
			tracker = interview.DefaultInterview(0)
		}
		round = interview.ComputeNextScanRound(tracker)
		e := state.InterviewEvent{TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), SessionID: a.SessionID, Event: state.ScanCompleted, RoundID: round, ContradictionCount: a.ContradictionCount, HighContradictionCount: a.HighContradictionCount}
		if a.Derive && len(a.Map) > 0 {
			e.Map = []state.MapEntry{}
			for _, key := range a.MapOrder {
				e.Map = append(e.Map, state.MapEntry{QuestionID: key, Dimension: string(a.Map[key])})
			}
		}
		if err := appendEvent(a.Cwd, e); err != nil {
			return err
		}
		dimensions := scanRecordScores(tracker.Dimensions)
		touched := map[interview.Dimension]bool{}
		if a.Derive {
			before := scanRecordLengths(dimensions)
			if err := scanRecordDerive(a, dimensions); err != nil {
				return err
			}
			for _, d := range interview.DimensionOrder() {
				score := dimensions[d]
				if before[d] != [2]int{len(score.known), len(score.unknown)} {
					derivedCount++
					touched[d] = true
				}
			}
		}
		for _, entry := range a.Known {
			score := dimensions[entry.Dimension]
			score.known = scanRecordCap(scanRecordPushUnique(score.known, entry.Text))
			touched[entry.Dimension] = true
		}
		for _, entry := range a.Unknown {
			score := dimensions[entry.Dimension]
			score.unknown = scanRecordCap(scanRecordPushUnique(score.unknown, entry.Text))
			touched[entry.Dimension] = true
		}
		for d := range touched {
			score := dimensions[d]
			if score.level != interview.LevelMax {
				score.level = scanRecordLevel(score)
			}
		}
		for d, value := range a.Confidence {
			dimensions[d].confidence = value
		}
		for d, level := range a.Dims {
			dimensions[d].level = level
		}
		next := *tracker
		for _, d := range interview.DimensionOrder() {
			score := dimensions[d]
			*next.Dimensions.Score(d) = interview.DimensionScore{Level: score.level, Known: scanRecordStrings(score.known), Unknown: scanRecordStrings(score.unknown), Confidence: score.confidence}
		}
		next.ScanRounds = math.MaxInt64
		if round < float64(math.MaxInt64) {
			next.ScanRounds = int64(round)
		}
		next.LastScanRoundID = next.ScanRounds
		s.Interview = &next
		return state.WriteState(a.Cwd, s)
	})
	if err != nil {
		return CliResult{Code: 1, Output: "scan record failed: " + cliErrorMessage(err)}
	}
	derived := ""
	if a.Derive {
		derived = fmt.Sprintf(", derived=%d dimension(s) from the answer ledger", derivedCount)
		if derivedCount == 0 {
			if len(a.Map) == 0 {
				derived += " — WARNING: no --map given, so every question was skipped and the tracker stays empty."
			} else {
				derived += " — WARNING: nothing matched; check that --map ids match the ledger's questionIds."
			}
		}
	}
	return CliResult{Output: fmt.Sprintf("scan record: round %s recorded for session %s (contradictions=%s, high=%s%s)", scanRecordNumberText(round), a.SessionID, scanRecordNumberText(a.ContradictionCount), scanRecordNumberText(a.HighContradictionCount), derived)}
}

// Working values stay raw until write-side normalization, like the oracle.
// JSON objects/arrays carry reference identity for Map keys and includes().
type scanRecordScore struct {
	level          interview.DimensionLevel
	known, unknown []any
	confidence     float64
}
type scanRecordReference struct{ value any }
type scanRecordUndefined struct{}
type scanRecordQuestion struct{ id, text any }

func scanRecordScores(dims interview.Dimensions) map[interview.Dimension]*scanRecordScore {
	out := map[interview.Dimension]*scanRecordScore{}
	for _, d := range interview.DimensionOrder() {
		s := dims.Score(d)
		score := &scanRecordScore{level: s.Level, confidence: s.Confidence}
		for _, s := range s.Known {
			score.known = append(score.known, s)
		}
		for _, s := range s.Unknown {
			score.unknown = append(score.unknown, s)
		}
		out[d] = score
	}
	return out
}
func scanRecordLengths(scores map[interview.Dimension]*scanRecordScore) map[interview.Dimension][2]int {
	out := map[interview.Dimension][2]int{}
	for d, s := range scores {
		out[d] = [2]int{len(s.known), len(s.unknown)}
	}
	return out
}
func scanRecordPushUnique(list []any, value any) []any {
	for _, prior := range list {
		if prior == value {
			return list
		}
	}
	return append(list, value)
}
func scanRecordCap(list []any) []any {
	if len(list) > interview.MaxTrackerArray {
		return list[:interview.MaxTrackerArray]
	}
	return list
}
func scanRecordStrings(list []any) []string {
	out := []string{}
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func scanRecordLevel(s *scanRecordScore) interview.DimensionLevel {
	if len(s.known) == 0 && len(s.unknown) == 0 {
		return interview.LevelLow
	}
	if len(s.unknown) > 0 {
		return interview.LevelMid
	}
	return interview.LevelHigh
}

func scanRecordDerive(a ScanCliArgs, scores map[interview.Dimension]*scanRecordScore) error {
	questions := []scanRecordQuestion{}
	positions := map[any]int{}
	answers := map[any][]any{}
	for _, ev := range ledger.ReadQaEvents(a.Cwd, a.SessionID) {
		var row map[string]any
		decoder := json.NewDecoder(bytes.NewReader(ev.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&row); err != nil {
			return err
		}
		id, exists := row["questionId"]
		if !exists {
			id = scanRecordUndefined{}
		} else {
			id = scanRecordValue(id)
		}
		question := scanRecordValue(row["question"])
		if ev.Event == ledger.QuestionAsked && scanRecordTruthy(question) {
			if i, seen := positions[id]; seen {
				questions[i].text = question
			} else {
				positions[id] = len(questions)
				questions = append(questions, scanRecordQuestion{id, question})
			}
		}
		if values, ok := row["answers"].([]any); ev.Event == ledger.AnswerRecorded && ok {
			for _, v := range values {
				answers[id] = scanRecordPushUnique(answers[id], scanRecordValue(v))
			}
		}
	}
	for _, q := range questions {
		key, err := scanRecordPropertyKey(q.id)
		if err != nil {
			return err
		}
		d, matched := a.Map[key]
		if !matched {
			continue
		}
		score := scores[d]
		if answered := answers[q.id]; len(answered) > 0 {
			for _, v := range answered {
				score.known = scanRecordPushUnique(score.known, v)
			}
			score.known = scanRecordCap(score.known)
			gaps := []any{}
			for _, gap := range score.unknown {
				if gap != q.text {
					gaps = append(gaps, gap)
				}
			}
			score.unknown = gaps
		} else {
			score.unknown = scanRecordCap(scanRecordPushUnique(score.unknown, q.text))
		}
	}
	return nil
}

func scanRecordValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		n, _ := strconv.ParseFloat(string(x), 64)
		return n
	case []any:
		values := make([]any, len(x))
		for i, v := range x {
			values[i] = scanRecordValue(v)
		}
		return &scanRecordReference{values}
	case map[string]any:
		values := map[string]any{}
		for k, v := range x {
			values[k] = scanRecordValue(v)
		}
		return &scanRecordReference{values}
	default:
		return v
	}
}
func scanRecordTruthy(v any) bool {
	switch x := v.(type) {
	case nil, scanRecordUndefined:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	default:
		return true
	}
}
func scanRecordPropertyKey(v any) (string, error) {
	switch x := v.(type) {
	case scanRecordUndefined:
		return "undefined", nil
	case nil:
		return "null", nil
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		return scanRecordNumberText(x), nil
	case *scanRecordReference:
		if m, ok := x.value.(map[string]any); ok {
			if _, own := m["toString"]; own {
				return "", errors.New("Cannot convert object to primitive value")
			}
			return "[object Object]", nil
		}
		values := x.value.([]any)
		parts := make([]string, len(values))
		for i, v := range values {
			if v == nil {
				continue
			}
			s, err := scanRecordPropertyKey(v)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return strings.Join(parts, ","), nil
	}
	return "", errors.New("Cannot convert object to primitive value")
}
func scanRecordNumberText(n float64) string {
	if n == 0 {
		return "0"
	}
	if math.IsInf(n, 1) {
		return "Infinity"
	}
	if math.IsInf(n, -1) {
		return "-Infinity"
	}
	if math.IsNaN(n) {
		return "NaN"
	}
	b, _ := json.Marshal(n)
	return string(b)
}

const scanRecordHelp = `crw pabcd scan — record an interview rescan round and fold answers into the tracker

Usage:
  crw pabcd scan record --session <id> [--cwd <path>] [--contradictions N] [--high N]
                  [--derive] [--map <questionId>=<dimension>]...
                  [--dim <dimension>=<level>]... [--known <dimension>=<text>]...
                  [--unknown <dimension>=<text>]... [--confidence <dimension>=<0..1>]...
  crw pabcd scan --help

Notes:
  --session is required; there is no latest-session fallback for a mutating command.
  --cwd matters when the answer ledger lives outside the process cwd:
  answers are read from <cwd>/.crw/interviews/<session>.jsonl.
  --derive folds captured answers in; --map attributes a questionId to a dimension.
  --derive is what makes a dimension count for I->P readiness: the gate re-reads the
  ledger and requires an asked+answered+mapped question per dimension. --known records
  a fact but lends no provenance, and --dim cannot set 'max'.`
