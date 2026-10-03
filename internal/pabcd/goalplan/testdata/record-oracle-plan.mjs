// Records what the CXC v0.2.40 oracle's goalplan slug validation, goalplanDir and plan revival answer (pabcd-state/src/goalplan.ts
// :292-332 and :468-678); the Go tests replay oracle-plan.json (no Node at test time). reviveGoalplan is private to the module,
// so each case writes its raw text as a goalplan.json and reads it back with readGoalplan, the way every caller reaches it; the
// recorded answer is the compact JSON of the revived plan, or null when readGoalplan refused it. A case's input is raw JSON text
// (a value JSON.stringify cannot write, such as 1e999, is spliced in as a marker). validateGoalplanSlug and goalplanDir are
// recorded directly. The goalplanDir answers hold the oracle's ".codexclaw", which the replay maps to ".crw". Recorded with Node
// v24.20.0 as
//   node record-oracle-plan.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> > oracle-plan.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d). Not recorded: a lone
// surrogate escape (a Go string cannot hold one; known-defects.md) and symlink cases (the Go tests build them in a temp tree).
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";

const [oracleDist, workRoot] = process.argv.slice(2);
const { readGoalplan, validateGoalplanSlug, goalplanDir } = await import(oracleDist + "/goalplan.js");

const T = "2026-01-01T00:00:00.000Z";
const raw = (token) => "@@" + token + "@@"; // spliced into the text as written, quotes dropped
const text = (v) => JSON.stringify(v).replace(/"@@(.*?)@@"/g, "$1");
const plan = (x = {}) => ({ objective: "o", slug: "rec-plan", createdAt: T, updatedAt: T, activeWorkPhaseId: null, workPhases: [], criteria: [], host: { armed: false, armedAt: null, source: "none" }, ...x });
const wp = (x = {}) => ({ id: "wp1", title: "t", status: "pending", tasks: [], criteriaIds: [], ...x });
const task = (x = {}) => ({ id: "t1", title: "t", status: "pending", ...x });
const crit = (x = {}) => ({ id: "c-1", scenario: "s", expectedEvidence: "e", capturedEvidence: null, status: "open", ...x });
const dec = (x = {}) => ({ id: "dec-1", question: "q?", status: "open", askedAt: T, ...x });
const step = (x = {}) => ({ idempotencyKey: "k", rationale: "r", evidence: "e", appliedAt: T, summary: "s", ...x });
const plans = {};
const add = (name, v, slug = "rec-plan") => { plans[name] = { raw: typeof v === "string" ? v : text(v), slug }; };
const each = (prefix, table, make) => { for (const [n, v] of Object.entries(table)) add(prefix + n, make(v)); };
const odd = { null: null, number: 5, string: "x", bool: true, object: {}, array: [] };

// The whole value: not an object, required text, the slug, the declared version, the two lists.
for (const [n, v] of Object.entries({ array: "[]", string: '"x"', null: "null", number: "5", true: "true", empty: "{}" })) add("top_" + n, v);
add("top_duplicate_key_last_wins", '{"objective":"first","objective":"second","slug":"rec-plan","workPhases":[],"criteria":[]}');
add("minimal", plan()); add("minimal_only_required", { objective: "o", slug: "rec-plan", workPhases: [], criteria: [] });
each("objective_", { missing: undefined, number: 5, null: null, empty: "" }, (v) => plan({ objective: v }));
each("slug_stored_", { missing: undefined, number: 5, empty: "", dot: ".", dotdot: "..", traversal: "../../escaped", slash: "a/b", dash_first: "-a", dot_first: ".a", space: "a b", multibyte: "é", newline: "a\n", other: "other" }, (v) => plan({ slug: v }));
add("slug_stored_128_bytes_ok", plan({ slug: "a".repeat(128) }), "a".repeat(128)); add("slug_stored_129_bytes", plan({ slug: "a".repeat(129) }), "a".repeat(128));
add("slug_mismatch_with_requested", plan({ slug: "rec-plan" }), "other-plan");
each("version_", { absent: undefined, v0: 0, v1: 1, v2: 2, v3: 3, v4: 4, v99: 99, fraction_3_5: 3.5, fraction_2_9: 2.9, negative: -7, minus_zero: raw("-0"), string: "2", null: null, bool: true, huge: raw("1e999"), huge_negative: raw("-1e999"), rounds_to_3: raw("3.0000000000000001"), underflow: raw("1e-999") }, (v) => plan({ schemaVersion: v }));
each("workPhases_", { missing: undefined, ...odd }, (v) => plan({ workPhases: v })); each("criteria_", { missing: undefined, ...odd }, (v) => plan({ criteria: v }));
each("createdAt_", { missing: undefined, number: 5, empty: "", null: null }, (v) => plan({ createdAt: v, updatedAt: v }));
each("activeWorkPhaseId_", { string: "wp1", empty: "", number: 5, null: null, missing: undefined }, (v) => plan({ activeWorkPhaseId: v }));
each("host_", { missing: undefined, null: null, string: "x", array: [], empty: {}, armed: { armed: true }, armed_string: { armed: "true" }, freeze: { armed: true, armedAt: T, source: "freeze" }, source_other: { source: "git" }, armedAt_number: { armedAt: 5 }, armedAt_empty: { armedAt: "" } }, (v) => plan({ host: v }));
add("extra_keys_dropped", plan({ evil: 1, nested: { a: [1] }, workPhases: [wp({ evil: 2, tasks: [task({ evil: 3 })] })], criteria: [crit({ evil: 4 })] }));
add("key_order_follows_revive", { steeringLog: [], finalGate: { status: "pending", qaRequired: false, updatedAt: T }, schemaVersion: 2, activeFinalGateRoundId: "r2", activePlanAuditRoundId: "r1", decisions: [], reviewRounds: [], host: {}, criteria: [], workPhases: [], activeWorkPhaseId: "x", updatedAt: T, createdAt: T, slug: "rec-plan", objective: "o" });
add("sub_records_pass_through", plan({ schemaVersion: 2, reviewRounds: [{ roundId: "r1", purpose: "plan_audit", planPath: "p", planSha256: "ab", status: "pending", lane: { launchId: "l1" }, openedAt: T }], finalGate: { status: "approved", qaRequired: true, updatedAt: T }, activePlanAuditRoundId: "r1", activeFinalGateRoundId: "r2" }));
each("active_round_ids_", { number: 5, null: null, empty: "" }, (v) => plan({ activePlanAuditRoundId: v, activeFinalGateRoundId: v }));

// Work phases.
each("phase_entry_", { ...odd, id_missing: wp({ id: undefined }), id_number: wp({ id: 5 }), title_missing: wp({ title: undefined }), title_number: wp({ title: 5 }), id_empty_kept: wp({ id: "", title: "" }) }, (v) => plan({ workPhases: [wp({ id: "ok" }), v] }));
each("phase_status_", { in_progress: "in_progress", done: "done", blocked: "blocked", superseded: "superseded", pending: "pending", other: "doing", case: "Done", number: 5, null: null, missing: undefined }, (v) => plan({ workPhases: [wp({ status: v })] }));
each("phase_tasks_", { missing: undefined, null: null, string: "x", object: {}, empty: [] }, (v) => plan({ workPhases: [wp({ tasks: v })] }));
each("phase_criteriaIds_", { missing: undefined, null: null, string: "x", empty: [], mixed: ["c-1", 5, null, "c-2", {}], }, (v) => plan({ workPhases: [wp({ criteriaIds: v })] }));
each("phase_reasons_", { blocked_text: { blockedReason: "why" }, blocked_empty: { blockedReason: "" }, blocked_number: { blockedReason: 5 }, blocked_null: { blockedReason: null }, superseded_text: { supersededBy: "wp2" }, superseded_empty: { supersededBy: "" }, both: { blockedReason: "b", supersededBy: "s", status: "blocked" } }, (v) => plan({ workPhases: [wp(v)] }));
for (const key of ["dependsOn", "awaitsDecision"]) {
  each("phase_" + key + "_", { absent: undefined, empty: [], ids: ["wp0", "wp1"], untrimmed_kept: [" wp0 "], dup_kept: ["a", "a"], null: null, string: "wp0", object: {}, number: 5, blank: [" "], empty_id: [""], tab_only: ["\t\n"], feff_only: ["\uFEFF"], nbsp_only: ["\u00A0"], nel_not_blank: ["\u0085"], non_string: [1], null_item: [null], mixed: ["ok", 5] }, (v) => plan({ workPhases: [wp({ [key]: v })] }));
}
each("task_dependsOn_", { absent: undefined, empty: [], ids: ["t0"], null: null, string: "t0", object: {}, blank: [" "], empty_id: [""], non_string: [1], null_item: [null] }, (v) => plan({ workPhases: [wp({ tasks: [task({ dependsOn: v })] })] }));
each("task_entry_", { null: null, number: 5, string: "x", array: [], empty_object: {}, id_missing: task({ id: undefined }), id_number: task({ id: 5 }), title_missing: task({ title: undefined }), title_number: task({ title: 5 }), id_empty_kept: task({ id: "", title: "" }), skipped_with_bad_dependsOn: task({ id: undefined, dependsOn: "x" }), bad_dependsOn_nulls_plan: task({ id: "t9", dependsOn: "x" }) }, (v) => plan({ workPhases: [wp({ tasks: [task({ id: "ok" }), v] })] }));
each("task_status_", { done: "done", pending: "pending", other: "in_progress", case: "DONE", number: 5, null: null, missing: undefined }, (v) => plan({ workPhases: [wp({ tasks: [task({ status: v })] })] }));
each("task_outcome_", { text: "node --test: 0 fail", padded: "  node --test: 0 fail  ", blank: "   ", empty: "", number: 42, null: null, object: {}, missing: undefined, feff_padded: "\uFEFFok\uFEFF", nbsp_padded: "\u00A0ok\u00A0", ideographic_padded: "\u3000ok\u3000", line_sep_padded: "\u2028ok\u2029", nel_kept: "\u0085ok\u0085", lrm_kept: "\u200Eok\u200E", zwsp_kept: "\u200Bok", inner_kept: " a  b ", only_feff: "\uFEFF", only_nel: "\u0085" }, (v) => plan({ workPhases: [wp({ tasks: [task({ status: "done", outcome: v })] })] }));
add("tasks_order_and_duplicate_ids_kept", plan({ workPhases: [wp({ tasks: [task({ id: "b" }), task({ id: "a" }), task({ id: "b", title: "again" })] }), wp({ id: "wp1", title: "dup phase id" })] }));
add("legacy_plan_without_optional_fields", { objective: "o", slug: "rec-plan", createdAt: T, updatedAt: T, activeWorkPhaseId: "wp1", workPhases: [wp({ status: "done", tasks: [task({ status: "done" })] })], criteria: [crit()], host: { armed: false, armedAt: null, source: "none" } });

// Criteria.
each("criterion_entry_", { ...odd, id_missing: crit({ id: undefined }), id_number: crit({ id: 5 }), scenario_missing: crit({ scenario: undefined }), scenario_number: crit({ scenario: 5 }), empty_text_kept: crit({ id: "", scenario: "" }) }, (v) => plan({ criteria: [crit(), v] }));
each("criterion_expectedEvidence_", { text: "e", empty: "", number: 5, null: null, missing: undefined }, (v) => plan({ criteria: [crit({ expectedEvidence: v })] }));
each("criterion_capturedEvidence_", { text: "proof", empty: "", number: 5, null: null, missing: undefined }, (v) => plan({ criteria: [crit({ capturedEvidence: v })] }));
each("criterion_status_", { met: "met", open: "open", other: "done", case: "Met", number: 5, null: null, missing: undefined }, (v) => plan({ criteria: [crit({ status: v })] }));
each("criterion_surface_", { logic: "logic", web: "web", tui: "tui", desktop: "desktop", other: "api", case: "Web", number: 5, null: null, missing: undefined }, (v) => plan({ criteria: [crit({ surface: v })] }));
each("criterion_presented_", { native: "native", other: "web", case: "Native", number: 5, null: null, with_desktop: "native" }, (v) => plan({ criteria: [crit({ surface: "desktop", presented: v })] }));

// Decisions.
each("decisions_", { missing: undefined, null: null, string: "x", object: {}, number: 5, empty: [], two: [dec(), dec({ id: "dec-2", status: "decided", answer: "yes", decidedAt: T })] }, (v) => plan({ decisions: v }));
each("decision_entry_", { null: null, number: 5, string: "x", array: [], empty_object: {} }, (v) => plan({ decisions: [dec(), v] }));
each("decision_id_", { upper: "Dec-1", underscore: "dec_1", dash_first: "-a", digit_first: "1a", empty: "", number: 5, null: null, missing: undefined, "40_chars": "a".repeat(40), "41_chars": "a".repeat(41), unicode: "dé", space: "a b", newline: "a\n", hyphen_inside: "a-b-c" }, (v) => plan({ decisions: [dec({ id: v })] }));
each("decision_question_", { text: "q?", blank: "  ", empty: "", number: 5, null: null, missing: undefined, feff_only: "\uFEFF", nel_only: "\u0085", padded_kept: " q " }, (v) => plan({ decisions: [dec({ question: v })] }));
each("decision_askedAt_", { ok: T, no_millis: "2026-01-01T00:00:00Z", date_only: "2026-01-01", offset: "2026-01-01T00:00:00.000+00:00", lower_z: "2026-01-01T00:00:00.000z", lower_t: "2026-01-01t00:00:00.000Z", extra_digit: "2026-01-01T00:00:00.0000Z", two_digit_ms: "2026-01-01T00:00:00.00Z", short_month: "2026-1-01T00:00:00.000Z", space_prefix: " 2026-01-01T00:00:00.000Z", space_sep: "2026-01-01 00:00:00.000Z", number: 5, null: null, missing: undefined, empty: "", feb_30: "2026-02-30T00:00:00.000Z", feb_29_leap: "2024-02-29T00:00:00.000Z", feb_29_common: "2023-02-29T00:00:00.000Z", feb_29_1900: "1900-02-29T00:00:00.000Z", feb_29_2000: "2000-02-29T00:00:00.000Z", apr_31: "2026-04-31T00:00:00.000Z", month_13: "2026-13-01T00:00:00.000Z", month_00: "2026-00-01T00:00:00.000Z", day_00: "2026-01-00T00:00:00.000Z", hour_24: "2026-01-01T24:00:00.000Z", minute_60: "2026-01-01T00:60:00.000Z", second_60: "2026-01-01T00:00:60.000Z", last_instant: "9999-12-31T23:59:59.999Z", year_0: "0000-01-01T00:00:00.000Z", year_0_dec_31: "0000-12-31T23:59:59.999Z", plus_10000: "+010000-01-01T00:00:00.000Z", minus_1: "-000001-01-01T00:00:00.000Z", minus_0: "-000000-01-01T00:00:00.000Z", plus_2026: "+002026-01-01T00:00:00.000Z", minus_2026: "-002026-01-01T00:00:00.000Z", max_time: "+275760-09-13T00:00:00.000Z", past_max: "+275760-09-13T00:00:00.001Z", min_time: "-271821-04-20T00:00:00.000Z", before_min: "-271821-04-19T23:59:59.999Z", ext_year_400_not_canonical: "+000400-02-29T00:00:00.000Z", ext_leap: "+012000-02-29T00:00:00.000Z", ext_century_common: "+010100-02-29T00:00:00.000Z", ext_neg_leap: "-000004-02-29T00:00:00.000Z", ext_neg_common: "-000001-02-29T00:00:00.000Z", plus_7_digits: "+0100000-01-01T00:00:00.000Z", fullwidth: "２０２６-01-01T00:00:00.000Z", plus_in_ms: "2026-01-01T00:00:00.+00Z", newline_end: "2026-01-01T00:00:00.000Z\n", z_missing: "2026-01-01T00:00:00.000" }, (v) => plan({ decisions: [dec({ askedAt: v })] }));
each("decision_recommendation_", { absent: undefined, text: "r", empty: "", blank: "  ", null: null, number: 5, padded: " r ", feff_only: "\uFEFF" }, (v) => plan({ decisions: [dec({ recommendation: v })] }));
each("decision_options_", { absent: undefined, null: null, empty: [], string: "a", object: {}, two: ["a", "b"], one: ["a"], blank: ["a", " "], empty_item: ["a", ""], non_string: ["a", 5], null_item: ["a", null], dup: ["a", "a"], dup_after_trim: ["a", " a "], dup_after_feff_trim: ["a", "\uFEFFa"], distinct_nel: ["a", "a\u0085"], distinct_lrm: ["a", "a\u200E"], distinct_case: ["a", "A"], untrimmed_kept: [" a ", "b"], nel_only: ["\u0085"], feff_only: ["\uFEFF"] }, (v) => plan({ decisions: [dec({ options: v })] }));
each("decision_rec_in_options_", { member: { options: ["a", "b"], recommendation: "b" }, not_member: { options: ["a", "b"], recommendation: "c" }, trimmed_member: { options: ["a", "b"], recommendation: " b " }, option_padded: { options: [" a ", "b"], recommendation: "a" }, feff_trim: { options: ["a"], recommendation: "\uFEFFa" }, nel_not_trimmed: { options: ["a"], recommendation: "a\u0085" }, case: { options: ["a"], recommendation: "A" }, options_without_rec: { options: ["a"] }, rec_without_options: { recommendation: "zzz" }, empty_options_with_rec: { options: [], recommendation: "a" } }, (v) => plan({ decisions: [dec(v)] }));
each("decision_open_", { with_answer: { answer: "x" }, with_empty_answer: { answer: "" }, with_null_answer: { answer: null }, with_decidedAt: { decidedAt: T }, with_null_decidedAt: { decidedAt: null }, with_both: { answer: "x", decidedAt: T }, extra_keys_dropped: { evil: 1 } }, (v) => plan({ decisions: [dec(v)] }));
each("decision_decided_", { ok: { status: "decided", answer: "yes", decidedAt: T }, answer_padded_kept: { status: "decided", answer: " yes ", decidedAt: T }, answer_blank: { status: "decided", answer: " ", decidedAt: T }, answer_empty: { status: "decided", answer: "", decidedAt: T }, answer_number: { status: "decided", answer: 5, decidedAt: T }, answer_missing: { status: "decided", decidedAt: T }, decidedAt_missing: { status: "decided", answer: "y" }, decidedAt_invalid: { status: "decided", answer: "y", decidedAt: "2026-02-30T00:00:00.000Z" }, decidedAt_null: { status: "decided", answer: "y", decidedAt: null }, with_all: { status: "decided", answer: "y", decidedAt: T, recommendation: "a", options: ["a", "b"] }, evil_dropped: { status: "decided", answer: "y", decidedAt: T, evil: 1 } }, (v) => plan({ decisions: [dec(v)] }));
each("decision_status_", { other: "answered", case: "Open", number: 5, null: null, missing: undefined }, (v) => plan({ decisions: [dec({ status: v })] }));
add("decisions_order_and_duplicate_ids_kept", plan({ decisions: [dec({ id: "b" }), dec({ id: "a" }), dec({ id: "b", question: "again" })] }));

// steeringLog.
each("steering_", { missing: undefined, null: null, string: "x", object: {}, number: 5, empty: [], one: [step()], two: [step(), step({ idempotencyKey: "k2" })], extra_keys_dropped: [step({ evil: 1 })], whitespace_values_kept: [step({ rationale: " " })] }, (v) => plan({ steeringLog: v }));
each("steering_entry_", { null: null, number: 5, string: "x", array: [] }, (v) => plan({ steeringLog: [step(), v] }));
for (const key of ["idempotencyKey", "rationale", "evidence", "appliedAt", "summary"]) each("steering_" + key + "_", { missing: undefined, empty: "", number: 5, null: null }, (v) => plan({ steeringLog: [step({ [key]: v })] }));

// validateGoalplanSlug and goalplanDir, recorded directly.
const slugs = {}, dirs = {};
const slug = (name, input) => { let oracle; try { oracle = "ok:" + validateGoalplanSlug(input); } catch (e) { oracle = "err:" + e.message; } slugs[name] = { input, oracle }; };
for (const [n, v] of Object.entries({ simple: "a", mixed: "Abc-1.2_x", dotted: "a.b", digit_first: "9", trailing_dot: "a.", trailing_dash: "a-", "128_bytes": "a".repeat(128), "129_bytes": "a".repeat(129), empty: "", dot: ".", dotdot: "..", triple_dot: "...", dot_first: ".hidden", dash_first: "-x", underscore_first: "_x", slash: "a/b", backslash: "a\\b", traversal: "../../escaped", absolute: "/etc/passwd", space: "a b", tab: "a\tb", newline: "a\n", leading_newline: "\na", nul: "a\u0000b", colon: "a:b", unicode: "é", unicode_128_chars: "é".repeat(128), fullwidth: "ａ", emoji: "a😀", quote: 'a"b', backslash_quote: 'a\\"b', angle: "a<b", ampersand: "a&b", line_separator: "a\u2028b", backspace: "a\bb", formfeed: "a\fb", control: "a\u001fb", del: "a\u007fb", long_unicode: "é".repeat(65) })) slug(n, v);
const dir = (name, cwd, s) => { let oracle; try { oracle = "ok:" + goalplanDir(cwd, s); } catch (e) { oracle = "err:" + e.message; } dirs[name] = { cwd, slug: s, oracle }; };
dir("plain", "/work/proj", "plan-1"); dir("trailing_slash", "/work/proj/", "plan-1"); dir("dot_segments", "/work/../work/proj//a/..", "plan-1"); dir("root", "/", "x"); dir("invalid_slug", "/work/proj", "../x"); dir("empty_slug", "/work/proj", "");

const out = { plans: {}, slugs, dirs };
for (const [name, c] of Object.entries(plans)) {
  const root = mkdtempSync(join(workRoot, "g-"));
  const d = join(root, ".codexclaw", "goalplans", c.slug);
  mkdirSync(d, { recursive: true });
  writeFileSync(join(d, "goalplan.json"), c.raw);
  const read = readGoalplan(root, c.slug);
  out.plans[name] = { raw: c.raw, slug: c.slug, oracle: read === null ? null : JSON.stringify(read) };
  rmSync(root, { recursive: true, force: true });
}
const lines = (o) => "{\n" + Object.keys(o).map((n, i, a) => "  " + JSON.stringify(n) + ": " + JSON.stringify(o[n]) + (i < a.length - 1 ? "," : "")).join("\n") + "\n}";
process.stdout.write('{\n"plans": ' + lines(out.plans) + ',\n"slugs": ' + lines(slugs) + ',\n"dirs": ' + lines(dirs) + "\n}\n"); // one case per line

