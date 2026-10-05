// Force two real oracle processes to finish their state reads before either writes.
// Usage: node record-unlocked.mjs <oracle pabcd-state directory> <output.json>
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { syncBuiltinESMExports } from 'node:module';
import { pathToFileURL } from 'node:url';
const [root, output, worker, workspace] = process.argv.slice(2);
const load = name => import(pathToFileURL(path.join(root, 'dist', name + '.js')));
if (worker !== undefined) {
  const read = fs.readFileSync;
  const target = path.join(workspace, '.codexclaw', 'sessions', 's1.json');
  let first = true;
  fs.readFileSync = function (file, ...options) {
    const value = read.call(this, file, ...options);
    if (file === target && first) {
      first = false;
      fs.writeFileSync(path.join(workspace, `read-${worker}`), 'read');
      const deadline = Date.now() + 5000;
      while (!fs.existsSync(path.join(workspace, `read-${1 - Number(worker)}`))) {
        if (Date.now() > deadline) throw Error('read barrier timed out');
        Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5);
      }
    }
    return value;
  };
  syncBuiltinESMExports();
  const scan = await load('scan-cli');
  const result = scan.runScanCli(scan.parseScanCliArgs(['record', '--session', 's1', '--known', `goal=fact-${worker}`], workspace));
  if (result.code) throw Error(result.output);
} else {
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), 'scan-unlocked-'));
  try {
    const state = await load('state');
    state.writeState(cwd, state.defaultState('s1'));
    const errors = await Promise.all([0, 1].map(id => new Promise((resolve, reject) => {
      const child = spawn(process.execPath, [process.argv[1], root, output, String(id), cwd]);
      let error = '';
      child.stderr.on('data', chunk => error += chunk);
      child.on('error', reject);
      child.on('exit', code => code === 0 ? resolve(null) : reject(Error(error || `worker exit ${code}`)));
    })));
    if (errors.some(Boolean)) throw Error('worker failed');
    const tracker = state.readState(cwd, 's1').interview;
    const events = state.readInterviewEvents(cwd, 's1');
    fs.writeFileSync(output, JSON.stringify({id:'unlocked-update-data-loss',classification:'intentionally-changed',reason:'The state session lock serializes scan read/append/write, retaining every concurrent update.',oracle:{scanRounds:tracker.scanRounds,knownCount:tracker.dimensions.goal.known.length,rows:events.length,roundIds:events.map(e=>e.roundId)}},null,2)+'\n');
  } finally { fs.rmSync(cwd, {recursive:true, force:true}); }
}
