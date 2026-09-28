package reception

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Ladder(ctx context.Context, s *store.Store, rid, subject string, observation any) (Obj, error) {
	states := Unobserved()
	Set(&states, "linear_done", Stage("unmeasured", nil, "the store holds no reading of the issue's status; read it in Linear"))
	sync := Stage("unmeasured", nil, "nothing was read")
	result := func() Obj { return O("handover", states, "coordinationSync", sync) }
	if s == nil || rid == "" {
		return result(), nil
	}
	err := func() error {
		known, e := s.One(ctx, "SELECT 1 FROM events WHERE event_id = ? AND relationship_id = ?", subject, rid)
		if e != nil {
			return e
		}
		if known == nil {
			for _, n := range Progression {
				if n != "read" && n != "linear_done" {
					Set(&states, n, Stage("unmeasured", nil, "the packet's subject names no event of this relationship"))
				}
			}
			return nil
		}
		attempts, e := s.All(ctx, "SELECT state, internal_state FROM attempts WHERE event_id = ?", subject)
		if e != nil {
			return e
		}
		dispatched, uncertain := false, false
		ended := []string{}
		for _, a := range attempts {
			dispatched = dispatched || a.Get("state") == "dispatched"
			uncertain = uncertain || a.Get("state") == "held_uncertain" || a.Get("internal_state") != "settled"
			ended = append(ended, evidence.Text(a.Get("state")))
		}
		transport := Stage("no", "attempts", "no attempt")
		if dispatched {
			transport = Stage("yes", "attempts", "an attempt was dispatched")
		} else if uncertain {
			transport = Stage("unmeasured", nil, "an attempt is held uncertain or not yet settled")
		} else if len(attempts) > 0 {
			transport = Stage("no", "attempts", "attempts ended as "+strings.Join(uniqueSorted(ended), ", ")+"; inbox_only is an approval failure with no send")
		}
		Set(&states, "transport_accepted", transport)
		ack, e := s.One(ctx, "SELECT accepted, verified FROM acks WHERE event_id = ?", subject)
		if e != nil {
			return e
		}
		acknowledgement := Stage("no", "acks", "no acknowledgement row")
		if ack != nil {
			accepted, _ := evidence.PyInt(ack.Get("accepted"))
			if accepted != 0 {
				if ack.Get("verified") == "verified" {
					acknowledgement = Stage("yes", "acks", "accepted and verified")
				} else {
					acknowledgement = Stage("conditional", "acks", "recorded, verification "+evidence.Text(ack.Get("verified")))
				}
			} else {
				acknowledgement = Stage("no", "acks", "the acknowledgement rejected it")
			}
		}
		Set(&states, "relay_ack", acknowledgement)
		verdict, e := s.One(ctx, "SELECT verdict FROM verdicts WHERE event_id = ?", subject)
		if e != nil {
			return e
		}
		judged, accepted := Stage("no", "verdicts", "no verdict"), Stage("no", "verdicts", "no verdict")
		if verdict != nil {
			detail := "verdict " + str(verdict.Get("verdict"))
			judged = Stage("yes", "verdicts", detail)
			state := "no"
			if verdict.Get("verdict") == "verified" {
				state = "yes"
			}
			accepted = Stage(state, "verdicts", detail)
		}
		Set(&states, "criteria_verdict", judged)
		Set(&states, "parent_acceptance", accepted)
		landing := Stage("unmeasured", nil, "which repository, pull request and head is a forge reading; supply all three in the observation")
		if truth(Get(observation, "repository")) && truth(Get(observation, "prNumber")) && truth(Get(observation, "headSha")) {
			turns, e := s.All(ctx, "SELECT turn_id, state, candidate_head FROM merge_turns WHERE relationship_id = ? AND pr_number = ? AND repository = ? ORDER BY updated_at DESC", rid, Get(observation, "prNumber"), Get(observation, "repository"))
			if e != nil {
				return e
			}
			heads := []string{}
			detail := "no merge turn"
			if len(turns) > 0 {
				detail = "no landed turn"
			}
			landing = Stage("no", "merge_turns", detail)
			for _, turn := range turns {
				if turn.Get("state") != "landed" {
					continue
				}
				heads = append(heads, str(turn.Get("candidate_head")))
				if equal(turn.Get("candidate_head"), Get(observation, "headSha")) {
					landing = Stage("yes", "merge_turns", "turn "+str(turn.Get("turn_id"))+" landed the observed head")
					break
				}
			}
			if Get(landing, "state") != "yes" && len(heads) > 0 {
				landing = Stage("no", "merge_turns", "landed only for head "+strings.Join(uniqueSorted(heads), ", ")+", not the observed one")
			}
		}
		Set(&states, "merge_landing", landing)
		outbox, e := s.All(ctx, "SELECT state FROM sync_outbox WHERE relationship_id = ? AND event_id = ?", rid, subject)
		if e != nil {
			return e
		}
		detail := "no outbox row"
		if len(outbox) > 0 {
			detail = "no confirmed outbox row"
		}
		sync = Stage("no", "sync_outbox", detail)
		for _, row := range outbox {
			if row.Get("state") == "confirmed" {
				sync = Stage("yes", "sync_outbox", "a coordination summary block written and read back; this says nothing about the issue's status")
				break
			}
		}
		return nil
	}()
	if err != nil {
		detail := "the store could not be read: OperationalError"
		for _, n := range Progression {
			if n != "read" && n != "linear_done" {
				Set(&states, n, Stage("unmeasured", nil, detail))
			}
		}
		sync = Stage("unmeasured", nil, detail)
	}
	return result(), CheckProgression(states)
}
