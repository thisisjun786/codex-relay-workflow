export const meta = { name: 'lane-reads-const', phases: ['build'] }
const design = { revision: 0 }
let counter = 0
counter += 1
async function lane(node) {
  let local = 0
  local += 1
  return design.revision + node + local
}
return crw.dag(args.plan, lane, { nodes: args.nodes })
