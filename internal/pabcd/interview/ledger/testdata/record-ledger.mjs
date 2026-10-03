// Records what the CXC v0.2.40 oracle's interview-ledger.ts does, for the Go tests to replay (no Node at test time): parseQuestions and
// parseAnswers over payload shapes, captureInterviewAnswers over rounds (the ledger text it leaves), and readQaEvents and
// dimensionsBackedByAnswers over hand-written ledgers. Date is frozen, so every ts is 2026-01-01T00:00:00.000Z.
// Recorded with Node v24.20.0 as
//   node record-ledger.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> > oracle-ledger.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdirSync, writeFileSync, readFileSync, readdirSync, existsSync } from "node:fs";
import { join } from "node:path";

const FROZEN = Date.parse("2026-01-01T00:00:00.000Z");
const RealDate = Date;
globalThis.Date = class extends RealDate {
  constructor(...a) { super(...(a.length ? a : [FROZEN])); }
  static now() { return FROZEN; }
};
const [dist, workRoot] = process.argv.slice(2);
const L = await import(dist + "/interview-ledger.js");
const { sanitizeKey } = await import(dist + "/state.js");

const J = (s) => JSON.parse(s); // payloads are written as JSON text so a "__proto__" key is an own key, as the hook delivers it
const body = { answers: { q_scope: { answers: ["adaptive 1-N"] }, q_chat: { answers: ["remove it"] } } };
const qs = { questions: [{ id: "q_scope", header: "Scope", question: "How wide?" }, { id: "q_chat", question: "Keep chat?" }] };
const str = JSON.stringify, twice = (v) => str(str(v));
const hazard = 'q"uote back\\slash \n\r\t\b\f \u0001\u001f \u007f \u2028 \u2029 </script> & é 😀 한국어 \\u2028';
const out = [];
const rec = (id, body) => out.push({ id, ...body });

const answerShapes = {
  plain: body, string: str(body), double: twice(body), triple: str(twice(body)), array_one: [body],
  blocks: [{ type: "input_text", text: str(body) }], output_string: { output: str(body) }, output_object: { output: body },
  text_object: { text: body }, text_string: { text: str(body) }, content_string: { content: str(body) }, result_string: { result: str(body) },
  result_object: { result: body }, output_blocks: { output: [{ type: "input_text", text: str(body) }] },
  part_output_key: [{ output: str(body) }], part_content_key: [{ content: str(body) }], string_part: ["not json", str(body)],
  leading_non_records: [1, null, [2], "x", { type: "a", text: str(body) }], ninth_part: [1, 2, 3, 4, 5, 6, 7, 8, body],
  eighth_part: [1, 2, 3, 4, 5, 6, 7, body], first_record_without_answers: [{ foo: 1 }, body], text_not_json: [{ text: "hello" }, body],
  text_json_non_record: [{ text: "5" }, body], text_double_encoded: [{ text: twice(body) }], part_with_answers_wins: [{ text: str({ x: 1 }), answers: body.answers }], text_decodes_to_other_record: [{ text: str({ x: 1 }) }, body],
  first_part_empty_object: [{}, body], part_text_object: [{ text: body }], text_before_output: [{ text: str({ x: 1 }), output: str(body) }],
  output_nested_twice: { output: { output: str(body) } }, top_level_empty_answers_wins: { answers: {}, output: str(body) },
  answers_array: { answers: [1, 2] }, answers_string: { answers: "x" }, answers_null: { answers: null },
  values: J('{"answers":{"a":{"answers":["x",1,null,"y"]},"b":{"answers":"x"},"c":"x","d":{},"e":{"answers":[]},"f":["x"],"g":null}}'),
  reserved_ids: J('{"answers":{"__proto__":{"answers":["pwn"]},"constructor":{"answers":["c"]},"prototype":{"answers":["p"]},"q_real":{"answers":["ok"]}}}'),
  numeric_ids: J('{"answers":{"1":{"answers":["a"]},"01":{"answers":["b"]},"b":{"answers":["c"]}}}'),
  null: null, number: 42, true: true, prose: "plain prose, not json", empty_array: [], null_part: [null], empty_object: {}, open_brace: "{",
  empty_string: "", json_number: "5", json_string: '"str"', json_array: "[1,2]", json_null: "null", huge_number: "1e999",
  trailing_data: str(body) + " x", surrounding_space: "  " + str(body) + "\n", bom_prefix: "\uFEFF" + str(body), nbsp_prefix: "\u00a0" + str(body),
  duplicate_key: '{"answers":{"a":{"answers":["x"]}},"answers":{"b":{"answers":["y"]}}}',
};
for (const [name, v] of Object.entries(answerShapes)) rec("parse_answers_" + name, { kind: "parse", toolResponse: v, answers: L.parseAnswers(v) });

const questionShapes = {
  plain: qs, string: str(qs), double: twice(qs), array_wrapped: [qs], blocks: [{ type: "input_text", text: str(qs) }], not_array: { questions: { id: "a" } },
  string_questions: { questions: "x" }, output_wrapper_not_unwrapped: { output: str(qs) }, empty: {}, null: null, open_brace: "{",
  mixed: J('{"questions":[{"id":"a","question":"A?"},{"id":"b","header":"H"},{"id":"c","question":"","header":"H3"},{"id":"","question":"no id"},{"id":5,"question":"num id"},{"question":"missing id"},"str",null,[1],{"id":"d","question":7,"header":"H4"},{"id":"e","question":7,"header":8},{"id":"f"},{"id":"a","question":"again"}]}'),
  unicode: { questions: [{ id: "k", question: "한국어? 😀" }] },
};
for (const [name, v] of Object.entries(questionShapes)) rec("parse_questions_" + name, { kind: "parse", toolInput: v, questions: L.parseQuestions(v) });

const round = (qsIn, ans) => ({ toolInput: qsIn, toolResponse: ans });
const full = round(qs, body);
let n = 0;
const capture = (id, { sessionId = "s", turnId = "t1", rounds = [full], preLedger, turns } = {}) => {
  const cwd = join(workRoot, "c" + n++);
  const dir = join(cwd, ".codexclaw", "interviews");
  mkdirSync(cwd, { recursive: true });
  if (preLedger !== undefined) { mkdirSync(dir, { recursive: true }); writeFileSync(join(dir, sanitizeKey(sessionId) + ".jsonl"), preLedger); }
  const written = rounds.map((r, i) => L.captureInterviewAnswers({ cwd, sessionId, turnId: turns ? turns[i] : turnId, ...r }).written.map((e) => e.eventId));
  const file = existsSync(dir) ? readdirSync(dir).sort() : null;
  const text = file && file.length ? readFileSync(join(dir, file[0]), "utf8") : null;
  rec("capture_" + id, { kind: "capture", sessionId, turnId, turns, rounds, preLedger, written, files: file, ledger: text });
};
capture("basic");
capture("no_answers", { rounds: [round(qs, { answers: {} })] });
capture("one_answered", { rounds: [round({ questions: [{ id: "q1", question: "unanswered?" }] }, { answers: {} })] });
capture("no_turn", { turnId: "" });
capture("empty_session", { sessionId: "" });
capture("sanitised_session", { sessionId: "a/b c" });
capture("idempotent_then_new_turn", { rounds: [full, full, full], turns: ["t1", "t1", "t2"] });
capture("string_round", { rounds: [round(str(qs), str(body))] });
capture("empty_answers_array", { rounds: [round({ questions: [{ id: "q1", question: "?" }] }, { answers: { q1: { answers: [] } } })] });
capture("escapes", { rounds: [round({ questions: [{ id: "q1", question: hazard }] }, { answers: { q1: { answers: [hazard, "", " "] } } })] });
capture("reserved_question_id", { rounds: [round(J('{"questions":[{"id":"__proto__","question":"p"},{"id":"constructor","question":"c"}]}'), answerShapes.reserved_ids)] });
capture("numeric_ids", { rounds: [round({ questions: [{ id: "1", question: "one" }, { id: "01", question: "zero one" }] }, answerShapes.numeric_ids)] });
capture("header_only", { rounds: [round({ questions: [{ id: "q1", header: "Only header" }, { id: "q2", question: "", header: "H" }] }, { answers: {} })] });
const scan = str({ ts: "t", sessionId: "s", event: "scan_completed", roundId: 1, contradictionCount: 0, highContradictionCount: 0, map: { q_scope: "goal" } });
const already = str({ ts: "t", sessionId: "s", turnId: "t1", event: "question_asked", questionId: "q_scope", eventId: "t1:q_scope:question_asked", question: "How wide?" });
capture("pre_existing_rows", { preLedger: [scan, "garbage", "", already].join("\n") + "\n" });
capture("unterminated_valid_row", { preLedger: already });
capture("event_id_collision", { rounds: [round({ questions: [{ id: "r", question: "first" }] }, { answers: { r: { answers: ["a"] } } }), round({ questions: [{ id: "q:r", question: "second" }] }, { answers: { "q:r": { answers: ["b"] } } })], turns: ["t:q", "t"] });
capture("unterminated_tail", { preLedger: scan + '\n{"partial":' });
capture("crlf_ledger", { preLedger: already + "\r\n" });
capture("bom_ledger", { preLedger: "\uFEFF" + already + "\n" });
capture("nbsp_padded_ledger", { preLedger: "\u00a0" + already + "\u00a0\n" });

const row = (o) => str(o) + "\n";
const ask = (q) => row({ event: "question_asked", questionId: q, eventId: "t:" + q + ":question_asked", question: "?" });
const ans = (q, a) => row({ event: "answer_recorded", questionId: q, eventId: "t:" + q + ":answer_recorded", answers: a });
const map = (m, event = "scan_completed") => row({ event, roundId: 1, contradictionCount: 0, map: m });
const reads = {
  mixed: ["", "   \n", "{bad\n", "[]\n", "5\n", '"s"\n', "null\n", ask("q1"), row({ event: "answer_recorded", eventId: 5 }), map({ q1: "goal" }), row({ event: "question_asked" }),
    row({ event: "other", eventId: "x" }), ask("q2").replace("\n", "\r\n"), "\uFEFF" + ask("q3"), "\u00a0" + ask("q4").trimEnd() + "\u00a0\n", row({ event: "answer_recorded", eventId: "e", answers: "x" })].join(""),
  basic: ask("q1") + ask("q2") + ask("q3") + ask("q4") + ans("q1", ["x"]) + ans("q2", []) + ans("q3", ["  "]) + ans("q4", [1, null, " a "]) + map({ q1: "goal", q2: "constraint", q3: "success", q4: "ontology" }),
  later_bad_values_keep_earlier: ask("q1") + ans("q1", ["x"]) + map({ q1: "goal" }) + map({ q1: "" }) + map({ q1: 7 }),
  whitespace_dimension_is_valid: ask("q1") + ans("q1", ["x"]) + map({ q1: " " }),
  rows_without_event_ids: row({ event: "question_asked", questionId: "q1" }) + row({ event: "answer_recorded", questionId: "q1", answers: ["x"] }) + map({ q1: "goal" }),
  last_attribution_wins: ask("q1") + ans("q1", ["x"]) + map({ q1: "goal" }) + map({ q1: "success" }),
  attribution_ignores_bad_values: ask("q1") + ask("q2") + ask("q3") + ans("q1", ["x"]) + ans("q2", ["x"]) + ans("q3", ["x"]) + map({ q1: "", q2: 5, q3: "goal" }),
  map_not_a_record: ask("q1") + ans("q1", ["x"]) + row({ event: "scan_completed", map: ["q1", "goal"] }),
  rescan_rows_are_not_scan_completed: ask("q1") + ans("q1", ["x"]) + map({ q1: "goal" }, "rescan_completed") + map({ q1: "success" }, "scan_started"),
  answer_before_question: ans("q1", ["x"]) + ask("q1") + map({ q1: "goal" }),
  answers_not_array: ask("q1") + row({ event: "answer_recorded", questionId: "q1", answers: "yes" }) + map({ q1: "goal" }),
  unasked_question: ans("q1", ["x"]) + map({ q1: "goal" }),
  same_dimension_twice: ask("q1") + ask("q2") + ans("q2", ["x"]) + map({ q1: "goal", q2: "goal" }),
  odd_dimension_names: ask("q1") + ans("q1", ["x"]) + map({ q1: "__proto__" }),
  no_rows: "",
};
for (const [name, text] of Object.entries(reads)) {
  const cwd = join(workRoot, "c" + n++), dir = join(cwd, ".codexclaw", "interviews");
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "s.jsonl"), text);
  rec("read_" + name, { kind: "read", ledger: text, eventIds: L.readQaEvents(cwd, "s").map((e) => e.eventId), backed: [...L.dimensionsBackedByAnswers(cwd, "s")].sort() });
}
rec("read_missing_file", { kind: "read", ledger: null, eventIds: L.readQaEvents(join(workRoot, "none"), "s").map((e) => e.eventId), backed: [...L.dimensionsBackedByAnswers(join(workRoot, "none"), "s")] });
console.log("[\n" + out.map((c) => JSON.stringify(c)).join(",\n") + "\n]");
