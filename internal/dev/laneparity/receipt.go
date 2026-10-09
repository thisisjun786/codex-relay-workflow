//go:build dev

package laneparity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Receipt is the record of one hook firing: which run, plugin and build fired which leg for which
// session, turn, tool call, agent and skills, and what came back. A cell passes only on receipts
// of this run, of the plugin and build under test, that name the leg's own event and the payload's
// own subject.
type Receipt struct {
	Run     string `json:"run"`
	Plugin  string `json:"plugin"` // PluginDigest of the root that was fired
	Binary  string `json:"binary"` // sha256 of the executable the declared command starts (empty when it names none)
	Fixture string `json:"fixture"`
	Step    int    `json:"step"`
	Leg     string `json:"leg"`
	Event   string `json:"event"`
	Command string `json:"command"`

	Session  string   `json:"session,omitempty"`
	Turn     string   `json:"turn,omitempty"`
	ToolUse  string   `json:"toolUse,omitempty"`
	ToolName string   `json:"toolName,omitempty"`
	Agent    string   `json:"agent,omitempty"`
	Skills   []string `json:"skills,omitempty"`

	Exit         int    `json:"exit"`
	StdoutSHA256 string `json:"stdoutSha256"`
}

// Want is what a step of a fixture must have left as a receipt: the leg and event it is fired
// under, the subject its payload names, and the skills its expected answer names.
type Want struct {
	Fixture string
	Step    int
	Leg     string
	Event   string
	Subject Subject
}

// Subject is whom a firing is about: taken from the payload delivered and the answer expected.
type Subject struct {
	Session, Turn, ToolUse, ToolName, Agent string
	Skills                                  []string
}

// NewRunID is a fresh identifier of one harness run.
func NewRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SubjectOf reads the subject out of a hook payload and the text of the answer expected for it.
func SubjectOf(payload json.RawMessage, answer string) Subject {
	var p struct {
		Session  string          `json:"session_id"`
		Turn     string          `json:"turn_id"`
		ToolUse  string          `json:"tool_use_id"`
		ToolName string          `json:"tool_name"`
		Agent    json.RawMessage `json:"agent_id"`
	}
	text := string(payload)
	var s string
	if json.Unmarshal(payload, &s) == nil { // a stdin given as a JSON text
		text = s
	}
	_ = json.Unmarshal([]byte(text), &p)
	sub := Subject{Session: p.Session, Turn: p.Turn, ToolUse: p.ToolUse, ToolName: p.ToolName, Skills: SkillsIn(answer)}
	var agent string
	if json.Unmarshal(p.Agent, &agent) == nil {
		sub.Agent = agent
	}
	return sub
}

var skillRef = regexp.MustCompile(`crw:crw-[a-z0-9]+(?:-[a-z0-9]+)*`)

// SkillsIn lists, sorted and without repeats, the skills a text names (the crw:crw-<name> form the
// directives and the attach answer use).
func SkillsIn(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range skillRef.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return out
}

// StdoutDigest is the sha256 of a hook's stdout, as a receipt carries it.
func StdoutDigest(stdout string) string {
	sum := sha256.Sum256([]byte(stdout))
	return hex.EncodeToString(sum[:])
}

// VerifyReceipts returns the problems that keep the receipts from proving the wanted firings: for
// each wanted step exactly one receipt of this run, of the plugin and build under test, for the same
// leg, event and subject; and no receipt that no wanted step explains.
func VerifyReceipts(run, plugin, binary string, want []Want, got []Receipt) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	type key struct {
		fixture string
		step    int
	}
	byStep := map[key][]Receipt{}
	for _, r := range got {
		byStep[key{r.Fixture, r.Step}] = append(byStep[key{r.Fixture, r.Step}], r)
	}
	wanted := map[key]bool{}
	for _, w := range want {
		k := key{w.Fixture, w.Step}
		wanted[k] = true
		where := fmt.Sprintf("%s step %d (%s)", w.Fixture, w.Step, w.Leg)
		rs := byStep[k]
		switch len(rs) {
		case 0:
			add("%s: no receipt: the hook did not fire or left no record", where)
			continue
		case 1:
		default:
			add("%s: %d receipts for one firing", where, len(rs))
			continue
		}
		r := rs[0]
		if r.Run != run {
			add("%s: receipt of run %s, this run is %s", where, r.Run, run)
		}
		if r.Plugin != plugin {
			add("%s: receipt names another plugin root (%s), the fired one is %s", where, short(r.Plugin), short(plugin))
		}
		if r.Binary == "" {
			add("%s: receipt names no crw build: the declared command starts no executable the harness can identify", where)
		} else if r.Binary != binary {
			add("%s: receipt names another crw build (%s), the fired one is %s", where, short(r.Binary), short(binary))
		}
		if r.Leg != w.Leg {
			add("%s: receipt is for leg %s", where, r.Leg)
		}
		if r.Event != w.Event {
			add("%s: receipt is for event %s, expected %s", where, r.Event, w.Event)
		}
		check := func(name, have, need string) {
			if have != need {
				add("%s: receipt %s is %q, the payload says %q", where, name, have, need)
			}
		}
		check("session", r.Session, w.Subject.Session)
		check("turn", r.Turn, w.Subject.Turn)
		check("tool call", r.ToolUse, w.Subject.ToolUse)
		check("tool", r.ToolName, w.Subject.ToolName)
		check("agent", r.Agent, w.Subject.Agent)
		if a, b := slices.Clone(r.Skills), slices.Clone(w.Subject.Skills); !slices.Equal(sorted(a), sorted(b)) {
			add("%s: receipt skills %v, expected %v", where, r.Skills, w.Subject.Skills)
		}
	}
	for k, rs := range byStep {
		if !wanted[k] {
			add("%s step %d: %d receipt(s) no wanted firing explains", k.fixture, k.step, len(rs))
		}
	}
	slices.Sort(problems)
	return problems
}

func sorted(s []string) []string { slices.Sort(s); return s }

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return strings.TrimSpace(digest)
}
