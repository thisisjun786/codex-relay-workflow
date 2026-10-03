// Records what the CXC v0.2.40 oracle's parseSourceBoundReceipt (pabcd-state/src/source-receipt.ts) and validateCheckReceipt
// (check-gate.ts) answer for the cases of cases.json, for the Go tests to replay from oracle-gate.json (no Node at test time).
// Every case runs in a fresh work directory under the work root with .codexclaw/evidence made. In a case "{S}" is the state
// directory name (.codexclaw in the oracle, .crw in the port) and "<CWD>" the case's workspace. A setup step is
// {file, text}, {dir} or {symlink, to}, applied in order below the workspace. A receipt case is {id, kind, setup, claim};
// a gate case {id, epoch, session, setup, claim} (epoch null is a state with no check epoch, claim "" no path).
// The text the oracle takes from Node or V8 (a JSON.parse message, an fs error) is not reproducible in Go and is recorded as
// "<JSON>" or "<ERR>" after the fixed words that precede it. Recorded with Node v24.20.0 as
//   TZ=UTC node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> cases.json > oracle-gate.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdirSync, writeFileSync, readFileSync, symlinkSync } from "node:fs";
import { join, dirname } from "node:path";

const [distUrl, workRoot, casesFile] = process.argv.slice(2);
process.env.GIT_CEILING_DIRECTORIES = workRoot;
process.env.GIT_CONFIG_GLOBAL = "/dev/null";
process.env.GIT_CONFIG_NOSYSTEM = "1";
const { parseSourceBoundReceipt } = await import(distUrl + "/source-receipt.js");
const { validateCheckReceipt } = await import(distUrl + "/check-gate.js");
const raw = readFileSync(casesFile, "utf8");
const S = ".codexclaw";
let n = 0;
const fresh = () => {
  const ws = join(workRoot, "c" + n++);
  mkdirSync(join(ws, S, "evidence"), { recursive: true });
  return ws;
};
const place = (c, ws) => {
  for (const o of c.setup) {
    const p = join(ws, o.file ?? o.dir ?? o.symlink);
    if (o.dir !== undefined) { mkdirSync(p, { recursive: true }); continue; }
    mkdirSync(dirname(p), { recursive: true });
    if (o.file !== undefined) writeFileSync(p, o.text);
    else symlinkSync(o.to, p);
  }
};
const mask = (msg, claim) => {
  for (const [prefix, tail] of [
    ["receipt is not valid JSON: " + claim + " (", "<JSON>)"],
    ["artifactManifest verdict cannot be parsed: ", "<JSON>"],
    ["artifactManifest evidence root cannot be read: ", "<ERR>"],
  ]) if (msg.startsWith(prefix)) return prefix + tail;
  const m = /^artifactManifest\[\d+\] cannot be read: /.exec(msg);
  return m ? m[0] + "<ERR>" : msg;
};
const norm = (v, ws) => JSON.parse(JSON.stringify(v).replaceAll(ws, "<CWD>").replaceAll(S, "{S}"));
const out = { receipt: [], gate: [] };
for (const which of ["receipt", "gate"]) {
  const ids = JSON.parse(raw)[which].map((c) => c.id);
  for (const [i, id] of ids.entries()) {
    const ws = fresh();
    const c = JSON.parse(raw.replaceAll("{S}", S).replaceAll("<CWD>", ws))[which][i];
    place(c, ws);
    let result;
    if (which === "receipt") {
      const r = parseSourceBoundReceipt(c.claim, ws, c.kind);
      result = "error" in r ? { error: mask(r.error, c.claim) } : { receipt: r };
    } else {
      const g = validateCheckReceipt({ phase: "C", checkEpoch: c.epoch }, c.session, c.claim === "" ? undefined : c.claim, ws);
      result = g.ok ? { ok: true } : { ok: false, reason: g.reason };
    }
    out[which].push(norm(result, ws));
  }
}
process.stdout.write(JSON.stringify(out, null, 1) + "\n");
