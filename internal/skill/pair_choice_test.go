package skill

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pairTestTime = "2026-01-01T01:00:00Z"

func pairTestRequest() map[string]any {
	return map[string]any{"bundle": "flexible", "as_of": pairTestTime, "working": map[string]int{"sonnet": 0, "sol": 0}, "lines": map[string]int{"sonnet": 0, "sol": 0}}
}
func pairTestSnapshot(claude, openai float64) map[string]any {
	side := func(used float64) map[string]any {
		state := "available"
		if used == 100 {
			state = "exhausted"
		}
		return map[string]any{"state": state, "observed_at": pairTestTime, "windows": []any{map[string]any{"name": "five-hour", "utilization": used, "reset_at": "2026-01-01T06:00:00Z"}}}
	}
	return map[string]any{"schema": "crw-pair-quota/1", "claude": side(claude), "openai": side(openai)}
}
func pairTestRun(t *testing.T, req map[string]any, snapshot any) (map[string]any, int) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"pair-choice", "choose"}
	if snapshot != nil {
		p := filepath.Join(t.TempDir(), "quota.json")
		b, e := json.Marshal(snapshot)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
		args = append(args, "--snapshot", p)
	}
	var out, stderr bytes.Buffer
	code := Run(args, bytes.NewReader(raw), &out, &stderr)
	var got map[string]any
	if e := json.Unmarshal(out.Bytes(), &got); e != nil {
		t.Fatalf("exit=%d stderr=%s stdout=%s: %v", code, stderr.String(), out.String(), e)
	}
	return got, code
}
func pairTestRule(t *testing.T, got map[string]any, rule string) {
	t.Helper()
	for _, s := range got["rule"].([]any) {
		if s == rule {
			return
		}
	}
	t.Errorf("rule lacks %s: %v", rule, got)
}
func TestPairChoiceRules(t *testing.T) {
	cases := []struct {
		name               string
		used               [2]float64
		change             func(map[string]any)
		pair, source, rule string
		exit               int
	}{
		{name: "hysteresis equality", used: [2]float64{60, 50}, pair: "Sonnet", source: "quota", rule: "hysteresis"},
		{name: "switch", used: [2]float64{61, 50}, pair: "SOL", source: "quota", rule: "headroom_switch"},
		{name: "reverse switch", used: [2]float64{20, 80}, change: func(r map[string]any) { r["tie"] = "SOL" }, pair: "Sonnet", source: "quota", rule: "headroom_switch"},
		{name: "window cap", used: [2]float64{80, 10}, change: func(r map[string]any) {
			r["releases"] = []any{map[string]any{"at": pairTestTime, "pair": "SOL", "bundle": "flexible"}}
		}, pair: "Sonnet", source: "quota", rule: "window_cap"},
		{name: "exhaustion ignores cap", used: [2]float64{100, 10}, change: func(r map[string]any) {
			r["releases"] = []any{map[string]any{"at": pairTestTime, "pair": "SOL", "bundle": "flexible"}}
		}, pair: "SOL", source: "quota", rule: "provider_exhausted"},
		{name: "both exhausted", used: [2]float64{100, 100}, source: "quota", rule: "both_unavailable", exit: 1},
		{name: "fixed keep", used: [2]float64{90, 10}, change: func(r map[string]any) { r["bundle"] = "Sonnet fixed" }, pair: "Sonnet", source: "table", rule: "fixed_bundle"},
		{name: "fixed exhaustion move", used: [2]float64{100, 10}, change: func(r map[string]any) { r["bundle"] = "Sonnet fixed" }, pair: "SOL", source: "quota", rule: "provider_exhausted"},
		{name: "project share above60", used: [2]float64{10, 90}, change: func(r map[string]any) { r["lines"] = map[string]int{"sonnet": 7, "sol": 3} }, pair: "SOL", source: "quota", rule: "project_balance"},
		{name: "project share exactly60", used: [2]float64{10, 90}, change: func(r map[string]any) {
			r["lines"] = map[string]int{"sonnet": 6, "sol": 4}
			r["working"] = map[string]int{"sonnet": 0, "sol": 1}
		}, pair: "Sonnet", source: "quota", rule: "hysteresis"},
		{name: "guards conflict", used: [2]float64{10, 90}, change: func(r map[string]any) {
			r["lines"] = map[string]int{"sonnet": 7, "sol": 3}
			r["releases"] = []any{map[string]any{"at": pairTestTime, "pair": "SOL", "bundle": "flexible"}}
		}, source: "quota", rule: "guards_conflict", exit: 1},
		{name: "legacy flexible keep", used: [2]float64{90, 10}, change: func(r map[string]any) { r["recorded_pair"] = "Sonnet"; r["source"] = "issue body" }, pair: "Sonnet", source: "issue body", rule: "recorded_choice"},
		{name: "user holds on exhaustion", used: [2]float64{100, 10}, change: func(r map[string]any) { r["recorded_pair"] = "Sonnet"; r["source"] = "user choice" }, source: "user choice", rule: "recorded_unavailable", exit: 1},
		{name: "manual unusable flexible", used: [2]float64{20, 20}, change: func(r map[string]any) { r["tie"] = "SOL"; r["unusable"] = map[string]string{"sol": "policy refusal"} }, pair: "Sonnet", source: "table", rule: "provider_unusable"},
		{name: "fixed rows ignored", used: [2]float64{80, 10}, change: func(r map[string]any) {
			r["releases"] = []any{map[string]any{"at": pairTestTime, "pair": "SOL", "bundle": "SOL fixed"}}
		}, pair: "SOL", source: "quota", rule: "headroom_switch"},
		{name: "old window ignored", used: [2]float64{80, 10}, change: func(r map[string]any) {
			r["releases"] = []any{map[string]any{"at": "2025-12-31T18:00:00Z", "pair": "SOL", "bundle": "flexible"}}
		}, pair: "SOL", source: "quota", rule: "headroom_switch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := pairTestRequest()
			if c.change != nil {
				c.change(r)
			}
			g, code := pairTestRun(t, r, pairTestSnapshot(c.used[0], c.used[1]))
			if code != c.exit || g["source"] != c.source {
				t.Fatalf("exit=%d got=%v", code, g)
			}
			if c.pair == "" {
				if g["pair"] != nil {
					t.Errorf("want hold: %v", g)
				}
			} else if g["pair"] != c.pair {
				t.Errorf("want %s: %v", c.pair, g)
			}
			pairTestRule(t, g, c.rule)
		})
	}
}
func TestPairChoiceFallbackAndCounts(t *testing.T) {
	for _, c := range []struct {
		name           string
		working, lines [2]int
		want           string
	}{
		{"tie", [2]int{0, 0}, [2]int{0, 0}, "Sonnet"},
		{"working", [2]int{2, 1}, [2]int{5, 5}, "SOL"},
		{"lines", [2]int{1, 1}, [2]int{5, 6}, "Sonnet"},
		{"project override", [2]int{0, 9}, [2]int{7, 3}, "SOL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := pairTestRequest()
			r["working"] = map[string]int{"sonnet": c.working[0], "sol": c.working[1]}
			r["lines"] = map[string]int{"sonnet": c.lines[0], "sol": c.lines[1]}
			g, code := pairTestRun(t, r, nil)
			q := g["quota"].(map[string]any)
			if code != 0 || g["pair"] != c.want || g["source"] != "table" || q["readable"] != false || q["reason"] != "no_snapshot_producer" {
				t.Fatalf("%d %v", code, g)
			}
		})
	}
	r := pairTestRequest()
	r["bundle"] = "undetermined"
	r["recorded_pair"] = "SOL"
	r["source"] = "issue body"
	r["classification_reason"] = "procedure unsettled"
	g, code := pairTestRun(t, r, nil)
	if code != 0 || g["pair"] != "SOL" || g["classification_reason"] != "procedure unsettled" {
		t.Fatal(code, g)
	}
	r = pairTestRequest()
	r["tie"] = "SOL"
	r["unusable"] = map[string]string{"sol": "policy refusal"}
	g, code = pairTestRun(t, r, nil)
	if code != 0 || g["pair"] != "Sonnet" || g["cause"] != "policy refusal" || g["date"] != "2026-01-01" {
		t.Fatal(code, g)
	}
	r["unusable"] = map[string]string{"sol": "policy refusal", "sonnet": "provider unavailable"}
	g, code = pairTestRun(t, r, nil)
	if code != 1 || g["pair"] != nil {
		t.Fatal(code, g)
	}
}
func TestPairChoiceUnreadableQuota(t *testing.T) {
	changes := []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing side", func(s map[string]any) { delete(s, "claude") }},
		{"unknown state", func(s map[string]any) { s["claude"].(map[string]any)["state"] = "unknown" }},
		{"stale", func(s map[string]any) { s["claude"].(map[string]any)["observed_at"] = "2026-01-01T00:29:59Z" }},
		{"future", func(s map[string]any) { s["claude"].(map[string]any)["observed_at"] = "2026-01-01T01:00:01Z" }},
		{"percent absent", func(s map[string]any) {
			delete(s["claude"].(map[string]any)["windows"].([]any)[0].(map[string]any), "utilization")
		}},
		{"percent out of range", func(s map[string]any) {
			s["claude"].(map[string]any)["windows"].([]any)[0].(map[string]any)["utilization"] = 101
		}},
		{"expired only", func(s map[string]any) {
			s["claude"].(map[string]any)["windows"].([]any)[0].(map[string]any)["reset_at"] = pairTestTime
		}},
		{"inconsistent state", func(s map[string]any) { s["claude"].(map[string]any)["state"] = "exhausted" }},
		{"unknown field", func(s map[string]any) { s["account"] = "synthetic" }},
		{"wrong schema", func(s map[string]any) { s["schema"] = "other" }},
	}
	for _, c := range changes {
		t.Run(c.name, func(t *testing.T) {
			s := pairTestSnapshot(80, 10)
			c.change(s)
			g, code := pairTestRun(t, pairTestRequest(), s)
			q := g["quota"].(map[string]any)
			if code != 0 || g["pair"] != "Sonnet" || g["source"] != "table" || q["readable"] != false || q["reason"] == "" {
				t.Fatal(code, g)
			}
		})
	}
}
func TestPairChoiceLimitingWindow(t *testing.T) {
	s := pairTestSnapshot(10, 10)
	s["claude"].(map[string]any)["windows"] = []any{
		map[string]any{"name": "short", "utilization": 5, "reset_at": "2026-01-01T06:00:00Z"},
		map[string]any{"name": "weekly", "utilization": 80, "reset_at": "2026-01-08T01:00:00Z"},
		map[string]any{"name": "expired", "utilization": 100, "reset_at": "2026-01-01T00:50:00Z"},
	}
	s["claude"].(map[string]any)["observed_at"] = "2026-01-01T00:30:00Z"
	g, code := pairTestRun(t, pairTestRequest(), s)
	if code != 0 || g["pair"] != "SOL" {
		t.Fatal(code, g)
	}
	head := g["quota"].(map[string]any)["headroom"].(map[string]any)
	if head["claude"] != float64(20) {
		t.Fatal(head)
	}
}
func TestPairChoiceCLI(t *testing.T) {
	for _, c := range []struct {
		args []string
		raw  string
		exit int
	}{
		{[]string{"pair-choice"}, "", 2}, {[]string{"pair-choice", "--help"}, "", 0},
		{[]string{"pair-choice", "choose", "--help"}, "", 0}, {[]string{"pair-choice", "choose", "--unknown"}, "", 2},
		{[]string{"pair-choice", "choose"}, `{}`, 2},
		{[]string{"pair-choice", "choose"}, `{"bundle":"flexible","as_of":"2026-01-01T01:00:00Z","lines":{"sonnet":-1}}`, 2},
		{[]string{"pair-choice", "choose"}, `{"bundle":"flexible","as_of":"2026-01-01T01:00:00Z","extra":1}`, 2},
		{[]string{"pair-choice", "choose"}, `null`, 2},
		{[]string{"pair-choice", "choose"}, "\xff", 2},
		{[]string{"pair-choice", "choose", "--snapshot", filepath.Join(t.TempDir(), "missing")}, `{"bundle":"flexible","as_of":"2026-01-01T01:00:00Z"}`, 0},
	} {
		var out, stderr bytes.Buffer
		code := Run(c.args, strings.NewReader(c.raw), &out, &stderr)
		if code != c.exit {
			t.Errorf("args=%v raw=%s exit%d stderr%s", c.args, c.raw, code, stderr.String())
		}
	}
}
