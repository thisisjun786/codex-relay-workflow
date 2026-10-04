// Records the answers of the CXC v0.2.40 oracle's metric-cli.ts for the Go port's replay. The Go test reads
// oracle.json; Node is used only while recording. Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <this directory>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d) with its dist/ built.
// The B-class cases of test/metrics.test.ts:128-146 and test/help-verbs.test.ts:112-118 are covered by the
// "bclass-" ids; the rest lock the quirks of the ported parser and renderer.
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, relative, sep } from "node:path";

const [dist, out] = process.argv.slice(2);
const oracle = await import(dist + "/metric-cli.js");
const TS = /\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z/g;
const cases = [];
const add = (id, argv, stdin = "", given = null) => cases.push({ id, argv, stdin, ...(given ? { given } : {}) });
const ROW = (session, name, value, baseline, best, phase, source) =>
  JSON.stringify({ ts: "2026-01-01T00:00:00.000Z", sessionId: session, workPhaseId: phase, metricName: name, value, baseline, best, source });
const LEDGER = (rows) => ({ ".codexclaw/metrics.jsonl": rows.join("\n") + "\n" });

// --- metrics.test.ts:128-146 (runMetricCli: record/show/kind/ingest) ---
add("bclass-record", ["record", "--session", "cli", "--name", "score", "--value", "4", "--json"]);
add("bclass-show", ["show", "--session", "cli", "--json"], "", LEDGER([ROW("cli", "score", 4, 4, 4, "default", "operator-entered")]));
add("bclass-kind", ["kind", "--session", "cli", "satisfy", "--json"]);
add("bclass-ingest", ["ingest", "--session", "cli"], "METRIC score=5\n");

// --- help-verbs.test.ts:112-118 (metric help/--help/-h print usage without demanding --session) ---
for (const token of ["help", "--help", "-h"]) add("help-" + token.replace(/^-+/, "dash"), [token]);
add("help-empty-argv", []);
add("help-wins-over-rest", ["help", "--session", "x", "--nope"]);
add("help-case-is-a-verb", ["HELP"]);
for (const verb of ["record", "ingest", "show", "kind", "parse-line"]) add("sub-" + verb + "-dashdash-help", [verb, "--help"]);

// --- session flags ---
add("session-required", ["record"]);
add("session-last", ["show", "--session"]);
add("session-empty", ["show", "--session", ""]);
add("session-short", ["show", "-s", "s"]);
add("session-long-last-falls-back-to-short", ["show", "-s", "s", "--session"]);
add("session-equals-form-not-read", ["show", "--session=s"]);
add("session-takes-the-flag-as-its-value", ["show", "--session", "--json"]);
add("session-empty-beats-short", ["show", "--session", "", "-s", "valid"]);
add("unknown-verb", ["bogus"]);
add("verb-is-case-sensitive", ["Record", "--session", "s"]);

// --- record ---
add("record-name-required", ["record", "--session", "s", "--value", "4"]);
add("record-name-empty", ["record", "--session", "s", "--name", "", "--value", "4"]);
add("record-name-last", ["record", "--session", "s", "--name"]);
add("record-name-takes-the-flag", ["record", "--session", "s", "--name", "--json", "--value", "4"]);
add("record-source-invalid", ["record", "--session", "s", "--name", "n", "--value", "1", "--source", "other"]);
add("record-source-evaluate", ["record", "--session", "s", "--name", "n", "--value", "1", "--source", "evaluate.sh"]);
add("record-source-last-defaults", ["record", "--session", "s", "--name", "n", "--value", "1", "--source"]);
add("record-source-beats-bad-value", ["record", "--session", "s", "--name", "n", "--value", "abc", "--source", "other"]);
add("record-name-beats-bad-value", ["record", "--session", "s", "--value", "abc"]);
add("record-work-phase", ["record", "--session", "s", "--name", "n", "--value", "1", "--work-phase", "p1", "--json"]);
add("record-work-phase-empty", ["record", "--session", "s", "--name", "n", "--value", "1", "--work-phase", "", "--json"]);
add("record-work-phase-last-defaults", ["record", "--session", "s", "--name", "n", "--value", "1", "--work-phase"]);
add("record-text-line", ["record", "--session", "s", "--name", "n", "--value", "12.5"]);
add("record-carries-baseline-and-best", ["record", "--session", "s", "--name", "score", "--value", "9", "--json"],
  "", LEDGER([ROW("s", "score", 10, 10, 12, "p1", "operator-entered"), ROW("other", "score", 99, 99, 99, "p1", "operator-entered")]));
add("record-short-session-flag", ["record", "-s", "s", "--name", "n", "--value", "1"]);
for (const [i, value] of ["4", "+5", "-0", " 7 ", "\uFEFF3", "1.", ".5", "1.e5", "1e-999", "0x10", "0X1F", "0o17", "0b101",
  "0x10000000000000000", "1e21", "1e-7", " ", "\n"].entries())
  add("value-accepted-" + i, ["record", "--session", "s", "--name", "n", "--value", value]);
for (const [i, value] of ["", "abc", "1_000", "0x", "0b2", "-0x1", "+0x1", "1e", ".", "+", "Inf", "Infinity", "-Infinity",
  "NaN", "1e999"].entries())
  add("value-refused-" + i, ["record", "--session", "s", "--name", "n", "--value", value]);
add("value-flag-last", ["record", "--session", "s", "--name", "n", "--value"]);
add("value-accepted-big-rounds-to-even", ["record", "--session", "s", "--name", "n", "--value", "0x1fffffffffffff9"]);
add("value-accepted-long-binary", ["record", "--session", "s", "--name", "n", "--value", "0b" + "1".repeat(70)]);
add("value-accepted-long-octal", ["record", "--session", "s", "--name", "n", "--value", "0o" + "7".repeat(24)]);
add("value-refused-prefixed-overflow", ["record", "--session", "s", "--name", "n", "--value", "0x" + "f".repeat(260)]);

// --- show ---
add("show-no-rows", ["show", "--session", "s"]);
add("show-rows-text", ["show", "--session", "s"], "",
  LEDGER([ROW("s", "score", 12.5, 10, 12.5, "p1", "operator-entered"), ROW("s", "win-rate", 0.75, 0.75, 0.75, "", "evaluate.sh")]));
add("show-rows-json-escaping", ["show", "--session", "x<&\"z", "--json"], "",
  LEDGER([ROW("x<&\"z", "score", 1, 1, 1, "p\t1", "operator-entered")]));
add("show-empty-session-reads-every-session", ["show", "--session", ""], "", LEDGER([ROW("s", "score", 1, 1, 1, "", "operator-entered")]));

// --- ingest ---
add("ingest-empty-stdin", ["ingest", "--session", "s"]);
add("ingest-mixed-lines", ["ingest", "--session", "s", "--json"], "hello\nMETRIC score=2\nnoise\nMETRIC win-rate=0.75\n");
add("ingest-crlf-no-match", ["ingest", "--session", "s"], "METRIC score=2\r\n");
add("ingest-source-operator", ["ingest", "--session", "s", "--source", "operator-entered"]);
add("ingest-source-invalid", ["ingest", "--session", "s", "--source", "other"]);
add("ingest-work-phase", ["ingest", "--session", "s", "--work-phase", "p1", "--json"], "METRIC score=1\n");
add("ingest-name-normalised-twice", ["ingest", "--session", "s", "--json"], "METRIC .-a=1\n");

// --- kind ---
add("kind-default-satisfy", ["kind", "--session", "s"]);
add("kind-infers-maximize", ["kind", "--session", "s"], "", LEDGER([ROW("s", "score", 1, 1, 1, "", "operator-entered")]));
add("kind-sets-satisfy", ["kind", "--session", "s", "satisfy"]);
add("kind-sets-via-flag-shaped-positional", ["kind", "--session", "s", "--set", "maximize", "--json"]);
add("kind-ignores-invalid-positional", ["kind", "--session", "s", "--set", "bogus", "--json"]);
add("kind-positional-after-json", ["kind", "--json", "--session", "s", "maximize"]);
add("kind-skips-flag-values", ["kind", "--session", "s", "--work-phase", "satisfy", "--json"]);
add("kind-skips-dash-tokens", ["kind", "--session", "s", "-"]);
add("kind-reads-seeded-file", ["kind", "--session", "s"], "",
  ({ ".codexclaw/objective-kind/s.json": JSON.stringify({ sessionId: "s", kind: "maximize", updatedAt: "2026-01-01T00:00:00.000Z" }) }));
add("kind-json-explicit-null", ["kind", "--session", "s", "--json"]);

// --- parse-line ---
add("parse-line-stdin-form", ["parse-line", "--session", "rec-s1"], "METRIC latency_ms=110\n");
add("parse-line-argv-form", ["parse-line", "METRIC", "latency_ms=110", "--session", "rec-s1"]);
add("parse-line-session-guard-first", ["parse-line", "METRIC", "x=1"]);
add("parse-line-succeeds-when-the-session-value-completes-the-line", ["parse-line", "METRIC", "--session", "=1"]);
add("parse-line-succeeds-via-short-flag", ["parse-line", "METRIC", "-s", "=1"]);

const root = mkdtempSync(join(process.env.TMPDIR || "/tmp", "cxc-metric-rec-"));
const escape = (m) => "\\u" + m.charCodeAt(0).toString(16).padStart(4, "0");
const json = (v) => JSON.stringify(v).replace(/[\u007f-\uffff]/g, escape);
function walk(dir, base, into) {
  for (const name of readdirSync(dir).sort()) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) walk(path, base, into);
    else into[relative(base, path).split(sep).join("/")] = readFileSync(path, "utf8").replace(TS, "<TS>");
  }
  return into;
}
let failures = 0;
for (const c of cases) {
  const cwd = mkdtempSync(join(root, "case-"));
  try {
    for (const [rel, body] of Object.entries(c.given ?? {})) {
      mkdirSync(dirname(join(cwd, rel)), { recursive: true });
      writeFileSync(join(cwd, rel), body);
    }
    const result = oracle.runMetricCli(c.argv, cwd, c.stdin);
    c.want = { code: result.code, output: result.output.replace(TS, "<TS>") };
    c.files = walk(cwd, cwd, {});
    if (c.want.output.replace(TS, "").includes(cwd)) failures++;
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
}
rmSync(root, { recursive: true, force: true });
if (failures) throw new Error("a recorded answer leaked its case path");
mkdirSync(out, { recursive: true });
writeFileSync(join(out, "oracle.json"), '{\n  "oracle": "CXC v0.2.40 3c1459ac",\n  "cases": [\n' +
  cases.map((c) => "    " + json(c)).join(",\n") + "\n  ]\n}\n");
console.log("recorded " + cases.length + " metric CLI answers");
