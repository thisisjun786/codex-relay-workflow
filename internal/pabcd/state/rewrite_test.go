package state

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rewriteFile(list string) string { return `{"phase":"B","unverifiedSubagents":` + list + `}` }

func rewriteRecord(fields string) string {
	return `{"agentId":"a","recordedAt":"t"` + fields + `}`
}

func rewriteMany(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = `{"agentId":"a` + strconv.Itoa(i) + `","recordedAt":"t"}`
	}
	return "[" + strings.Join(items, ",") + "]"
}

// rewriteVerdict reads file as ReadStateStrict does and asks whether writing the rebuilt records back would keep the stored ones.
func rewriteVerdict(t *testing.T, file string) bool {
	t.Helper()
	s, unreadable := restore("s1", []byte(file), time.Now())
	if unreadable {
		t.Fatalf("the file does not read as a state: %s", file)
	}
	return RewriteKeepsUnverified([]byte(file), s.UnverifiedSubagents)
}

func TestRewriteKeepsUnverifiedJudgesEveryHandledField(t *testing.T) {
	receipt := func(n int) string { return `,"receiptClaimed":"` + strings.Repeat("r", n) + `"` }
	for _, c := range []struct {
		name, file string
		keeps      bool
	}{
		{"no list", `{"phase":"B"}`, true},
		{"null list", rewriteFile("null"), true},
		{"empty list", rewriteFile("[]"), true},
		{"a record of the two required fields", rewriteFile("[" + rewriteRecord("") + "]"), true},
		{"a full record", rewriteFile("[" + rewriteRecord(`,"turnId":"t1","agentType":"worker","attempts":3,"receiptClaimed":"x","resolvable":false`) + "]"), true},
		{"a receipt of 256 units", rewriteFile("[" + rewriteRecord(receipt(256)) + "]"), true},
		{"a receipt of 257 units", rewriteFile("[" + rewriteRecord(receipt(257)) + "]"), false},
		{"a receipt cut inside an astral character", rewriteFile("[" + rewriteRecord(`,"receiptClaimed":"`+strings.Repeat("r", 255)+"\U0001F600b"+`"`) + "]"), false},
		{"a receipt that ends on an astral character", rewriteFile("[" + rewriteRecord(`,"receiptClaimed":"`+strings.Repeat("r", 254)+"\U0001F600"+`"`) + "]"), true},
		{"a receipt that is not text", rewriteFile("[" + rewriteRecord(`,"receiptClaimed":5`) + "]"), false},
		{"a null receipt", rewriteFile("[" + rewriteRecord(`,"receiptClaimed":null`) + "]"), true},
		{"a turn id that is not text", rewriteFile("[" + rewriteRecord(`,"turnId":5`) + "]"), false},
		{"an agent type that is not text", rewriteFile("[" + rewriteRecord(`,"agentType":5`) + "]"), false},
		{"a null agent type", rewriteFile("[" + rewriteRecord(`,"agentType":null`) + "]"), true},
		{"attempts 3", rewriteFile("[" + rewriteRecord(`,"attempts":3`) + "]"), true},
		{"attempts 3.0", rewriteFile("[" + rewriteRecord(`,"attempts":3.0`) + "]"), true},
		{"attempts 1e2", rewriteFile("[" + rewriteRecord(`,"attempts":1e2`) + "]"), true},
		{"attempts -0", rewriteFile("[" + rewriteRecord(`,"attempts":-0`) + "]"), true},
		{"null attempts", rewriteFile("[" + rewriteRecord(`,"attempts":null`) + "]"), true},
		{"attempts 3.5", rewriteFile("[" + rewriteRecord(`,"attempts":3.5`) + "]"), false},
		{"attempts as text", rewriteFile("[" + rewriteRecord(`,"attempts":"3"`) + "]"), false},
		{"attempts beyond a float64", rewriteFile("[" + rewriteRecord(`,"attempts":1e400`) + "]"), false},
		{"attempts a float64 cannot hold", rewriteFile("[" + rewriteRecord(`,"attempts":9007199254740993`) + "]"), false},
		{"attempts below a float64", rewriteFile("[" + rewriteRecord(`,"attempts":1e-400`) + "]"), false},
		{"attempts as a very long token", rewriteFile("[" + rewriteRecord(`,"attempts":0.`+strings.Repeat("0", 70)) + "]"), false},
		{"attempts printed in the shortest form", rewriteFile("[" + rewriteRecord(`,"attempts":1000000000000000100`) + "]"), true},
		{"attempts printed with an exponent", rewriteFile("[" + rewriteRecord(`,"attempts":1e+23`) + "]"), true},
		{"the largest attempts a double holds", rewriteFile("[" + rewriteRecord(`,"attempts":1.7976931348623157e+308`) + "]"), true},
		{"attempts the writer prints as another number", rewriteFile("[" + rewriteRecord(`,"attempts":1000000000000000128`) + "]"), false},
		{"attempts the writer prints with an exponent", rewriteFile("[" + rewriteRecord(`,"attempts":99999999999999991611392`) + "]"), false},
		{"attempts with a very large exponent", rewriteFile("[" + rewriteRecord(`,"attempts":1e1000000`) + "]"), false},
		{"resolvable false", rewriteFile("[" + rewriteRecord(`,"resolvable":false`) + "]"), true},
		{"null resolvable", rewriteFile("[" + rewriteRecord(`,"resolvable":null`) + "]"), true},
		{"resolvable as text", rewriteFile("[" + rewriteRecord(`,"resolvable":"no"`) + "]"), false},
		{"resolvable as a number", rewriteFile("[" + rewriteRecord(`,"resolvable":0`) + "]"), false},
		{"a key the reader does not handle", rewriteFile("[" + rewriteRecord(`,"note":"x"`) + "]"), true},
		{"a record without recordedAt", rewriteFile(`[{"agentId":"a"}]`), false},
		{"a record without agentId", rewriteFile(`[{"recordedAt":"t"}]`), false},
		{"an entry that is no record", rewriteFile("[1]"), false},
		{"a null entry", rewriteFile("[null]"), false},
		{"a good record before a dropped one", rewriteFile("[" + rewriteRecord("") + ",1]"), false},
		{"the most records the reader keeps", rewriteFile(rewriteMany(MaxUnverifiedSubagents)), true},
		{"one record more than the reader keeps", rewriteFile(rewriteMany(MaxUnverifiedSubagents + 1)), false},
		{"a list that is an object", rewriteFile("{}"), false},
		{"a list that is text", rewriteFile(`"x"`), false},
		{"a list that is false", rewriteFile("false"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteVerdict(t, c.file); got != c.keeps {
				t.Errorf("RewriteKeepsUnverified = %v, want %v for %.200s", got, c.keeps, c.file)
			}
		})
	}
}

func TestRewriteKeepsUnverifiedAgainstWhatTheCallerHolds(t *testing.T) {
	one := []UnverifiedSubagent{{AgentID: "a", RecordedAt: "t", AgentType: "worker", Resolvable: true}}
	for _, c := range []struct {
		name, file string
		kept       []UnverifiedSubagent
		keeps      bool
	}{
		{"a list the caller holds more of", rewriteFile("[]"), one, false},
		{"an absent list the caller holds records for", `{"phase":"B"}`, one, false},
		{"a list the caller holds fewer of", rewriteFile("[" + rewriteRecord("") + "]"), nil, false},
		{"a record the caller changed", rewriteFile("[" + rewriteRecord(`,"agentType":"explorer"`) + "]"), one, false},
		{"text that is no JSON", "not json", nil, false},
		{"a JSON array", "[]", nil, false},
		{"two JSON values", rewriteFile("[]") + " {}", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RewriteKeepsUnverified([]byte(c.file), c.kept); got != c.keeps {
				t.Errorf("RewriteKeepsUnverified = %v, want %v", got, c.keeps)
			}
		})
	}
}

// What WriteState prints for any attempts the reader can hold is kept: the guard never refuses the writer's own output.
func TestRewriteKeepsWhatTheWriterWrites(t *testing.T) {
	for _, attempts := range []float64{0, 3, 1e15, 9007199254740992, 1000000000000000128, 1e23, math.MaxFloat64} {
		s := DefaultState("s1", "")
		s.Phase = PhaseB
		s.UnverifiedSubagents = []UnverifiedSubagent{{AgentID: "a", RecordedAt: "t", Attempts: attempts, Resolvable: true}}
		raw, err := Encode(s)
		if err != nil {
			t.Fatal(err)
		}
		back, unreadable := restore("s1", raw, time.Now())
		if unreadable || !RewriteKeepsUnverified(raw, back.UnverifiedSubagents) || back.UnverifiedSubagents[0].Attempts != attempts {
			t.Errorf("attempts %v: the writer's own record is refused or changed: %s", attempts, raw)
		}
	}
}
