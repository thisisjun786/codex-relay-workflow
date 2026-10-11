export const meta = { name: 'on-signal-mutable-let', phases: ['build'] }
let design = { revision: 0 }
const lane = async (node) => node
design = { revision: 1 }
return crw.dag(args.plan, lane, { nodes: args.nodes, onSignal: async (sig) => (design.revision > 1 ? 'halt' : 'continue') })
