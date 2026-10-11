package engine

import (
	"fmt"
	"testing"
)

// A join item's key is data: a key that holds the path separators must not name the same path as a nested join.
func TestPathIdentityIsOneToOneForSeparatorKeys(t *testing.T) {
	h := newHarness(t)
	res := h.run(`return await parallel([
  async () => agent.call('flat', { agentType: 'explorer', label: 'flat' }),
  async () => parallel([async () => agent.call('nested', { agentType: 'explorer', label: 'nested' })], { keys: ['b'] }),
  async () => agent.call('pct', { agentType: 'explorer', label: 'pct' }),
  async () => agent.call('slash', { agentType: 'explorer', label: 'slash' }),
], { keys: ['a/1:b', 'a', 'a%2F1:b', 'a%2F1%3Ab'] })`)
	if res.Status != RunCompleted {
		t.Fatalf("%s: %s", res.Status, res.Error)
	}
	if len(res.Calls) != 4 {
		t.Fatalf("calls %+v", res.Calls)
	}
	ids, paths, threads := map[string]string{}, map[string]string{}, map[string]string{}
	for _, c := range res.Calls {
		for what, m := range map[string]map[string]string{"callId": ids, "path": paths, "thread": threads} {
			v := map[string]string{"callId": c.CallID, "path": fmt.Sprintf("%s#%d", c.Path, c.Position), "thread": c.Meta.ThreadID}[what]
			if prev, dup := m[v]; dup {
				t.Errorf("%s %q is shared by %s and %s", what, v, prev, c.Label)
			}
			m[v] = c.Label
		}
		// each call's journal holds exactly its own rows
		rows := CallEvents(h.journal, c.CallID)
		for _, r := range rows {
			if r.Path != c.Path || r.Label != c.Label {
				t.Errorf("call %s (%s) reads a row of %s at %s", c.CallID, c.Label, r.Label, r.Path)
			}
		}
		info, ok := LookupCall(h.journal, c.CallID)
		if !ok || info.Path != c.Path {
			t.Errorf("lookup of %s: %+v", c.Label, info)
		}
	}
}
