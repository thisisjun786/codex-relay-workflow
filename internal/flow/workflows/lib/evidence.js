// crw:lib/evidence: the independent failure evidence of SP-TIER-1, kept for one node by its lane.
//
// A lane builds the tracker from nothing and feeds it the results of the calls it makes, in the order it makes them. The tracker
// is a function of those results only, so a lane that is replayed after a resume feeds it the same results and gets the same
// answers; nothing here is read from outside the lane. The rule ids are not named here: the lane passes the key of a rule to the
// operation that records the decision (crw.spawnDecision, crw.stepRecord).
//
// What counts, on one tier:
//   a  the reviewer says needs_changes in two consecutive reviewed rounds, and at least one finding digest is in both;
//   b  the required verification fails on two consecutive committed heads;
//   c  two consecutive executor calls of the tier end null_model;
//   d  four reviewed rounds without a verified verdict.
// What never counts: the executor's own failed or budget_exhausted outcome, needs_research, blocked_scope, a single null_model,
// null_unknown, and any held, refused or superseded result - those are not evidence about the executor, and the lane never feeds
// them here. A result that is not a verdict is not a reviewed round.

export const ROUNDS = 4

function tierOf(state, tier) {
  if (!state[tier]) {
    state[tier] = { nulls: 0, nullCalls: [], failedHeads: 0, failedDigests: [], reviews: [], rounds: 0 }
  }
  return state[tier]
}

export function evidence(node) {
  const state = {}
  return {
    node,
    // an executor call of the tier ended in the state crw.classify gave: null_model counts toward (c), any other state ends the run of nulls
    executor(tier, callId, classified) {
      const t = tierOf(state, tier)
      if (classified === 'null_model') {
        t.nulls += 1
        t.nullCalls.push(callId)
      } else {
        t.nulls = 0
        t.nullCalls = []
      }
    },
    nullModel(tier, callId) {
      this.executor(tier, callId, 'null_model')
    },
    // the verification of a committed head
    verification(tier, head, pass, digest) {
      const t = tierOf(state, tier)
      if (pass) {
        t.failedHeads = 0
        t.failedDigests = []
      } else {
        t.failedHeads += 1
        t.failedDigests.push(digest || head)
      }
    },
    // a reviewed round: the reviewer's call, its verdict and the digests of its findings
    review(tier, callId, verdict, findingDigests) {
      const t = tierOf(state, tier)
      t.rounds += 1
      t.reviews.push({ callId, verdict, digests: findingDigests || [] })
    },
    // the letter of the rule that is due on the tier, or null
    due(tier) {
      const t = tierOf(state, tier)
      const n = t.reviews.length
      if (n >= 2) {
        const a = t.reviews[n - 2]
        const b = t.reviews[n - 1]
        if (a.verdict === 'needs_changes' && b.verdict === 'needs_changes' && b.digests.some((d) => a.digests.includes(d))) return 'a'
      }
      if (t.failedHeads >= 2) return 'b'
      if (t.nulls >= 2) return 'c'
      if (t.rounds >= ROUNDS && !t.reviews.some((r) => r.verdict === 'verified')) return 'd'
      return null
    },
    tierStepDue(tier) {
      return this.due(tier) !== null
    },
    // the evidence a tier step is recorded with
    evidence(tier) {
      const t = tierOf(state, tier)
      return {
        tier,
        letter: this.due(tier),
        reviewer_calls: t.reviews.map((r) => r.callId),
        verification_digests: t.failedDigests.slice(),
        null_model_calls: t.nullCalls.slice(),
        rounds: t.rounds,
      }
    },
    ids(tier) {
      const e = this.evidence(tier)
      return [].concat(e.reviewer_calls, e.verification_digests, e.null_model_calls)
    },
  }
}
