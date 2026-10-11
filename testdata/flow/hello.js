import { FINDINGS } from 'crw:lib/schemas'
import { greeting, SIZES } from './hello-lib.js'
export const meta = { name: 'hello', description: 'F01 smoke script: a call, a fan-out and a pipeline on the fake backend', phases: ['greet', 'fan-out'] }

phase('greet')
const hi = await agent(greeting('crw'), { agentType: 'explorer', label: 'greet' })
log('greeted: ' + hi)

phase('fan-out')
const found = await parallel([1, 2, 3].map((n) => async () =>
  agent.call('look at ' + n, { agentType: 'explorer', schema: FINDINGS, label: 'look-' + n })))
const sized = await pipeline(SIZES,
  async (s) => s.length,
  async (n, s, i) => {
    const r = await agent.call('count ' + n, { agentType: 'explorer', label: 'count-' + i })
    return { s, n, status: r.status }
  })

// top-level return is a CRW extension: the value is the run's result
return { hi, found: found.map((r) => r.status), sized }
