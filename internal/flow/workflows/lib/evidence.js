// crw:lib/evidence: the independent failure evidence of SP-TIER-1, kept for one node by its lane.
//
// A lane builds the tracker from nothing and feeds it the results of the calls it makes, in the order it makes them. The tracker
// is a function of those results only, so a lane that is replayed after a resume feeds it the same results and gets the same
// answers; nothing here is read from outside the lane. The rule ids are not named here: the lane passes the key of a rule to the
// operation that records the decision (crw.spawnDecision, crw.stepRecord).
//
// What counts, on one tier:
//   a  the reviewer says needs_changes in two consecutive reviewed rounds, and at least one finding digest is in both; in the
//      node lane the unit check is the reviewed round: two consecutive unit checks that both rule needs_changes and share a finding
//      id (reviewadapter.FindingID: the file and the text, never the line, so a finding that moved in the file is the same); the
//      findings of a verified check block nothing and are no evidence: the lane carries what is left of them after its round trips;
//   b  the required verification fails on two consecutive committed heads;
//   c  two consecutive executor calls of the tier end null_model;
//   d  four reviewed rounds (reviews or unit checks) without a verified verdict.
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
    // a reviewed round: the reviewer's call, its verdict and the digests of its findings (the old reviewer path, kept for old journals)
    review(tier, callId, verdict, findingDigests) {
      const t = tierOf(state, tier)
      t.rounds += 1
      t.reviews.push({ kind: 'review', callId, verdict, digests: findingDigests || [], counts: verdict === 'needs_changes' })
    },
    // a unit check of the node lane: its call, its verdict and its findings, each a finding id or a finding with its id; only the
    // id is read, never the line. Only a check that rules needs_changes counts toward (a): the lane fixes every finding, but the
    // findings of a verified check block nothing, and what is left of them after the round trips is carried, not a tier step.
    unitCheck(tier, callId, verdict, findingIds) {
      const t = tierOf(state, tier)
      const ids = (findingIds || []).map((f) => (f !== null && typeof f === 'object' ? f.id : f)).filter((id) => typeof id === 'string' && id !== '')
      t.rounds += 1
      t.reviews.push({ kind: 'unit_check', callId, verdict, digests: ids, counts: verdict === 'needs_changes' && ids.length > 0 })
    },
    // the letter of the rule that is due on the tier, or null
    due(tier) {
      const t = tierOf(state, tier)
      const n = t.reviews.length
      if (n >= 2) {
        const a = t.reviews[n - 2]
        const b = t.reviews[n - 1]
        if (a.counts && b.counts && b.digests.some((d) => a.digests.includes(d))) return 'a'
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
        reviewer_calls: t.reviews.filter((r) => r.kind === 'review').map((r) => r.callId),
        unit_check_calls: t.reviews.filter((r) => r.kind === 'unit_check').map((r) => r.callId),
        verification_digests: t.failedDigests.slice(),
        null_model_calls: t.nullCalls.slice(),
        rounds: t.rounds,
      }
    },
    ids(tier) {
      const e = this.evidence(tier)
      return [].concat(e.reviewer_calls, e.unit_check_calls, e.verification_digests, e.null_model_calls)
    },
  }
}
