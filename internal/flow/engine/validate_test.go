package engine

import (
	"testing"
)

// The message must be one JSON document and nothing after it.
func TestTrailingJSONIsInvalid(t *testing.T) {
	for name, text := range map[string]string{
		"closing bracket": `{"ok":true} ]`,
		"closing brace":   `{"ok":true} }`,
		"second value":    `{"ok":true} {"ok":false}`,
		"garbage":         `{"ok":true} garbage`,
		"comma":           `{"ok":true},`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Handler = func(FakeRequest) FakeReply { return FakeReply{Text: text} }
			res := h.run(`const r = await agent.call('p', { agentType: 'explorer', schema: { type: 'object', properties: { ok: { type: 'boolean' } }, required: ['ok'], additionalProperties: false } })
return [r.status, r.reason, r.meta.turnIds.length]`)
			if res.Status != RunCompleted || len(res.Calls) != 1 {
				t.Fatalf("%s %s", res.Status, res.Error)
			}
			c := res.Calls[0]
			if c.Status != StatusNullModel || c.Reason != ReasonSchemaInvalidAfterRepair || !c.Meta.RepairUsed || countTurns(h.fake) != 2 {
				t.Errorf("%s/%s repair %v turns %d", c.Status, c.Reason, c.Meta.RepairUsed, countTurns(h.fake))
			}
		})
	}
	// whitespace around the one document is fine
	h := newHarness(t)
	h.fake.Handler = func(FakeRequest) FakeReply { return FakeReply{Text: "\n {\"ok\": true}\n\n"} }
	res := h.run(`return (await agent.call('p', { agentType: 'explorer', schema: { type: 'object', properties: { ok: { type: 'boolean' } }, required: ['ok'], additionalProperties: false } })).status`)
	if got := value[string](t, res); got != StatusCompleted {
		t.Errorf("%s", got)
	}
}
