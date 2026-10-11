export const meta = { name: 'determinism', phases: ['build'] }
const started = Date.now()
const pick = Math.random()
const today = new Date()
return [started, pick, today]
