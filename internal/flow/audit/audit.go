// Package audit is the audit of the reviews behind the acceptances of a flow run (CRW-1206, F13). A run accepts a node on the
// word of a reviewer call: the verdict it returned, which the run recorded as a result digest, with the thread and the turn the
// call ended on. The audit does not trust the record. It reads the reviewer's persistent thread from the host - thread/turns/list
// with itemsView full, and thread/items/list for a turn the list did not return in full - and checks that
//
//   - the journal recorded a turn id and a thread for the call, and the call ended completed;
//   - the host lists that turn, and it is completed there;
//   - the digest of the last agentMessage of the turn is the digest of the result the journal recorded;
//   - the packet the thread was sent names the head and the criteria digest the acceptance stands on;
//   - the pair of the executor that wrote the head was observed (the thread read back its model and effort), so the independence of
//     the reviewer is something the record can show.
//
// The result of one acceptance is ok, mismatch or unobserved. unobserved is never a pass: a host that does not list the thread, or
// does not return its items, proves nothing, and neither does a record without the pair of the executor. Only an ok result lets
// the run archive the reviewer thread.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The results of an audit, of one check and of one acceptance.
const (
	OK         = "ok"
	Mismatch   = "mismatch"
	Unobserved = "unobserved"
)

// Op is the op of the rows that hold audit results: the row of crw.audit in a run, and the row `crw flow audit` appends.
const Op = "crw.audit"

// Reader reads the turns of a thread from the host: thread/turns/list with itemsView full, and thread/items/list for a turn the
// list returned in summary.
type Reader interface {
	ReadTurns(ctx context.Context, threadID string, turnIDs []string) ([]appserver.TurnReading, error)
}

// BusReader reads through the bus of an App Server client.
type BusReader struct{ Bus *appserver.Bus }

// ReadTurns implements Reader.
func (b BusReader) ReadTurns(ctx context.Context, threadID string, turnIDs []string) ([]appserver.TurnReading, error) {
	return b.Bus.ReconcileTurns(ctx, threadID, turnIDs)
}

// Subject is an acceptance of a node on the agent path, with the call of the reviewer whose verdict it rests on.
type Subject struct {
	Plan         string `json:"plan"`
	Milestone    string `json:"milestone"`
	Node         string `json:"node"`
	Head         string `json:"head"`
	AcceptanceID string `json:"acceptance_id"`
	// CriteriaDigest is the digest of the criteria set the verdict was given against.
	CriteriaDigest string `json:"criteria_digest"`
	ReviewerCall   string `json:"reviewer_call"`
}

// Check is one thing an audit looked at and what it found.
type Check struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// Result is the audit of one acceptance.
type Result struct {
	Subject
	State  string  `json:"state"`
	Reason string  `json:"reason"`
	Thread string  `json:"thread"`
	Turn   string  `json:"turn"`
	Checks []Check `json:"checks"`
}

// The names of the checks.
const (
	CheckRecorded = "recorded"
	CheckExecutor = "executor_pair"
	CheckHost     = "host_read"
	CheckStatus   = "turn_status"
	CheckDigest   = "result_digest"
	CheckHead     = "packet_head"
	CheckCriteria = "packet_criteria"
)

// Subjects lists the acceptances of the agent path in the journal: the newest acceptance of each node and head, with the verified
// review it rests on. An acceptance whose review row cannot be found is a subject with no reviewer call, which Audit reports as a
// mismatch: an acceptance with no review behind it is not one the audit can vouch for.
func Subjects(events []engine.Event) []Subject {
	var out []Subject
	seen := map[string]int{}
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != "crw.accept" || ev.Event != engine.EvCompleted {
			continue
		}
		s := Subject{Node: str(ev.Data, "node"), Head: str(ev.Data, "head"), AcceptanceID: str(ev.Data, "acceptance_id"), Plan: str(ev.Data, "plan")}
		key := s.Node + "\x00" + s.Head
		if i, ok := seen[key]; ok {
			out[i] = s
			continue
		}
		seen[key] = len(out)
		out = append(out, s)
	}
	for i := range out {
		s := &out[i]
		for _, ev := range events {
			if ev.Kind != engine.KindCall || ev.Event != engine.EvCompleted {
				continue
			}
			switch {
			case ev.Op == "crw.release" && str(ev.Data, "node") == s.Node && str(ev.Data, "milestone") != "":
				s.Milestone = str(ev.Data, "milestone")
			case ev.Op == "crw.review" && str(ev.Data, "node") == s.Node && str(ev.Data, "head") == s.Head && str(ev.Data, "state") == "verdict" && str(ev.Data, "verdict") == "verified":
				s.ReviewerCall, s.CriteriaDigest = str(ev.Data, "call"), str(ev.Data, "criteria_digest")
			}
		}
	}
	return out
}

// Audit audits one acceptance against the journal and the host. A nil reader is a host that does not answer.
func Audit(ctx context.Context, reader Reader, events []engine.Event, s Subject) Result {
	res := Result{Subject: s, Checks: []Check{}}
	add := func(name, result, detail string) {
		res.Checks = append(res.Checks, Check{Name: name, Result: result, Detail: detail})
	}

	call := callOf(events, s.ReviewerCall)
	switch {
	case s.ReviewerCall == "":
		add(CheckRecorded, Mismatch, "no verified review of the head is recorded: the acceptance rests on no reviewer call")
	case call.terminal != engine.EvCompleted:
		add(CheckRecorded, Mismatch, fmt.Sprintf("the reviewer call %s did not end completed (%q)", s.ReviewerCall, call.terminal))
	case call.thread == "" || len(call.turns) == 0:
		add(CheckRecorded, Mismatch, fmt.Sprintf("the reviewer call %s recorded no thread or no turn id", s.ReviewerCall))
	case call.digest == "":
		add(CheckRecorded, Mismatch, fmt.Sprintf("the reviewer call %s recorded no result digest", s.ReviewerCall))
	default:
		add(CheckRecorded, OK, "")
		res.Thread, res.Turn = call.thread, call.turns[len(call.turns)-1]
	}
	if pair, detail := executorPair(events, s.Node); pair {
		add(CheckExecutor, OK, "")
	} else {
		add(CheckExecutor, Unobserved, detail)
	}
	if res.Checks[0].Result == OK {
		hostChecks(ctx, reader, s, call, add)
	}
	res.State, res.Reason = summarize(res.Checks)
	return res
}

// hostChecks reads the thread of the reviewer from the host and compares it with the journal.
func hostChecks(ctx context.Context, reader Reader, s Subject, call callRecord, add func(name, result, detail string)) {
	if reader == nil {
		add(CheckHost, Unobserved, "no host reader is configured")
		return
	}
	first, last := call.turns[0], call.turns[len(call.turns)-1]
	ids := []string{first}
	if last != first {
		ids = append(ids, last)
	}
	readings, err := reader.ReadTurns(ctx, call.thread, ids)
	if err != nil {
		add(CheckHost, Unobserved, "the host did not answer: "+err.Error())
		return
	}
	if len(readings) != len(ids) {
		add(CheckHost, Unobserved, fmt.Sprintf("the host answered %d readings for %d turns", len(readings), len(ids)))
		return
	}
	for i, r := range readings {
		switch {
		case !r.Found:
			add(CheckHost, Unobserved, fmt.Sprintf("the host lists no turn %s on thread %s", ids[i], call.thread))
			return
		case !r.Readable:
			add(CheckHost, Unobserved, fmt.Sprintf("the items of turn %s could not be read: %v", ids[i], r.ReadError))
			return
		}
	}
	add(CheckHost, OK, "")
	firstReading, lastReading := readings[0], readings[len(readings)-1]
	if lastReading.Status != "completed" {
		add(CheckStatus, Mismatch, fmt.Sprintf("turn %s is %q on the host", last, lastReading.Status))
	} else {
		add(CheckStatus, OK, "")
	}
	text, ok := lastAgentMessage(lastReading.Items)
	if !ok {
		add(CheckDigest, Mismatch, fmt.Sprintf("turn %s holds no agentMessage", last))
	} else if got, err := resultDigest(text); err != nil {
		add(CheckDigest, Mismatch, "the last agentMessage is not the one JSON document the journal's result is: "+err.Error())
	} else if got != call.digest {
		add(CheckDigest, Mismatch, fmt.Sprintf("the last agentMessage has digest %s, the journal recorded %s", got, call.digest))
	} else {
		add(CheckDigest, OK, "")
	}
	packet := userText(firstReading.Items)
	head, headOK := packetValue(packet, headLine)
	switch {
	case !headOK:
		add(CheckHead, Mismatch, "the packet names no head")
	case head != s.Head:
		add(CheckHead, Mismatch, fmt.Sprintf("the packet names head %s, the acceptance stands on %s", head, s.Head))
	default:
		add(CheckHead, OK, "")
	}
	criteria, critOK := packetValue(packet, criteriaLine)
	switch {
	case !critOK:
		add(CheckCriteria, Mismatch, "the packet names no criteria digest")
	case criteria != s.CriteriaDigest:
		add(CheckCriteria, Mismatch, fmt.Sprintf("the packet names criteria digest %s, the acceptance stands on %s", criteria, s.CriteriaDigest))
	default:
		add(CheckCriteria, OK, "")
	}
}

// summarize is the result of an acceptance: mismatch when any check found one, else unobserved when any could not look, else ok.
func summarize(checks []Check) (state, reason string) {
	state = OK
	for _, c := range checks {
		switch c.Result {
		case Mismatch:
			return Mismatch, c.Name + ": " + c.Detail
		case Unobserved:
			if state == OK {
				state, reason = Unobserved, c.Name+": "+c.Detail
			}
		}
	}
	return state, reason
}

// The lines of a reviewer packet that name what the acceptance stands on (workflows/lib/packets.js writes them).
var (
	headLine     = regexp.MustCompile(`(?m)^HEAD: (\S+)\s*$`)
	criteriaLine = regexp.MustCompile(`(?m)^CRITERIA DIGEST: (\S+)\s*$`)
)

func packetValue(packet string, re *regexp.Regexp) (string, bool) {
	m := re.FindStringSubmatch(packet)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// callRecord is what the journal recorded of a call: where it ran, the turns it ran and the digest of its result.
type callRecord struct {
	terminal string
	thread   string
	turns    []string
	digest   string
}

func callOf(events []engine.Event, id string) callRecord {
	var c callRecord
	if id == "" {
		return c
	}
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.CallID != id || ev.Op != "agent" {
			continue
		}
		switch ev.Event {
		case engine.EvPrepared:
			// a new attempt of the call starts over
			c = callRecord{}
		case engine.EvThreadStarted, engine.EvForkStarted:
			c.thread = str(ev.Data, "thread_id")
		case engine.EvTurnStarted:
			if t := str(ev.Data, "turn_id"); t != "" {
				c.turns = append(c.turns, t)
			}
		case engine.EvCompleted, engine.EvNullModel, engine.EvHeld, engine.EvRefused, engine.EvOutcomeUnknown, engine.EvSuperseded:
			c.terminal = ev.Event
			if ev.Event == engine.EvCompleted {
				c.digest = str(ev.Data, "result_digest")
			}
		}
	}
	return c
}

// executorPair reports whether the pair of every executor call of the node was observed: the thread read back its model and
// effort (the row of the thread or the fork it started on, or of the call it continues).
func executorPair(events []engine.Event, node string) (bool, string) {
	started := map[string]bool{}
	resumeOf := map[string]string{}
	var calls []string
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != "agent" {
			continue
		}
		switch ev.Event {
		case engine.EvPrepared:
			role := str(ev.Data, "role")
			if (role == "executor" || role == "executor-t1") && str(ev.Data, "node") == node {
				calls = append(calls, ev.CallID)
			}
			if r := str(ev.Data, "resume_of"); r != "" {
				resumeOf[ev.CallID] = r
			}
		case engine.EvThreadStarted, engine.EvForkStarted:
			if str(ev.Data, "model") != "" && str(ev.Data, "effort") != "" {
				started[ev.CallID] = true
			}
		}
	}
	if len(calls) == 0 {
		return false, fmt.Sprintf("the journal records no executor call of node %s", node)
	}
	for _, id := range calls {
		root := id
		for hops := 0; !started[root] && resumeOf[root] != "" && hops < 64; hops++ {
			root = resumeOf[root]
		}
		if !started[root] {
			return false, fmt.Sprintf("the thread of executor call %s recorded no model and effort", id)
		}
	}
	return true, ""
}

// lastAgentMessage is the text of the last agentMessage item that is not commentary.
func lastAgentMessage(items []json.RawMessage) (string, bool) {
	text, found := "", false
	for _, raw := range items {
		var it struct {
			Type  string  `json:"type"`
			Text  string  `json:"text"`
			Phase *string `json:"phase"`
		}
		if json.Unmarshal(raw, &it) != nil || it.Type != "agentMessage" {
			continue
		}
		if it.Phase != nil && *it.Phase == "commentary" {
			continue
		}
		text, found = it.Text, true
	}
	return text, found
}

// userText is the text of the userMessage items of a turn.
func userText(items []json.RawMessage) string {
	var b strings.Builder
	for _, raw := range items {
		var it struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &it) != nil || it.Type != "userMessage" {
			continue
		}
		for _, c := range it.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// resultDigest is the digest the engine records for the result of a call (the SHA-256 of the canonical JSON of the one document
// the last message holds): the text of a message that is not one JSON document has none.
func resultDigest(text string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	var plain any
	if err := dec.Decode(&plain); err != nil {
		return "", fmt.Errorf("not JSON: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("more than one JSON value")
	}
	canon, err := json.Marshal(plain)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// Summary counts the results by state.
type Summary struct {
	OK         int `json:"ok"`
	Mismatch   int `json:"mismatch"`
	Unobserved int `json:"unobserved"`
}

// Summarize counts results.
func Summarize(results []Result) Summary {
	var s Summary
	for _, r := range results {
		switch r.State {
		case OK:
			s.OK++
		case Mismatch:
			s.Mismatch++
		default:
			s.Unobserved++
		}
	}
	return s
}

// RowData is the data of a crw.audit row: the results and their count by state, in the form the store returns a row's data (the
// JSON round trip). The result member is the JSON the row's result holds, as every op row has one.
func RowData(milestone string, results []Result) map[string]any {
	sum := Summarize(results)
	if results == nil {
		results = []Result{}
	}
	body := map[string]any{"milestone": milestone, "results": results, "ok": sum.OK, "mismatch": sum.Mismatch, "unobserved": sum.Unobserved}
	raw, _ := json.Marshal(body)
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	data["result"] = string(raw)
	return data
}

// Passed lists the acceptances that have an audit pass row in the journal, by acceptance id: an acceptance passes while the newest
// row that audited it says ok.
func Passed(events []engine.Event) map[string]Result {
	state := map[string]Result{}
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != Op || ev.Event != engine.EvCompleted {
			continue
		}
		list, _ := ev.Data["results"].([]any)
		for _, item := range list {
			raw, err := json.Marshal(item)
			if err != nil {
				continue
			}
			var r Result
			if json.Unmarshal(raw, &r) != nil || r.AcceptanceID == "" {
				continue
			}
			state[r.AcceptanceID] = r
		}
	}
	out := map[string]Result{}
	for id, r := range state {
		if r.State == OK {
			out[id] = r
		}
	}
	return out
}
