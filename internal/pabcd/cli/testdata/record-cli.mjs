// Development-only recorder. Usage: node record-cli.mjs <oracle-root> <scratch-root>.
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
const [oracle, scratch] = process.argv.slice(2);
const src = path.join(oracle, 'plugins/codexclaw/components/pabcd-state/src');
const evidence = await import(pathToFileURL(path.join(src, 'evidence-cli.ts')));
const memory = await import(pathToFileURL(path.join(src, 'memory-cli.ts')));
const state = await import(pathToFileURL(path.join(src, 'state.ts')));
const attempts = await import(pathToFileURL(path.join(src, 'subagent-evidence.ts')));
const cases = [];
const flagArgs = ['resolve', '--session', 'rec-s1', '--agent', 'a1', '--receipt', '.codexclaw/evidence/check.md'];
const parseCases = {
  evidence: [[], ['help'], ['resolve'], ['resolve', '--session', '../x', '--agent', 'a1'], flagArgs,
    [...flagArgs, '--turn', ''], [...flagArgs, '--turn'], [...flagArgs, '--override'],
    [...flagArgs, '--receipt', 'later'], ['resolve', '--session=rec-s1', '--agent', 'a1', '--receipt', 'x'],
    ['resolve', '--session', 'rec-s1', '--agent', '--receipt', '--receipt', '--override']],
  memory: [[], ['grant'], ['allow-write'], ['allow-write', '--session', '../x'],
    ['allow-write', '--session=rec-s1'], ['allow-write', '--session', 'rec-s1'],
    ['allow-write', '--session', 'first', '--session=last'], ['allow-write', '--session'],
    ['allow-write', '--session', 'rec-s1', '--force'], ['allow-write', '--help'],
    ['allow-write', '-h'], ['allow-write', '--session', 'rec-s1', '--help'],
    ['allow-write', '--unknown', '--help'], ['allow-write', '--session', '--help']]
};
for (const [kind, argvs] of Object.entries(parseCases)) for (const [i, argv] of argvs.entries()) {
  const fn = kind === 'evidence' ? evidence.parseEvidenceCliArgs : memory.parseMemoryCliArgs;
  cases.push({id: `${kind}-parse-${i}`, kind, argv, expect: fn(argv, '<WS>')});
}
cases.push({id: 'memory-usage', kind: 'usage', expect: memory.MEMORY_USAGE});
for (const mode of ['memory-new', 'memory-existing', 'evidence-resolve', 'evidence-ambiguous', 'evidence-no-match', 'evidence-empty-turn']) {
  const cwd = fs.mkdtempSync(path.join(scratch, 'cli-oracle-'));
  const tombstone = turnId => ({agentId:'a1',turnId,agentType:'worker',attempts:3,receiptClaimed:'none',recordedAt:'recorded',resolvable:true});
  const seed = {...state.defaultState('rec-s1'), phase:'B', slug:'keep', memoryWriteRequested:true, stopBlockTotal:7,
    unverifiedSubagents:[tombstone('t1'), tombstone('t2'), tombstone('')]};
  if (mode !== 'memory-new') state.writeState(cwd, seed);
  fs.mkdirSync(path.join(cwd,'.codexclaw/evidence'), {recursive:true});
  fs.writeFileSync(path.join(cwd,'.codexclaw/evidence/check.md'),'verified');
  for (const turn of ['t1','t2','']) attempts.writeAttempts(cwd,'rec-s1','a1',3,turn);
  const args = {verb:'resolve',sessionId:'rec-s1',agentId:mode==='evidence-no-match'?'other':'a1',receipt:'.codexclaw/evidence/check.md',cwd};
  if (mode === 'evidence-resolve') args.turnId='t1';
  if (mode === 'evidence-empty-turn') args.turnId='';
  const result = mode.startsWith('memory') ? memory.runMemoryCli({verb:'allow-write',sessionId:'rec-s1',cwd}) : evidence.runEvidenceCli(args);
  const ledger = path.join(cwd,'.codexclaw/ledger.jsonl');
  const value = {result, state:state.readState(cwd,'rec-s1'), counters:['t1','t2',''].map(t=>attempts.readAttempts(cwd,'rec-s1','a1',t)),
    ledger: fs.existsSync(ledger)?fs.readFileSync(ledger,'utf8').trim().split('\n').map(s=>JSON.parse(s)):[]};
  const normalized = JSON.stringify(value).replaceAll(cwd,'<WS>').replace(/\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z/g,'<TIME>');
  cases.push({id:mode,kind:'run',expect:JSON.parse(normalized)});
  fs.rmSync(cwd,{recursive:true});
}
process.stdout.write(JSON.stringify(cases,null,2)+'\n');
