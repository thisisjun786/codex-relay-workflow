package mergeturn

import (
	"encoding/json"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

func Test26_MTN_4_whole_outstanding(t *testing.T) {
	w := newFx(t)
	held := w.step(w.claimOn(alpha, fxA, "head-a", fxBase, true)).(map[string]any)["turnId"].(string)
	w.step(w.m.Outstanding(w.ctx, alpha.TaskID))
	w.step(w.claimOn(alpha, fxA, "head-a", fxBase, true))
	other, err := store.Open(w.ctx, w.s.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	after := *w.m
	after.Store = other
	w.step(after.Outstanding(w.ctx, alpha.TaskID))
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	w.answer(held, alpha.TaskID)
	w.step(w.begin(held, defaults()))
	w.step(w.m.Unknown(w.ctx, held, alpha.TaskID, "the host stopped answering"))
	w.m.Now = func() string { return "2023-11-26T12:00:00.000000+00:00" }
	w.step(w.m.Outstanding(w.ctx, alpha.TaskID))
	w.sameAsPython("mtn4_outstanding")
}

func Test26_MTN_5_whole_claims(t *testing.T) {
	w := newFx(t)
	w.step(w.claimOn(alpha, fxA, "head-a", fxBase, true))
	waiter := w.step(w.claimOn(beta, fxB, "head-b", fxBase, true)).(map[string]any)["turnId"].(string)
	w.step(w.m.Target(w.ctx, fxRepo, fxBase))
	w.step(w.m.Attest(w.ctx, waiter, "claimed_turn", "chat-1", beta.TaskID, "I said in chat that it is my turn"))
	w.turn(waiter)
	w.step(w.claimOn(alpha, fxA, "head-a", fxBase, true))
	key, _ := TargetKey(fxRepo, fxBase)
	w.rows("SELECT * FROM merge_turns WHERE target_key=?", key)
	w.step(w.claimOn(beta, fxA, "head-x", fxBase, true))
	w.contests()
	w.step(w.claimOn(alpha, "PRJ-UNKNOWN", "head-x", fxBase, true))
	w.step(w.m.Request(w.ctx, "owner/other", fxBase, fxB, beta.TaskID, beta.HostID, "head-b", true))
	w.sameAsPython("mtn5_claims")
}

func Test26_MTN_6_whole_racing_claims(t *testing.T) {
	for _, row := range []struct {
		name                                               string
		first, second                                      registry.Endpoint
		firstProject, secondProject, firstHead, secondHead string
	}{
		{"mtn6_alpha_first", alpha, beta, fxA, fxB, "head-a", "head-b"},
		{"mtn6_beta_first", beta, alpha, fxB, fxA, "head-b", "head-a"},
	} {
		t.Run(row.name, func(t *testing.T) {
			w := newFx(t)
			w.step(w.claimOn(row.first, row.firstProject, row.firstHead, fxBase, true))
			w.step(w.claimOn(row.second, row.secondProject, row.secondHead, fxBase, true))
			w.step(w.m.Target(w.ctx, fxRepo, fxBase))
			key, _ := TargetKey(fxRepo, fxBase)
			w.rows("SELECT turn_id,state FROM merge_turns WHERE target_key=? ORDER BY turn_id", key)
			w.sameAsPython(row.name)
		})
	}
}

func Test26_MTN_16_whole_grants(t *testing.T) {
	w := newFx(t)
	id := w.step(w.claimOn(alpha, fxA, "head-a", fxBase, true)).(map[string]any)["turnId"].(string)
	grant := w.must(w.m.Turn(w.ctx, id))["grant"].(map[string]any)["grantId"].(string)
	for range 2 {
		w.step(w.m.Acknowledge(w.ctx, id, alpha.TaskID, grant, "read and checked"))
	}
	w.step(fReady(w, id, alpha.TaskID, true, "", "head-a2"))
	w.turn(id)
	w.step(w.m.Acknowledge(w.ctx, id, alpha.TaskID, grant, "old head"))
	next := w.must(w.m.Turn(w.ctx, id))["grant"].(map[string]any)["grantId"].(string)
	w.step(w.m.Acknowledge(w.ctx, id, alpha.TaskID, next, ""))
	w.step(w.m.Acknowledge(w.ctx, id, beta.TaskID, next, "read"))
	w.step(fRelease(w, id, alpha.TaskID, "done", "returned", ""))
	w.step(w.m.Acknowledge(w.ctx, id, alpha.TaskID, grant, "late"))
	w.step(w.claimOn(alpha, fxA, "head-a", fxBase, true))
	w.sameAsPython("mtn16_grants")
}

func fLegacyGrant(w *fx, id, kind, key, evidence string) {
	w.exec("INSERT INTO merge_turn_ledger (entry_id,turn_id,kind,from_state,to_state,evidence_kind,actor_task_id,evidence,idempotency_key,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?)", "legacy-"+key, id, "attestation", nil, nil, kind, beta.TaskID, evidence, key, "2026-01-01T00:00:00Z")
}
func Test26_MTN_17_whole_namespace(t *testing.T) {
	t.Run("namespace", func(t *testing.T) {
		w := newFx(t)
		id := fClaim(w, alpha, fxA, "head-a", true)
		for _, pair := range [][2]string{{"grant", "mine-1"}, {"landing_base_restated", "mine-2"}, {"transport_accepted", "close:landed"}, {"transport_accepted", "restate-base:1"}} {
			w.step(fAttest(w, id, pair[0], pair[1], "accepted"))
		}
		w.step(fAttest(w, id, "transport_accepted", "delivery-9", "accepted"))
		waiter := fClaim(w, beta, fxB, "head-b", true)
		tenure := w.must(w.m.Turn(w.ctx, waiter))["tenure"]
		key := fmt.Sprintf("promote:%v", tenure)
		w.exec("INSERT INTO merge_turn_ledger (entry_id,turn_id,kind,from_state,to_state,evidence_kind,actor_task_id,evidence,idempotency_key,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?)", "squatted-1", waiter, "attestation", nil, nil, "transport_accepted", beta.TaskID, "not a promotion", key, "2026-01-01T00:00:00Z")
		w.step(fRelease(w, id, alpha.TaskID, "handing it on", "returned", ""))
		w.turn(id)
		w.turn(waiter)
		w.step(w.m.Target(w.ctx, fxRepo, fxBase))
		w.sameAsPython("mtn17_namespace")
	})
	t.Run("legacy", func(t *testing.T) {
		w := newFx(t)
		id := fClaim(w, alpha, fxA, "head-a", true)
		g := w.must(w.m.Turn(w.ctx, id))["grant"].(map[string]any)["grantId"].(string)
		for _, row := range [][2]string{{"chat-note-1", "approved in chat"}, {"grant:not-json", "{oops"}, {"grant:no-sequence", `{"grantId": "mtg-forged"}`}} {
			fLegacyGrant(w, id, "grant", row[0], row[1])
		}
		w.turn(id)
		w.step(w.m.Target(w.ctx, fxRepo, fxBase))
		w.step(w.m.Outstanding(w.ctx, alpha.TaskID))
		w.step(w.m.Acknowledge(w.ctx, id, alpha.TaskID, g, "read the grant past the rows nobody can read"))
		w.step(w.begin(id, defaults()))
		w.sameAsPython("mtn17_legacy")
	})
	t.Run("foreign grants", func(t *testing.T) {
		w := newFx(t)
		id := fClaim(w, alpha, fxA, "head-a", true)
		turn := w.must(w.m.Turn(w.ctx, id))
		mine := turn["grant"].(map[string]any)["grantId"].(string)
		tenure := turn["tenure"].(int64)
		for _, row := range []struct {
			key      string
			envelope map[string]any
		}{
			{"grant:mtg-forged", map[string]any{"grantId": "mtg-forged", "sequence": 9999, "turnId": id, "tenure": tenure, "recipientTaskId": beta.TaskID, "candidateHead": "head-x"}},
			{"grant:" + GrantID(id, tenure, 9998), map[string]any{"grantId": GrantID(id, tenure, 9998), "sequence": 9998, "turnId": "some-other-turn", "tenure": tenure, "recipientTaskId": beta.TaskID, "candidateHead": "head-x"}},
			{"wrong-key", map[string]any{"grantId": GrantID(id, tenure, 9997), "sequence": 9997, "turnId": id, "tenure": tenure, "recipientTaskId": beta.TaskID, "candidateHead": "head-x"}},
		} {
			text, err := json.Marshal(row.envelope)
			if err != nil {
				t.Fatal(err)
			}
			fLegacyGrant(w, id, "grant", row.key, string(text))
		}
		w.turn(id)
		w.step(w.m.Acknowledge(w.ctx, id, alpha.TaskID, mine, "the impersonating rows are not this turn's grant"))
		w.turn(id)
		w.sameAsPython("mtn17_foreign_grants")
	})
	t.Run("only legacy", func(t *testing.T) {
		w := newFx(t)
		id := fClaim(w, alpha, fxA, "head-a", true)
		w.exec("DELETE FROM merge_turn_ledger WHERE turn_id=? AND evidence_kind='grant'", id)
		fLegacyGrant(w, id, "grant", "chat-note-1", "approved in chat")
		w.turn(id)
		w.step(w.begin(id, defaults()))
		w.sameAsPython("mtn17_only_legacy")
	})
}

func Test26_MTN_20_whole_readings(t *testing.T) {
	w := newFx(t)
	for i, mode := range []string{"reads", "stale", "own", "unreadable", "early", "mark", "abbrev"} {
		base := fmt.Sprintf("read-%d", i)
		w.target.set(fxRepo, base, "base-0")
		id := w.heldOn(alpha, fxA, "head-a", base)
		switch mode {
		case "reads":
			w.target.set(fxRepo, base, "cccccccccccccccccccccccccccccccccccccccc")
			w.step(w.check(id, "head-a", "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", ""))
		case "stale", "own":
			stated := "base-x"
			if mode == "own" {
				stated = "head-a"
			}
			w.step(w.check(id, "head-a", stated, ""))
			w.turn(id)
		case "unreadable":
			w.target.forget(fxRepo, base)
			w.step(w.check(id, "head-a", "base-0", ""))
			w.checksRows(id)
			w.step(w.m.Target(w.ctx, fxRepo, base))
		case "early":
			w.reads()
			w.step(w.check(id, "head-z", "base-0", ""))
			w.step(w.check(id, "head-a", "base-0", beta.TaskID))
			w.reads()
		case "mark":
			w.step(w.check(id, "head-a", "base-0", ""))
			w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id=? AND evidence_kind='currency_confirmed'", id)
		case "abbrev":
			w.target.set(fxRepo, base, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			w.step(w.check(id, "head-a", "aaaaaaa", ""))
			w.step(w.check(id, "head-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ""))
			w.step(w.m.Land(w.ctx, id, alpha.TaskID, "merge-1", "aaaaaaa", "merged", w.target))
		}
	}
	w.sameAsPython("mtn20_readings")
}
