// crw:lib/lane: the composites of the project lane. Each is a sequence of agent calls and crw operations that the script would
// otherwise repeat at every site, written once so that the sequence is the same everywhere: crw.dag (the lane scheduler), crw.explore
// (the explorer fan-out and its record), crw.land (accept, then integrate), crw.evidence (the tier evidence of a node) and
// crw.milestoneHalt (a milestone that cannot go on). A node is judged by its unit check (crw.unitCheck, an operation of the host), and
// no composite calls a reviewer agent (CRW-1447).
//
// crw.dag also runs the signal path of a milestone (CRW-1202) when it is given an onSignal function: every RECEIPT of a lane is a
// trigger, the triggers are taken in the journal's order into one signal at a time, and onSignal decides what the signal comes to
// (continue, refresh or halt). The signal path is described above dag.
//
// install(crw) makes them members of the crw object, next to the operations the host registers (internal/flow/ops): the script
// then reads as the design writes it, `crw.dag(...)`, `crw.land(...)`. A composite is not an operation of its own: it takes no
// position of its path, and every position it uses is that of a call or of an operation it makes, so a resume replays it through
// the journal like any other code of the script. It reads no variable outside its arguments.

import { evidence } from 'crw:lib/evidence'
import { closeMilestone } from 'crw:lib/close'
import { DEFAULT_CAP, dag } from 'crw:lib/lane-dag'
import { EXPLORER_CONCURRENCY, explore } from 'crw:lib/lane-explore'
import { milestoneHalt, land, tierStep } from 'crw:lib/lane-actions'

export { DEFAULT_CAP, EXPLORER_CONCURRENCY, dag, explore }

// install makes the composites members of crw. The engine's crw.tierStep mints the id of a step; the composite replaces it, calling it
// for the part it owns.
export function install(host) {
  const mint = host.tierStep
  host.tierStep = (node, ev) => tierStep(mint, node, ev)
  host.dag = dag
  host.explore = explore
  host.milestoneHalt = milestoneHalt
  host.land = land
  host.closeMilestone = closeMilestone
  host.evidence = evidence
  host.skip = (rule, info) => host.spawnDecision(rule, 'skip', info || {})
  return host
}
