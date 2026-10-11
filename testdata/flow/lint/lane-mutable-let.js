export const meta = { name: 'lane-mutable-let', phases: ['build'] }
let design = { revision: 0 }
async function lane(node) {
  return design.revision + node
}
design = { revision: 1 }
return crw.dag(args.plan, lane, { nodes: args.nodes })
