package role

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const AliasMapDate = "2026-09-24"

type ModelAlias string
type ResolvedAlias struct {
	ID       string `json:"id"`
	Verified bool   `json:"verified"`
	MapDate  string `json:"mapDate"`
}

// ModelAliases returns the dated oracle table without mutable startup state.
func ModelAliases() map[ModelAlias]string {
	return map[ModelAlias]string{"deepseek": "command-code/deepseek-deepseek-v4.1-flash", "swe2": "devin/swe-2", "kimi": "kimi/kimi-for-coding-highspeed", "sol": "gpt-6-sol", "luna": "gpt-6-luna"}
}

func ResolveDispatchAlias(name string, env host.LookupEnv) ResolvedAlias {
	item := ResolvedAlias{ID: name, MapDate: AliasMapDate}
	id, ok := ModelAliases()[ModelAlias(name)]
	if !ok {
		return item
	}
	item.ID = id
	for _, entry := range ReadNativeCatalog(env) {
		if entry.ID == id {
			item.Verified = true
			break
		}
	}
	return item
}

// RenderDispatchCard keeps dispatch-card.ts:36-47, including alias order and
// the UTF-16 character budget. The printed cell is data, never executed by the
// renderer, but the parent runs it as printed: its model is the first alias the
// local catalog lists, or none (CRW-1180), so the first cell cannot fail on a
// model the host does not know.
func RenderDispatchCard(env host.LookupEnv) (string, error) {
	aliases := []ModelAlias{"deepseek", "swe2", "kimi", "sol", "luna"}
	resolved := make([]ResolvedAlias, len(aliases))
	example := ""
	for i, alias := range aliases {
		resolved[i] = ResolveDispatchAlias(string(alias), env)
		if example == "" && resolved[i].Verified {
			example = resolved[i].ID
		}
	}
	core := dispatchUnresolvedCard(example)
	if v, _ := env("CRW_SPAWN_V1"); v == "1" {
		core = dispatchV1Card(example)
	}
	if example == "" {
		core += "\n" + dispatchNoModelNote
	}
	if dispatchUTF16Len(core) > 1200 {
		return "", sentinel("dispatch card core exceeds 1200 characters")
	}
	card := core + "\nAliases (map " + AliasMapDate + "; local Codex catalog membership only):"
	for i, alias := range aliases {
		item := resolved[i]
		status := "unverified, map " + AliasMapDate
		if item.Verified {
			status = "verified in local catalog"
		}
		line := fmt.Sprintf("\n%s -> %s (%s)", alias, item.ID, status)
		if dispatchUTF16Len(card)+dispatchUTF16Len(line) > 1200 {
			break
		}
		card += line
	}
	if dispatchUTF16Len(card) <= 1200 {
		return card, nil
	}
	return core, nil
}

func dispatchUTF16Len(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := pyjson.CodePoint(s, i)
		i += size
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// The following cards are the oracle text after name substitution, with the example model supplied by the caller: model is the
// catalog-verified id, or "" when none is, and then the cell carries no model key and the role default applies.
func dispatchModelKey(model string) string {
	if model == "" {
		return ""
	}
	return `model:"` + model + `",`
}

// dispatchNoModelNote says why the first cell sets no model.
const dispatchNoModelNote = `No alias is verified locally: the cell sets no model (role default).`

func dispatchV1Card(model string) string {
	return `[crw] Subagent dispatch: V1 requested by CRW_SPAWN_V1=1 (override, not host evidence).
Code Mode first cell: await tools.multi_agent_v1__spawn_agent({message:"Report your model and say OK; do not edit files.",` + dispatchModelKey(model) + `reasoning_effort:"low"});
Direct: multi_agent_v1.spawn_agent({message,model?,reasoning_effort?}); wait: tools.multi_agent_v1__wait_agent({targets:[agent_id],timeout_ms}); close: tools.multi_agent_v1__close_agent({target:agent_id}). Replace the probe message with the task; obey managed fallback dispatch first.`
}

func dispatchUnresolvedCard(model string) string {
	return `[crw] Subagent dispatch: family unresolved at SessionStart; managed fallback first; swap the probe for the task. First Code Mode cell (resolves and calls):
const n = ALL_TOOLS.map(t => t.name), has = r => n.some(x => r.test(x));
const s = n.filter(x => /spawn_agent$/.test(x));
if (s.length !== 1) throw new Error("expected one spawn_agent helper, found " + s.length);
const v1 = has(/(send_input|close_agent|resume_agent)$/), v2 = has(/(followup_task|interrupt_agent|list_agents)$/);
if (v1 === v2) throw new Error("collab family unresolved: v1=" + v1 + " v2=" + v2);
const a = {message:"Report your model and say OK; do not edit files.",` + dispatchModelKey(model) + `reasoning_effort:"low"};
text(await tools[s[0]](v1 ? a : {...a, task_name:"model_probe", fork_turns:"none"}));`
}
