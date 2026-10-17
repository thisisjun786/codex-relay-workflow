package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

const findingsMissingVerified = `{"answer":"found it","evidence":["README.md"],"inferred":[],"unknown":[]}`
const findingsEndMissingVerified = `{"answer":"found it","evidence":["README.md"],"inferred":[],"unknown":[],"end_conditions":[]}`
const findingsValidDoc = `{"answer":"found it","evidence":["README.md"],"verified":["read README.md"],"inferred":[],"unknown":[]}`
const findingsEndValidDoc = `{"answer":"found it","evidence":["README.md"],"verified":["read README.md"],"inferred":[],"unknown":[],"end_conditions":[]}`

const header1611End = "import { FINDINGS_END } from 'crw:lib/schemas'\nexport const meta = { name: 't' }\n"

func loadBody(t *testing.T, hdr, body string) *script.Program {
	t.Helper()
	p, err := script.Load(script.Source{Name: "t.js", Text: []byte(hdr + body)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runProg(t *testing.T, h *harness, p *script.Program) *Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Run(ctx, p, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func turnRequests(f *Fake) []FakeRequest {
	var out []FakeRequest
	for _, r := range f.Requests() {
		if r.Kind == "turn" {
			out = append(out, r)
		}
	}
	return out
}

func repairs(reqs []FakeRequest) int {
	n := 0
	for _, r := range reqs {
		if r.Repair {
			n++
		}
	}
	return n
}

func completedData(t *testing.T, h *harness, label string) map[string]any {
	t.Helper()
	rows := rowsNamed(h.callRows(label), EvCompleted)
	if len(rows) != 1 {
		t.Fatalf("%d completed rows for %q", len(rows), label)
	}
	return rows[0].Data
}

func outputObject(t *testing.T, res *Result) map[string]any {
	t.Helper()
	if len(res.Calls) != 1 {
		t.Fatalf("%d calls", len(res.Calls))
	}
	var out map[string]any
	if err := json.Unmarshal(res.Calls[0].Output, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExplorer1611MissingVerifiedNoRepair(t *testing.T) {
	for name, tc := range map[string]struct{ missing, valid string }{
		"findings":     {findingsMissingVerified, findingsValidDoc},
		"findings_end": {findingsEndMissingVerified, findingsEndValidDoc},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Handler = func(FakeRequest) FakeReply { return FakeReply{Text: tc.missing} }
			body := `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS, label: 'exp' }); return r;`
			hdr := header
			if name == "findings_end" {
				body = `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS_END, label: 'exp' }); return r;`
				hdr = header1611End
			}
			res := runProg(t, h, loadBody(t, hdr, body))
			call := res.Calls[0]
			if call.Status != StatusCompleted || call.Meta.RepairUsed {
				t.Fatalf("call = %s repairUsed %v", call.Status, call.Meta.RepairUsed)
			}
			reqs := turnRequests(h.fake)
			if len(reqs) != 1 || repairs(reqs) != 0 {
				t.Fatalf("%d turn requests, %d repairs", len(reqs), repairs(reqs))
			}
			out := outputObject(t, res)
			v, present := out["verified"]
			if !present || v == nil {
				t.Fatalf("verified absent or null in %v", out)
			}
			if arr, ok := v.([]any); !ok || len(arr) != 0 {
				t.Fatalf("verified = %v", v)
			}
			if out["answer"] != "found it" || out["inferred"] == nil {
				t.Fatalf("output = %v", out)
			}
			data := completedData(t, h, "exp")
			if fmt.Sprintf("%v", data["normalizations"]) != "[explorer_verified_missing_to_empty]" {
				t.Fatalf("normalizations = %v", data["normalizations"])
			}
			if data["raw_text"] != tc.missing {
				t.Fatalf("raw_text = %v", data["raw_text"])
			}
			if data["result"] != string(call.Output) || data["result_digest"] != digestJSON(call.Output) {
				t.Fatalf("result %v digest %v", data["result"], data["result_digest"])
			}
			if data["repair_used"] != false {
				t.Fatalf("repair_used = %v", data["repair_used"])
			}
		})
	}
}

func TestExplorer1611OtherInvalidFieldsRepair(t *testing.T) {
	valid := findingsValidDoc
	validEnd := findingsEndValidDoc
	cases := map[string]string{
		"missing answer":          `{"evidence":["README.md"],"verified":[],"inferred":[],"unknown":[]}`,
		"missing answer+verified": `{"evidence":["README.md"],"inferred":[],"unknown":[]}`,
		"missing evidence":        `{"answer":"found it","verified":[],"inferred":[],"unknown":[]}`,
		"missing inferred":        `{"answer":"found it","evidence":["README.md"],"verified":[],"unknown":[]}`,
		"missing unknown":         `{"answer":"found it","evidence":["README.md"],"verified":[],"inferred":[]}`,
		"verified null":           `{"answer":"found it","evidence":["README.md"],"verified":null,"inferred":[],"unknown":[]}`,
		"verified string":         `{"answer":"found it","evidence":["README.md"],"verified":"x","inferred":[],"unknown":[]}`,
		"verified bad items":      `{"answer":"found it","evidence":["README.md"],"verified":[1],"inferred":[],"unknown":[]}`,
		"answer wrong type":       `{"answer":1,"evidence":["README.md"],"verified":[],"inferred":[],"unknown":[]}`,
		"unknown extra member":    `{"answer":"found it","evidence":["README.md"],"verified":[],"inferred":[],"unknown":[],"extra":1}`,
		"end missing conditions":  `{"answer":"found it","evidence":["README.md"],"verified":[],"inferred":[],"unknown":[]}`,
		"malformed":               `{"answer":`,
		"two values":              `{"answer":"a","evidence":[],"verified":[],"inferred":[],"unknown":[]} {}`,
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Handler = func(r FakeRequest) FakeReply {
				if r.Repair {
					return FakeReply{Text: valid}
				}
				return FakeReply{Text: invalid}
			}
			body := `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS, label: 'exp' }); return r;`
			hdr := header
			if name == "end missing conditions" {
				body = `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS_END, label: 'exp' }); return r;`
				hdr = header1611End
				h.fake.Handler = func(r FakeRequest) FakeReply {
					if r.Repair {
						return FakeReply{Text: validEnd}
					}
					return FakeReply{Text: invalid}
				}
			}
			res := runProg(t, h, loadBody(t, hdr, body))
			call := res.Calls[0]
			if call.Status != StatusCompleted || !call.Meta.RepairUsed {
				t.Fatalf("call = %s repairUsed %v", call.Status, call.Meta.RepairUsed)
			}
			reqs := turnRequests(h.fake)
			if len(reqs) != 2 || repairs(reqs) != 1 {
				t.Fatalf("%d turn requests, %d repairs", len(reqs), repairs(reqs))
			}
			out := outputObject(t, res)
			if arr, ok := out["verified"].([]any); !ok || len(arr) != 1 || arr[0] != "read README.md" {
				t.Fatalf("verified = %v", out["verified"])
			}
			data := completedData(t, h, "exp")
			if _, present := data["normalizations"]; present {
				t.Fatalf("normalizations = %v", data["normalizations"])
			}
			if _, present := data["raw_text"]; present {
				t.Fatalf("raw_text = %v", data["raw_text"])
			}
		})
	}
}

func TestExplorer1611ExplicitVerifiedUnchanged(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":  `{"answer":"found it","evidence":["README.md"],"verified":[],"inferred":[],"unknown":[]}`,
		"filled": `{"answer":"found it","evidence":["README.md"],"verified":["read README.md"],"inferred":[],"unknown":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Handler = func(FakeRequest) FakeReply { return FakeReply{Text: doc} }
			res := runProg(t, h, loadBody(t, header, `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS, label: 'exp' }); return r;`))
			if res.Calls[0].Status != StatusCompleted {
				t.Fatalf("call = %s", res.Calls[0].Status)
			}
			if n := len(turnRequests(h.fake)); n != 1 {
				t.Fatalf("%d turn requests", n)
			}
			out := outputObject(t, res)
			want := `[]`
			if name == "filled" {
				want = `["read README.md"]`
			}
			got, _ := json.Marshal(out["verified"])
			if string(got) != want {
				t.Fatalf("verified = %s", got)
			}
			data := completedData(t, h, "exp")
			if _, present := data["normalizations"]; present {
				t.Fatalf("normalizations = %v", data["normalizations"])
			}
			if _, present := data["raw_text"]; present {
				t.Fatalf("raw_text = %v", data["raw_text"])
			}
		})
	}
}

func TestExplorer1611Scope(t *testing.T) {
	t.Run("reviewer role is out of scope", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Handler = func(r FakeRequest) FakeReply {
			if r.Repair {
				return FakeReply{Text: findingsValidDoc}
			}
			return FakeReply{Text: findingsMissingVerified}
		}
		res := runProg(t, h, loadBody(t, header, `const r = await agent.call('explore', { agentType: 'reviewer', schema: FINDINGS, label: 'exp' }); return r;`))
		if !res.Calls[0].Meta.RepairUsed {
			t.Fatal("a reviewer explorer document was normalized")
		}
		data := completedData(t, h, "exp")
		if _, present := data["normalizations"]; present {
			t.Fatalf("normalizations = %v", data["normalizations"])
		}
	})
	t.Run("custom two-field schema is out of scope", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Handler = func(r FakeRequest) FakeReply {
			if r.Repair {
				return FakeReply{Text: `{"answer":"found it","verified":[]}`}
			}
			return FakeReply{Text: `{"answer":"found it"}`}
		}
		res := runProg(t, h, loadBody(t, header, `const r = await agent.call('explore', { agentType: 'explorer', schema: {type:'object',required:['answer','verified'],properties:{answer:{type:'string'},verified:{type:'array',items:{type:'string'}}},additionalProperties:false}, label: 'exp' }); return r;`))
		if !res.Calls[0].Meta.RepairUsed {
			t.Fatal("a custom schema document was normalized")
		}
		data := completedData(t, h, "exp")
		if _, present := data["normalizations"]; present {
			t.Fatalf("normalizations = %v", data["normalizations"])
		}
	})
	t.Run("no schema returns text", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Handler = func(FakeRequest) FakeReply { return FakeReply{Text: findingsMissingVerified} }
		res := runProg(t, h, loadBody(t, header, `const r = await agent.call('explore', { agentType: 'explorer', label: 'exp' }); return r;`))
		if res.Calls[0].Status != StatusCompleted {
			t.Fatalf("call = %s", res.Calls[0].Status)
		}
		var text string
		if err := json.Unmarshal(res.Calls[0].Output, &text); err != nil || text != findingsMissingVerified {
			t.Fatalf("output = %s, error = %v", res.Calls[0].Output, err)
		}
		data := completedData(t, h, "exp")
		if _, present := data["normalizations"]; present {
			t.Fatalf("normalizations = %v", data["normalizations"])
		}
	})
	t.Run("minItems verified is not defaulted", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Handler = func(r FakeRequest) FakeReply {
			if r.Repair {
				return FakeReply{Text: findingsValidDoc}
			}
			return FakeReply{Text: findingsMissingVerified}
		}
		schema := `{"type":"object","required":["answer","evidence","verified","inferred","unknown"],"properties":{"answer":{"type":"string"},"evidence":{"type":"array","items":{"type":"string"}},"verified":{"type":"array","items":{"type":"string"},"minItems":1},"inferred":{"type":"array","items":{"type":"string"}},"unknown":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}`
		res := runProg(t, h, loadBody(t, header, `const r = await agent.call('explore', { agentType: 'explorer', schema: `+schema+`, label: 'exp' }); return r;`))
		if !res.Calls[0].Meta.RepairUsed {
			t.Fatal("a minItems verified document was normalized")
		}
		out := outputObject(t, res)
		if arr, ok := out["verified"].([]any); !ok || len(arr) != 1 {
			t.Fatalf("verified = %v", out["verified"])
		}
		data := completedData(t, h, "exp")
		if _, present := data["normalizations"]; present {
			t.Fatalf("normalizations = %v", data["normalizations"])
		}
	})
}

func TestExplorer1611SessionNormalization(t *testing.T) {
	for name, text := range map[string]string{
		"plain missing": findingsMissingVerified,
		"prose prefix":  "here is what I found\n\n" + findingsMissingVerified,
	} {
		t.Run(name, func(t *testing.T) {
			k := newSessionKit(t)
			k.orch.answer = func(st Step, _ string, n int) orchAnswer {
				return orchAnswer{text: text}
			}
			res := runProg(t, k.h, loadBody(t, header, `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS, label: 'exp' }); return r;`))
			if len(k.orch.spawned()) != 1 {
				t.Fatalf("%d spawned", len(k.orch.spawned()))
			}
			if len(k.orch.sentInput()) != 0 {
				t.Fatalf("%d sent inputs", len(k.orch.sentInput()))
			}
			if res.Calls[0].Status != StatusCompleted || res.Calls[0].Meta.RepairUsed {
				t.Fatalf("call = %s repairUsed %v", res.Calls[0].Status, res.Calls[0].Meta.RepairUsed)
			}
			data := completedData(t, k.h, "exp")
			if fmt.Sprintf("%v", data["normalizations"]) != "[explorer_verified_missing_to_empty]" {
				t.Fatalf("normalizations = %v", data["normalizations"])
			}
			if data["raw_text"] != text {
				t.Fatalf("raw_text = %v", data["raw_text"])
			}
			if name == "prose prefix" && data["extracted"] != true {
				t.Fatalf("extracted = %v", data["extracted"])
			}
		})
	}
}

func TestExplorer1611Replay(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(FakeRequest) FakeReply { return FakeReply{Text: findingsMissingVerified} }
	body := `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS, label: 'exp' }); return r;`
	res := runProg(t, h, loadBody(t, header, body))
	first := res.Calls[0].Output
	n := len(turnRequests(h.fake))
	h.cfg.Resume = &ResumeOptions{}
	res = runProg(t, h, loadBody(t, header, body))
	if got := len(turnRequests(h.fake)); got != n {
		t.Fatalf("%d turn requests after replay, was %d", got, n)
	}
	if string(res.Calls[0].Output) != string(first) {
		t.Fatalf("replayed output %s", res.Calls[0].Output)
	}
	if rows := rowsNamed(h.callRows("exp"), EvCompleted); len(rows) != 1 {
		t.Fatalf("%d completed rows", len(rows))
	}
	data := completedData(t, h, "exp")
	if fmt.Sprint(data["normalizations"]) != "[explorer_verified_missing_to_empty]" || data["raw_text"] != findingsMissingVerified {
		t.Fatalf("replayed normalization record = %v", data)
	}
}
