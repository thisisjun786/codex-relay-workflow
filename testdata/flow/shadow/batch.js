import { VERDICT_BATCH } from 'crw:lib/schemas'
export const meta = { name: 'scenario-batch' }
// one reviewer call over a batch of low-tier members: a flush
await agent.call('review batch', { agentType: 'reviewer', label: 'flush-1', schema: VERDICT_BATCH })
return 'done'
